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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redguard/redguard/test/utils"
)

const (
	// operatorNamespace is where the Helm release is installed.
	operatorNamespace = "redguard-system"
	// helmRelease is the release name; every chart resource is named after it.
	helmRelease = "redguard"
	// chartPath is the only chart in the repository.
	chartPath = "charts/redguard"

	// The operator image is built from this working tree and side-loaded into
	// kind, so the chart must never try to pull it.
	imageRepository = "redguard"
	imageTag        = "e2e"
)

var projectImage = imageRepository + ":" + imageTag

// kindCluster and kindBinary follow the Makefile, which passes both through the
// environment.
func kindCluster() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok && v != "" {
		return v
	}
	return "kind"
}

func kindBinary() string {
	if v, ok := os.LookupEnv("KIND"); ok && v != "" {
		return v
	}
	return "kind"
}

// pinKubeconfig exports the kind cluster's credentials to a file only this
// process reads, and points every command the suite spawns at it. Child
// processes inherit the environment, so this covers kubectl and helm alike.
func pinKubeconfig() error {
	cluster := kindCluster()
	out, err := run(kindBinary(), "get", "kubeconfig", "--name", cluster)
	if err != nil {
		return fmt.Errorf("read kubeconfig for kind cluster %s: %w", cluster, err)
	}

	dir, err := os.MkdirTemp("", "redguard-e2e-kubeconfig")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Setenv("KUBECONFIG", path)
}

// TestE2E runs the end-to-end suite against a live cluster. It requires a kind
// cluster to already exist (see the setup-test-e2e target) and a running Docker
// daemon to build the operator image.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting redguard e2e suite\n")
	RunSpecs(t, "e2e suite")
}

// BeforeSuite installs the operator exactly the way the README tells a user to:
// a chart install from charts/redguard. Installing through the chart is what
// exercises the generated RBAC; a kustomize install would test a different set
// of permissions than the one users get.
var _ = BeforeSuite(func() {
	// Every kubectl and helm invocation resolves the current context afresh
	// from the shared kubeconfig, and this suite deletes namespaces and
	// cluster-scoped RBAC. Anything that switches contexts while it runs -- a
	// second kind cluster being created, a person running kubectl -- would aim
	// those deletes at whatever cluster is current at that moment. Pinning a
	// kubeconfig of its own makes the target independent of that file.
	By("pinning the suite to the kind cluster's own kubeconfig")
	Expect(pinKubeconfig()).To(Succeed())

	By("building the operator image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+projectImage))
	Expect(err).NotTo(HaveOccurred(), "failed to build the operator image")

	By("loading the operator image into the kind cluster")
	Expect(utils.LoadImageToKindClusterWithName(projectImage)).To(Succeed(),
		"failed to load the operator image into kind")

	// Enforcement is turned on before anything is installed, so the API server
	// rejects the operator pod and every pod the operator builds unless they
	// meet the restricted standard. The suite forming a cluster is then proof
	// of compliance rather than an assertion about it.
	By("enforcing the restricted Pod Security Standard on both namespaces")
	_, _ = kubectl("create", "namespace", operatorNamespace)
	for _, ns := range []string{operatorNamespace, clusterNamespace} {
		Expect(enforceRestrictedPodSecurity(ns)).To(Succeed(),
			"failed to label namespace %s", ns)
	}

	By("installing the chart")
	_, err = run("helm", "upgrade", "--install", helmRelease, chartPath,
		"--namespace", operatorNamespace,
		"--create-namespace",
		"--set", "operator.image.repository="+imageRepository,
		"--set", "operator.image.tag="+imageTag,
		"--set", "operator.image.pullPolicy=Never",
		"--wait", "--timeout", "5m")
	Expect(err).NotTo(HaveOccurred(), "helm install failed")
})

var _ = AfterSuite(func() {
	// The pinned kubeconfig holds cluster credentials; it outlives the suite
	// otherwise, since it sits in a temp directory of its own.
	defer func() {
		if path := os.Getenv("KUBECONFIG"); filepath.Base(path) == "config" {
			_ = os.RemoveAll(filepath.Dir(path))
		}
	}()

	By("uninstalling the chart")
	if _, err := run("helm", "uninstall", helmRelease, "--namespace", operatorNamespace, "--wait"); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "helm uninstall failed: %v\n", err)
	}

	By("removing the operator namespace")
	if _, err := kubectl("delete", "namespace", operatorNamespace, "--ignore-not-found"); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "namespace delete failed: %v\n", err)
	}

	// The cluster namespace outlives the suite, so its enforcement labels are
	// removed rather than left on a namespace the suite no longer owns.
	By("clearing the Pod Security labels from the cluster namespace")
	if err := clearPodSecurityLabels(clusterNamespace); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "namespace unlabel failed: %v\n", err)
	}
})
