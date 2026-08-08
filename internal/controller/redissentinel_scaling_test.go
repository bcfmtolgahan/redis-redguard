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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	"github.com/bcfmtolgahan/redis-redguard/internal/builder"
	"github.com/bcfmtolgahan/redis-redguard/internal/redisclient/fake"
)

// A quorum above the number of Sentinels renders into sentinel.conf verbatim.
// Every pod then goes Ready and the status reports a healthy cluster, but no
// failover can ever reach quorum: the cluster is silently not HA. The API
// server is the only place that can refuse it before anything is created.
var _ = Describe("RedisSentinel quorum validation", func() {
	It("rejects a quorum larger than the sentinel count", func() {
		rs := newTestSentinel("quorum-too-high", "default")
		rs.Spec.SentinelConfig.Replicas = 3
		rs.Spec.SentinelConfig.Quorum = 4

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("quorum must not exceed"))
	})

	It("rejects lowering the sentinel count below the quorum", func() {
		rs := newTestSentinel("quorum-shrink", "default")
		rs.Spec.SentinelConfig.Replicas = 5
		rs.Spec.SentinelConfig.Quorum = 4
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, rs)).To(Succeed()) })

		rs.Spec.SentinelConfig.Replicas = 3
		err := k8sClient.Update(ctx, rs)
		Expect(err).To(HaveOccurred(),
			"shrinking the sentinel set below the quorum leaves a cluster that can never fail over")
		Expect(err.Error()).To(ContainSubstring("quorum must not exceed"))
	})

	It("accepts a quorum equal to the sentinel count", func() {
		rs := newTestSentinel("quorum-equal", "default")
		rs.Spec.SentinelConfig.Replicas = 3
		rs.Spec.SentinelConfig.Quorum = 3

		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	})
})

var _ = Describe("RedisSentinel PodDisruptionBudgets", func() {
	const resourceName = "pdb-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	var reconciler *RedisSentinelReconciler

	BeforeEach(func() {
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fake.NewFactory(),
		}
		Expect(k8sClient.Create(ctx, newTestSentinel(resourceName, "default"))).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)
	})

	AfterEach(func() {
		deleteSentinel(ctx, reconciler, key)
		drainEvents()
	})

	It("budgets both StatefulSets against a node drain", func() {
		owner := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, owner)).To(Succeed())

		for _, component := range []string{"redis", "sentinel"} {
			pdb := &policyv1.PodDisruptionBudget{}
			name := resourceName + "-" + component + "-pdb"
			Expect(k8sClient.Get(ctx, inDefault(name), pdb)).To(Succeed(),
				"no PodDisruptionBudget for %s: a node drain evicts every pod at once", component)
			expectControlledBy(pdb, owner)
			Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, inDefault(resourceName+"-"+component), sts)).To(Succeed())
			Expect(pdb.Spec.Selector.MatchLabels).To(Equal(sts.Spec.Template.Labels),
				"the %s budget selects pods that do not exist", component)
		}
	})

	It("recreates a deleted budget", func() {
		pdb := &policyv1.PodDisruptionBudget{}
		name := inDefault(resourceName + "-redis-pdb")
		Expect(k8sClient.Get(ctx, name, pdb)).To(Succeed())
		originalUID := pdb.UID
		Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())

		reconcileUntilSettled(ctx, reconciler, key)

		Expect(k8sClient.Get(ctx, name, pdb)).To(Succeed())
		Expect(pdb.UID).NotTo(Equal(originalUID))
	})

	It("updates a drifted budget in place", func() {
		name := inDefault(resourceName + "-redis-pdb")
		Eventually(func(g Gomega) {
			pdb := &policyv1.PodDisruptionBudget{}
			g.Expect(k8sClient.Get(ctx, name, pdb)).To(Succeed())
			pdb.Spec.Selector.MatchLabels = map[string]string{"drifted": "yes"}
			g.Expect(k8sClient.Update(ctx, pdb)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		reconcileUntilSettled(ctx, reconciler, key)

		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, name, pdb)).To(Succeed())
		Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "redis"),
			"a hand-edited budget was never corrected")
	})
})

// Scaling Redis down deletes the highest ordinals. When the master sits on one
// of them, deleting it hands Sentinel an unplanned failover on a cluster that
// is already losing a node, and the write Service has no endpoints until the
// operator has caught up with the promotion.
var _ = Describe("RedisSentinel master-safe scale-down", func() {
	const resourceName = "scaledown-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	podIPs := []string{"10.244.21.10", "10.244.21.11", "10.244.21.12"}

	var (
		fakeFactory *fake.Factory
		reconciler  *RedisSentinelReconciler
		podNames    []string
	)

	// setDesiredReplicas edits spec.redisConfig.replicas, retrying the write
	// against the Manager reconciling the same object.
	setDesiredReplicas := func(replicas int32) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			rs.Spec.RedisConfig.Replicas = replicas
			g.Expect(k8sClient.Update(ctx, rs)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	}

	liveReplicas := func(name string) int32 {
		GinkgoHelper()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, inDefault(name), sts)).To(Succeed())
		return *sts.Spec.Replicas
	}

	BeforeEach(func() {
		fakeFactory = fake.NewFactory()
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fakeFactory,
		}

		Expect(k8sClient.Create(ctx, newTestSentinel(resourceName, "default"))).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		template := builder.BuildRedisStatefulSet(rs).Spec.Template
		podNames = nil
		for i, ip := range podIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, template.Labels, ip)
			podNames = append(podNames, name)
		}
		drainEvents()
	})

	AfterEach(func() {
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)

		// envtest runs no garbage collector, so owned objects outlive their
		// owner. A StatefulSet left at the replica count one spec scaled it to
		// would be inherited by the next one.
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		drainEvents()
	})

	It("fails the master over before removing its ordinal", func() {
		By("running with the master on the highest ordinal")
		fakeFactory.SetMaster(podIPs[2] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		expectMasterLabelOnlyOn(ctx, podNames, podNames[2])
		drainEvents()

		By("scaling down past the master's ordinal")
		setDesiredReplicas(2)
		result := reconcileUntilSettled(ctx, reconciler, key)

		Expect(liveReplicas(resourceName+"-redis")).To(Equal(int32(3)),
			"the StatefulSet was shrunk while the master still sat on the ordinal being removed")
		Expect(failoverCalls(fakeFactory)).NotTo(BeEmpty(),
			"no failover was requested, so the master would be deleted without a promotion")
		Expect(result.RequeueAfter).To(Equal(failoverRequeue),
			"a deferred scale-down must be retried quickly, not on the periodic pass")
		Expect(findEvent("ScaleDownDeferred")).To(ContainSubstring("Warning"))

		By("completing the scale-down once Sentinel has promoted a surviving pod")
		fakeFactory.SetMaster(podIPs[0] + ":6379")
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(liveReplicas(resourceName + "-redis")).To(Equal(int32(2)))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("scales down immediately when the master survives", func() {
		fakeFactory.SetMaster(podIPs[0] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()

		setDesiredReplicas(2)
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(liveReplicas(resourceName + "-redis")).To(Equal(int32(2)))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(failoverCalls(fakeFactory)).To(BeEmpty(),
			"a scale-down that keeps the master must not disturb the cluster")
	})

	It("never forces a failover when scaling up", func() {
		fakeFactory.SetMaster(podIPs[2] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()

		setDesiredReplicas(5)
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(liveReplicas(resourceName + "-redis")).To(Equal(int32(5)))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(failoverCalls(fakeFactory)).To(BeEmpty())
	})

	It("holds the scale-down while no Sentinel can name the master", func() {
		By("scaling down with Sentinel unable to answer")
		setDesiredReplicas(2)
		result := reconcileUntilSettled(ctx, reconciler, key)

		Expect(liveReplicas(resourceName+"-redis")).To(Equal(int32(3)),
			"the ordinal being removed may be the master; nothing proved otherwise")
		Expect(result.RequeueAfter).To(Equal(failoverRequeue))
		Expect(findEvent("ScaleDownDeferred")).To(ContainSubstring("master"))
	})

	// Removing a Sentinel does not remove it from the survivors' view: they
	// keep it until a SENTINEL RESET. The leader election needs a majority of
	// the set each survivor knows, so a set shrunk past that majority detects
	// the master as down and never promotes anyone.
	It("makes the surviving Sentinels forget the ones that were removed", func() {
		fakeFactory.SetMaster(podIPs[0] + ":6379")
		fakeFactory.SetKnownSentinels(5)
		reconcileUntilSettled(ctx, reconciler, key)

		Expect(fakeFactory.Resets()).NotTo(BeEmpty(),
			"three sentinels still count five peers, so no failover can win an election")
		Expect(findEvent("SentinelPeersReset")).To(ContainSubstring("5"))
	})

	It("leaves a consistent Sentinel set alone", func() {
		fakeFactory.SetMaster(podIPs[0] + ":6379")
		fakeFactory.SetKnownSentinels(3)
		reconcileUntilSettled(ctx, reconciler, key)

		Expect(fakeFactory.Resets()).To(BeEmpty(),
			"a reset makes Sentinel forget the master's replicas until it rediscovers them; it is not a periodic operation")
	})

	It("warns that shrinking the Sentinel set weakens failover", func() {
		fakeFactory.SetMaster(podIPs[0] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()

		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			rs.Spec.SentinelConfig.Replicas = 5
			g.Expect(k8sClient.Update(ctx, rs)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()

		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			rs.Spec.SentinelConfig.Replicas = 3
			g.Expect(k8sClient.Update(ctx, rs)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		Expect(findEvent("SentinelScaleDown")).To(ContainSubstring("Warning"),
			"removed sentinels stay in the survivors' known set, so the majority needed to elect a failover leader is still counted from the old size")
	})
})

// failoverCalls returns the forced failovers the reconciler asked Sentinel for.
func failoverCalls(f *fake.Factory) []string {
	var out []string
	for _, call := range f.Calls() {
		if strings.HasSuffix(call, ":FailoverFromPool") {
			out = append(out, call)
		}
	}
	return out
}

// deleteSentinel removes the CR and drives the reconciler until the finalizer
// is gone, mirroring what the Manager-owned controller would do.
func deleteSentinel(ctx context.Context, r *RedisSentinelReconciler, key types.NamespacedName) {
	GinkgoHelper()

	rs := &redisv1alpha1.RedisSentinel{}
	Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
	Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	Eventually(func(g Gomega) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(errors.IsNotFound(k8sClient.Get(ctx, key, rs))).To(BeTrue())
	}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
}
