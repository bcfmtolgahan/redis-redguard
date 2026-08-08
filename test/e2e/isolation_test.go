//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bcfmtolgahan/redis-redguard/test/utils"
)

// probeNamespace exists only so a delete aimed at it would visibly succeed if
// the guard let it through.
const probeNamespace = "redguard-guard-probe"

// foreignKubeconfig is an EKS-shaped kubeconfig for a cluster that does not
// exist. Tests never name a real cluster: the point is the shape, not a target.
const foreignKubeconfig = `apiVersion: v1
kind: Config
current-context: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
clusters:
- cluster:
    server: https://FAKE0000000000000000000000000000.gr7.eu-west-3.eks.amazonaws.com
  name: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
contexts:
- context:
    cluster: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
    user: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
  name: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
users:
- name: arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests
  user: {}
`

// writeTempKubeconfig puts a kubeconfig somewhere only this spec reads and
// removes it when the spec ends.
func writeTempKubeconfig(body string) string {
	path := filepath.Join(GinkgoT().TempDir(), "kubeconfig")
	Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
	return path
}

// impostorCluster is a second kind cluster as far as any file-level check can
// tell: the kubeconfig is well formed, self-consistent and served over
// loopback, but it belongs to a cluster this run did not create. This is the
// shape a parallel agent's cluster takes.
func impostorCluster() utils.Cluster {
	raw, err := os.ReadFile(activeCluster.Kubeconfig)
	Expect(err).NotTo(HaveOccurred())

	name := activeCluster.Name + "-not-ours"
	body := strings.ReplaceAll(string(raw), activeCluster.Context(), "kind-"+name)
	return utils.Cluster{Name: name, Kubeconfig: writeTempKubeconfig(body)}
}

// withGuardRecorder aims the suite's commands at cluster and collects refusals
// instead of ending the run, so a refusal can be asserted on.
func withGuardRecorder(cluster utils.Cluster, body func(refusals *[]error)) {
	var refusals []error

	savedCluster, savedGuard := activeCluster, guardViolation
	activeCluster = cluster
	guardViolation = func(_ string, _ []string, err error) { refusals = append(refusals, err) }
	defer func() { activeCluster, guardViolation = savedCluster, savedGuard }()

	body(&refusals)
}

var _ = Describe("cluster isolation", Ordered, func() {
	It("targets its own kind cluster while KUBECONFIG points somewhere else", func() {
		// The Makefile already runs the whole suite under a decoy KUBECONFIG.
		// This replaces it with a foreign one for the length of the spec, so
		// the assertion holds for a plausible kubeconfig too, not only a
		// deliberately broken one.
		saved, hadKubeconfig := os.LookupEnv("KUBECONFIG")
		Expect(os.Setenv("KUBECONFIG", writeTempKubeconfig(foreignKubeconfig))).To(Succeed())
		defer func() {
			if hadKubeconfig {
				Expect(os.Setenv("KUBECONFIG", saved)).To(Succeed())
				return
			}
			Expect(os.Unsetenv("KUBECONFIG")).To(Succeed())
		}()

		By("checking kubectl still resolves this run's context")
		out, err := kubectl("config", "current-context")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal(activeCluster.Context()))

		By("checking helm still reaches this run's cluster")
		_, err = run("helm", "list", "--all-namespaces")
		Expect(err).NotTo(HaveOccurred())

		By("checking the cluster identifies itself as the one this run created")
		Expect(activeCluster.VerifyLive()).To(Succeed())
	})

	It("refuses every command once the pinned kubeconfig stops being its own", func() {
		bogus := utils.Cluster{
			Name:       activeCluster.Name,
			Kubeconfig: writeTempKubeconfig(foreignKubeconfig),
		}
		err := bogus.Verify()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(activeCluster.Context()))

		withGuardRecorder(bogus, func(refusals *[]error) {
			_, readErr := kubectl("get", "namespace", "kube-system")
			Expect(readErr).To(HaveOccurred())
			Expect(*refusals).To(HaveLen(1), "a read against a foreign context was not refused")
		})
	})

	It("tells a second kind cluster apart from its own", func() {
		impostor := impostorCluster()

		By("confirming the file alone cannot distinguish them")
		Expect(impostor.Verify()).To(Succeed())

		By("confirming the API server can")
		err := impostor.VerifyLive()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(impostor.Name))
	})

	It("refuses a delete aimed at a cluster this run did not create", func() {
		By("creating a namespace the delete would remove if it ran")
		_, err := kubectl("create", "namespace", probeNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = kubectl("delete", "namespace", probeNamespace, "--ignore-not-found")
		})

		// The impostor's kubeconfig reaches a live API server holding the
		// namespace, so the delete is executable. Only the identity check
		// stands between it and a namespace this run does not own.
		withGuardRecorder(impostorCluster(), func(refusals *[]error) {
			_, err := kubectl("delete", "namespace", probeNamespace, "--ignore-not-found")
			Expect(err).To(HaveOccurred())
			Expect(*refusals).To(HaveLen(1), "the delete was not refused")
		})

		By("confirming the namespace is still there")
		out, err := kubectl("get", "namespace", probeNamespace, "-o", "jsonpath={.metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(probeNamespace), "the guard let a delete through")
	})

	It("refuses a helm uninstall aimed at a cluster this run did not create", func() {
		withGuardRecorder(impostorCluster(), func(refusals *[]error) {
			_, err := run("helm", "uninstall", helmRelease, "--namespace", operatorNamespace)
			Expect(err).To(HaveOccurred())
			Expect(*refusals).To(HaveLen(1), "the uninstall was not refused")
		})

		By("confirming the release this run installed is untouched")
		out, err := run("helm", "status", helmRelease, "--namespace", operatorNamespace, "-o", "json")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring(`"status":"deployed"`))
	})
})
