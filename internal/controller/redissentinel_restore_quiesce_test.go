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
	"fmt"
	"io"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	"github.com/redguard/redguard/internal/redisclient/fake"
	"github.com/redguard/redguard/internal/sentinel"
)

// The restore quiesce and the sentinel monitor convergence write the same
// per-sentinel runtime value. These specs drive both reconcilers over one fake
// factory to pin the interleaving from the incident: quiesce, master shutdown,
// then a sentinel reconcile pass before the master returns. Timing plays no
// part; each step is a direct reconcile call.
var _ = Describe("RedisSentinel restore quiesce coordination", func() {
	const resourceName = "quiesce-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	redisIPs := []string{"10.244.33.10", "10.244.33.11", "10.244.33.12"}
	sentinelIPs := []string{"10.244.33.20", "10.244.33.21", "10.244.33.22"}

	var (
		fakeFactory       *fake.Factory
		reconciler        *RedisSentinelReconciler
		restoreReconciler *RedisRestoreReconciler
		podNames          []string
		restoreNames      []string
	)

	redisAddr := func(i int) string { return redisIPs[i] + ":6379" }
	sentinelAddr := func(i int) string { return sentinelIPs[i] + ":26379" }

	// memberMonitor reads the monitor state one sentinel member itself reports,
	// which is the state the next failure detection would run on.
	memberMonitor := func(i int) *sentinel.MasterInfo {
		GinkgoHelper()
		pool := fakeFactory.NewSentinelPool([]string{sentinelAddr(i)}, "", nil)
		info, err := pool.GetMasterFromPool(ctx, resourceName+"-master")
		Expect(err).NotTo(HaveOccurred())
		return info
	}

	// createRestore seeds a RedisRestore with the given phase and quiesce
	// breadcrumb, as the restore controller would have persisted them.
	createRestore := func(name, clusterRef string, phase redisv1alpha1.RestorePhase, quiesced int32) {
		GinkgoHelper()
		restore := &redisv1alpha1.RedisRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: redisv1alpha1.RedisRestoreSpec{
				RedisClusterRef: clusterRef,
				BackupSource: redisv1alpha1.BackupSource{
					S3: redisv1alpha1.S3Config{
						Bucket:               "test-bucket",
						Region:               "us-east-1",
						CredentialsSecretRef: "s3-creds",
					},
					BackupPath: "backups/" + clusterRef + "/backup-1.rdb.gz",
				},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())
		restoreNames = append(restoreNames, name)
		restore.Status.Phase = phase
		restore.Status.ObservedGeneration = restore.Generation
		restore.Status.StartTime = &metav1.Time{Time: time.Now()}
		restore.Status.QuiescedDownAfterMilliseconds = quiesced
		Expect(k8sClient.Status().Update(ctx, restore)).To(Succeed())
	}

	// quiesceViaRestore runs the restore reconciler through its Restoring pass:
	// it records the breadcrumb, raises down-after-milliseconds on every
	// sentinel and shuts the master down for the payload swap.
	quiesceViaRestore := func(name string) {
		GinkgoHelper()
		createRestore(name, resourceName, redisv1alpha1.RestorePhaseRestoring, 0)
		result, err := restoreReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: inDefault(name)})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(fakeFactory.Calls()).To(ContainElement(redisAddr(0)+":ShutdownNoSave"),
			"the restoring pass must shut the master down for the init-script swap")
		for i := range sentinelIPs {
			Expect(memberMonitor(i).DownAfterMilliseconds).To(Equal("600000"),
				"sentinel %d was not quiesced before the shutdown", i)
		}
	}

	BeforeEach(func() {
		fakeFactory = fake.NewFactory()
		fakeFactory.SetMonitorConfig(specMonitorConfig())
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fakeFactory,
		}
		restoreReconciler = &RedisRestoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fakeFactory,
			// The staged payload is present: the master restart has not
			// happened yet, so the pass ahead is quiesce plus shutdown.
			PodExec: func(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdin io.Reader) (string, string, error) {
				return "present\n", "", nil
			},
		}

		rs := newTestSentinel(resourceName, "default")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		redisTemplate := builder.BuildRedisStatefulSet(rs).Spec.Template
		sentinelTemplate := builder.BuildSentinelStatefulSet(rs).Spec.Template
		podNames = nil
		restoreNames = nil
		for i, ip := range redisIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, redisTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		for i, ip := range sentinelIPs {
			name := fmt.Sprintf("%s-sentinel-%d", resourceName, i)
			createRunningPod(ctx, name, sentinelTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		fakeFactory.SetMaster(redisAddr(0))

		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		for _, name := range restoreNames {
			restore := &redisv1alpha1.RedisRestore{}
			restore.Name, restore.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, restore))).To(Succeed())
		}
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		drainEvents()
	})

	It("keeps the quiesce in place while the restore's master restart is in flight", func() {
		quiesceViaRestore("quiesce-active")

		// The incident interleaving: a sentinel reconcile pass lands between
		// the shutdown and the master coming back.
		reconcileUntilSettled(ctx, reconciler, key)

		for i := range sentinelIPs {
			Expect(memberMonitor(i).DownAfterMilliseconds).To(Equal("600000"),
				"sentinel %d was converged back to the spec mid-restart; it then declares +sdown on the deliberately-down master and the failover resyncs the restored dataset away", i)
		}
	})

	It("keeps converging the monitor parameters the restore does not hold", func() {
		// failover-timeout drifted before the restore began; the quiesce must
		// not turn off drift repair wholesale.
		fakeFactory.SetMonitorConfig(map[string]string{
			"quorum":                  "2",
			"down-after-milliseconds": "5000",
			"failover-timeout":        "20000",
			"parallel-syncs":          "1",
		})
		quiesceViaRestore("quiesce-others")

		reconcileUntilSettled(ctx, reconciler, key)

		for i := range sentinelIPs {
			info := memberMonitor(i)
			Expect(info.FailoverTimeout).To(Equal("10000"),
				"sentinel %d keeps a drifted failover-timeout: the quiesce covers only the parameter the restore raised", i)
			Expect(info.DownAfterMilliseconds).To(Equal("600000"),
				"sentinel %d lost the quiesce while an unrelated parameter was repaired", i)
		}
	})

	It("resumes converging down-after-milliseconds once the restore is terminal", func() {
		quiesceViaRestore("quiesce-failed")

		// failRestore could not reach the sentinels: the phase is terminal but
		// the breadcrumb is still set. The reconciler is the promised fallback.
		restore := &redisv1alpha1.RedisRestore{}
		Expect(k8sClient.Get(ctx, inDefault("quiesce-failed"), restore)).To(Succeed())
		restore.Status.Phase = redisv1alpha1.RestorePhaseFailed
		Expect(k8sClient.Status().Update(ctx, restore)).To(Succeed())

		reconcileUntilSettled(ctx, reconciler, key)

		for i := range sentinelIPs {
			Expect(memberMonitor(i).DownAfterMilliseconds).To(Equal("5000"),
				"sentinel %d still cannot detect failures although no restore is running", i)
		}
	})

	It("does not honour a quiesce held by a restore for a different cluster", func() {
		createRestore("quiesce-foreign", "some-other-cluster", redisv1alpha1.RestorePhaseRestoring, 5000)
		// This cluster's sentinels carry a leftover raised value that no
		// restore of this cluster owns.
		fakeFactory.SetMonitorConfig(map[string]string{
			"quorum":                  "2",
			"down-after-milliseconds": "600000",
			"failover-timeout":        "10000",
			"parallel-syncs":          "1",
		})

		reconcileUntilSettled(ctx, reconciler, key)

		for i := range sentinelIPs {
			Expect(memberMonitor(i).DownAfterMilliseconds).To(Equal("5000"),
				"sentinel %d stays failover-blind because of a restore that targets another cluster", i)
		}
	})
})
