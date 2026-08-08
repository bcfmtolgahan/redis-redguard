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

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx    context.Context
	cancel context.CancelFunc

	testEnv *envtest.Environment
	cfg     *rest.Config

	// k8sClient talks to the API server directly (no cache). Specs need
	// read-after-write consistency, which the Manager's cached client cannot
	// guarantee, so this is deliberately NOT k8sManager.GetClient().
	k8sClient client.Client

	// k8sManager is a real controller-runtime Manager: it exercises
	// SetupWithManager, the informer cache and the watch/requeue machinery.
	k8sManager manager.Manager

	// testRecorder is handed to the reconcilers that specs construct
	// themselves, so events can be asserted without a nil-pointer panic.
	testRecorder *record.FakeRecorder
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = redisv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("starting a real controller manager")
	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	// Buffered far beyond what any spec records. FakeRecorder.Event blocks on a
	// full channel, so a spec that reconciles inside an Eventually loop wedges
	// the reconciler instead of failing, and the suite dies on the go test
	// deadline with no useful assertion.
	testRecorder = record.NewFakeRecorder(4096)

	// The managed reconciler gets the Manager's real recorder rather than
	// testRecorder: FakeRecorder.Event blocks once its buffer is full, which
	// would wedge the Manager's worker goroutine after 100 background
	// reconciles. testRecorder stays reserved for spec-owned reconcilers.
	//
	// It keeps the default Redis factory on purpose. Its dials all fail out
	// here, slowly, which keeps the background worker well behind the
	// spec-owned reconcilers; a fast factory makes it win races against the
	// specs whose events and diffs assume they converge the object first.
	// Spec-owned reconcilers inject fakes instead, so the specs themselves
	// never wait on those slow lookups.
	err = (&RedisSentinelReconciler{
		Client:   k8sManager.GetClient(),
		Scheme:   k8sManager.GetScheme(),
		Recorder: k8sManager.GetEventRecorderFor("redissentinel"),
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(k8sManager.Start(ctx)).To(Succeed())
	}()

	Expect(k8sManager.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// newTestSentinel returns a minimal RedisSentinel that satisfies the CRD schema.
// Field names/types follow api/v1alpha1/redissentinel_types.go: Storage is a
// *StorageSpec and Size is a resource.Quantity.
func newTestSentinel(name, ns string) *redisv1alpha1.RedisSentinel {
	return &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: redisv1alpha1.RedisSentinelSpec{
			RedisConfig: redisv1alpha1.RedisConfig{
				Replicas: 3,
				Image:    "redis:7-alpine",
				Storage: &redisv1alpha1.StorageSpec{
					Size: resource.MustParse("1Gi"),
				},
			},
			SentinelConfig: redisv1alpha1.SentinelConfig{
				Replicas:              3,
				Quorum:                2,
				DownAfterMilliseconds: 5000,
				FailoverTimeout:       10000,
				ParallelSyncs:         1,
			},
			ServiceType: corev1.ServiceTypeClusterIP,
		},
	}
}

// ensureTestSentinel creates the RedisSentinel referenced by dependent CRs if it
// is not there yet. Top-level containers are randomized by Ginkgo, so every
// spec has to be able to create its own fixtures.
func ensureTestSentinel(name, ns string) *redisv1alpha1.RedisSentinel {
	GinkgoHelper()

	existing := &redisv1alpha1.RedisSentinel{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	if err == nil {
		return existing
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected error getting RedisSentinel %s/%s: %v", ns, name, err)

	sentinel := newTestSentinel(name, ns)
	Expect(k8sClient.Create(ctx, sentinel)).To(Succeed())
	return sentinel
}

// ensureSecret creates an opaque Secret if it does not exist yet.
func ensureSecret(name, ns string, data map[string][]byte) {
	GinkgoHelper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
	err := k8sClient.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
