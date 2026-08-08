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
)

// imageTag is the cluster name, which is unique to this run. A shared tag
// would let a second suite rebuild the image between this one's build and its
// load into kind, so the cluster would come up on someone else's binary.
var (
	imageTag     string
	projectImage string
)

// releaseKubeconfig removes the credentials this run copied out of kind.
var releaseKubeconfig = func() {}

// kindCluster is the cluster the Makefile created for this run. There is no
// default: kind's own default is the cluster named "kind", and reaching for it
// would mean operating on a cluster nobody asked for.
func kindCluster() string {
	return os.Getenv("KIND_CLUSTER")
}

func kindBinary() string {
	if v, ok := os.LookupEnv("KIND"); ok && v != "" {
		return v
	}
	return "kind"
}

// containerTool has to match the one `make docker-build` used, or the image
// this run built would be removed from the wrong store, or not at all.
func containerTool() string {
	if v, ok := os.LookupEnv("CONTAINER_TOOL"); ok && v != "" {
		return v
	}
	return "docker"
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
	// kubectl and helm resolve the current context afresh on every call, and
	// this suite deletes namespaces and cluster-scoped RBAC. Anything that
	// switches contexts while it runs -- a second kind cluster being created,
	// a person running kubectl -- would aim those deletes at whatever cluster
	// is current at that moment. Copying the credentials into a file of this
	// run's own and naming it on every command line removes that coupling.
	By("pinning the suite to the kind cluster this run was given")
	cluster := kindCluster()
	Expect(cluster).NotTo(BeEmpty(),
		"KIND_CLUSTER is unset; run the suite through 'make test-e2e', which creates a cluster for it")

	var err error
	activeCluster, releaseKubeconfig, err = utils.NewKindCluster(kindBinary(), cluster)
	Expect(err).NotTo(HaveOccurred(), "failed to pin the kind cluster's kubeconfig")

	By("confirming the pinned kubeconfig reaches that kind cluster and no other")
	Expect(activeCluster.VerifyLive()).To(Succeed())

	imageTag = cluster
	projectImage = imageRepository + ":" + imageTag

	By("building the operator image")
	_, err = utils.Run(exec.Command("make", "docker-build", "IMG="+projectImage))
	Expect(err).NotTo(HaveOccurred(), "failed to build the operator image")

	By("loading the operator image into the kind cluster")
	Expect(utils.LoadImageToKindCluster(kindBinary(), cluster, projectImage)).To(Succeed(),
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
	// The pinned kubeconfig holds cluster credentials and would otherwise
	// outlive the run.
	defer releaseKubeconfig()

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

	// The tag is unique per run, so nothing would ever overwrite it.
	By("removing the operator image this run built")
	if projectImage != "" {
		if _, err := run(containerTool(), "image", "rm", "-f", projectImage); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "image removal failed: %v\n", err)
		}
	}
})
