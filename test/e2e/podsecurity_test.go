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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	enforceLabel = "pod-security.kubernetes.io/enforce"
	// probePod is created only to confirm the API server rejects a
	// non-compliant pod; it is never expected to exist.
	probePod = "pod-security-probe"
)

// enforceRestrictedPodSecurity turns the namespace into one where the API
// server rejects any pod that does not meet the restricted standard.
func enforceRestrictedPodSecurity(namespace string) error {
	_, err := kubectl("label", "--overwrite", "namespace", namespace,
		enforceLabel+"=restricted",
		"pod-security.kubernetes.io/enforce-version=latest",
		"pod-security.kubernetes.io/audit=restricted",
		"pod-security.kubernetes.io/warn=restricted")
	return err
}

func clearPodSecurityLabels(namespace string) error {
	_, err := kubectl("label", "--overwrite", "namespace", namespace,
		enforceLabel+"-",
		"pod-security.kubernetes.io/enforce-version-",
		"pod-security.kubernetes.io/audit-",
		"pod-security.kubernetes.io/warn-")
	return err
}

// These specs are independent of the sample cluster's lifecycle, so they hold
// wherever Ginkgo orders them relative to the Redguard container.
var _ = Describe("Pod Security", func() {
	It("enforces the restricted standard in both namespaces", func() {
		for _, ns := range []string{operatorNamespace, clusterNamespace} {
			level, err := kubectl("get", "namespace", ns,
				"-o", "jsonpath={.metadata.labels.pod-security\\.kubernetes\\.io/enforce}")
			Expect(err).NotTo(HaveOccurred())
			Expect(level).To(Equal("restricted"),
				"namespace %s does not enforce restricted, so nothing in this suite proves compliance", ns)
		}
	})

	// Without this control a suite that silently lost the namespace labels
	// would still pass and would still claim the workloads are compliant.
	It("rejects a pod that does not meet the standard", func() {
		By("creating a pod with no security context in the cluster namespace")
		out, err := kubectl("run", probePod, "--restart=Never", "-n", clusterNamespace,
			"--image=registry.k8s.io/pause:3.10")
		DeferCleanup(func() {
			_, _ = kubectl("delete", "pod", probePod, "-n", clusterNamespace, "--ignore-not-found")
		})

		Expect(err).To(HaveOccurred(), "the API server admitted a pod that violates restricted: %s", out)
		Expect(err.Error()).To(ContainSubstring("violates PodSecurity"),
			"the pod was rejected for a reason other than Pod Security: %v", err)
	})

	It("runs the operator with the security context the chart declares", func() {
		pods, err := listPods(operatorNamespace, "control-plane=controller-manager")
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).To(HaveLen(1))

		spec, err := kubectl("get", "pod", pods[0].Name, "-n", operatorNamespace,
			"-o", "jsonpath={.spec.securityContext}{'\\n'}{.spec.containers[0].securityContext}")
		Expect(err).NotTo(HaveOccurred())

		lines := strings.SplitN(spec, "\n", 2)
		Expect(lines).To(HaveLen(2), "unexpected jsonpath output: %q", spec)
		for _, want := range []string{`"runAsNonRoot":true`, `"runAsUser":65532`, `"runAsGroup":65532`, `"type":"RuntimeDefault"`} {
			Expect(lines[0]).To(ContainSubstring(want), "operator pod securityContext: %s", lines[0])
		}
		for _, want := range []string{`"allowPrivilegeEscalation":false`, `"readOnlyRootFilesystem":true`, `"drop":["ALL"]`} {
			Expect(lines[1]).To(ContainSubstring(want), "manager container securityContext: %s", lines[1])
		}
	})
})
