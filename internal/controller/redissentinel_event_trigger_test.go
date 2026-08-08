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
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	"github.com/bcfmtolgahan/redis-redguard/internal/builder"
	redisfake "github.com/bcfmtolgahan/redis-redguard/internal/redisclient/fake"
)

// switchMasterEvent is what the sentinelwatch watcher emits: a reference to
// the cluster and nothing else. The +switch-master payload's addresses are
// dropped before this point, so nothing an attacker publishes can appear
// anywhere downstream.
func switchMasterEvent(key types.NamespacedName) event.TypedGenericEvent[*redisv1alpha1.RedisSentinel] {
	return event.TypedGenericEvent[*redisv1alpha1.RedisSentinel]{
		Object: &redisv1alpha1.RedisSentinel{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		},
	}
}

// sendEvent pushes one event into the channel source, retrying until the
// started controller is draining it.
func sendEvent(events chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel], key types.NamespacedName) {
	GinkgoHelper()
	Eventually(func() bool {
		select {
		case events <- switchMasterEvent(key):
			return true
		default:
			return false
		}
	}, 10*time.Second, 50*time.Millisecond).Should(BeTrue(), "the channel source never started draining events")
}

// probeSerial keeps controller names unique: controller-runtime validates
// them process-wide, and every It builds a fresh probe manager.
var probeSerial atomic.Int64

// startProbeManager runs a manager whose single controller is fed only by the
// channel source: no For, no Owns. Every reconcile it performs is therefore
// attributable to an event, which is what these specs assert on.
func startProbeManager(ctx context.Context, name string, events chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel], r reconcile.Reconciler) context.CancelFunc {
	GinkgoHelper()

	mgrCtx, cancel := context.WithCancel(ctx)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  k8sClient.Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	err = ctrl.NewControllerManagedBy(mgr).
		Named(fmt.Sprintf("%s-%d", name, probeSerial.Add(1))).
		WatchesRawSource(source.Channel(events,
			&handler.TypedEnqueueRequestForObject[*redisv1alpha1.RedisSentinel]{})).
		Complete(r)
	Expect(err).NotTo(HaveOccurred())

	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	return func() {
		cancel()
		Eventually(done, 10*time.Second).Should(BeClosed())
	}
}

// requestLog records the reconcile requests a stub reconciler received.
type requestLog struct {
	mu       sync.Mutex
	requests []types.NamespacedName
}

func (l *requestLog) record(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req.NamespacedName)
	return reconcile.Result{}, nil
}

func (l *requestLog) forKey(key types.NamespacedName) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.requests {
		if r == key {
			n++
		}
	}
	return n
}

var _ = Describe("RedisSentinel switch-master event enqueue", func() {
	It("enqueues exactly one reconcile for the object one event names", func() {
		events := make(chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel])
		log := &requestLog{}
		stop := startProbeManager(ctx, "switchmaster-enqueue-probe", events, reconcile.Func(log.record))
		defer stop()

		key := inDefault("enqueue-probe-target")
		sendEvent(events, key)

		Eventually(func() int { return log.forKey(key) }, 10*time.Second, 50*time.Millisecond).
			Should(Equal(1), "one publication must wake exactly one reconcile")
		Consistently(func() int { return log.forKey(key) }, time.Second, 100*time.Millisecond).
			Should(Equal(1), "a single event must not fan out into repeated reconciles")
	})
})

// The subscription is a trigger, never a source of truth. An earlier release
// acted on the event's own view of the master and lost acknowledged writes;
// these specs pin the repaired discipline against the new wake-up path: the
// reconcile an event triggers resolves the master with Sentinel and acts on
// that answer alone.
var _ = Describe("RedisSentinel switch-master trigger discipline", func() {
	const resourceName = "evtrig-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	podIPs := []string{"10.244.61.10", "10.244.61.11", "10.244.61.12"}

	var (
		fakeFactory *redisfake.Factory
		reconciler  *RedisSentinelReconciler
		podNames    []string
		events      chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel]
		stopMgr     context.CancelFunc
	)

	// callsSince slices the factory log after start and keeps this cluster's
	// entries: sentinel pool calls carry the pool's joined DNS addresses,
	// SlaveOf entries carry the pod IPs.
	callsSince := func(start int) []string {
		all := fakeFactory.Calls()
		if start > len(all) {
			start = len(all)
		}
		var mine []string
		for _, c := range all[start:] {
			if strings.Contains(c, resourceName) || strings.Contains(c, "10.244.61.") {
				mine = append(mine, c)
			}
		}
		return mine
	}

	countSuffix := func(calls []string, suffix string) int {
		n := 0
		for _, c := range calls {
			if strings.HasSuffix(c, suffix) {
				n++
			}
		}
		return n
	}

	BeforeEach(func() {
		fakeFactory = redisfake.NewFactory()
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     record.NewFakeRecorder(2000),
			RedisFactory: fakeFactory,
		}

		rs := newTestSentinel(resourceName, "default")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		template := builder.BuildRedisStatefulSet(rs).Spec.Template
		podNames = nil
		for i, ip := range podIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, template.Labels, ip)
			podNames = append(podNames, name)
		}

		fakeFactory.SetMaster(podIPs[0] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		expectMasterLabelOnlyOn(ctx, podNames, podNames[0])

		events = make(chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel])
		stopMgr = startProbeManager(ctx, "switchmaster-trigger-probe", events, reconciler)
	})

	AfterEach(func() {
		stopMgr()
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, sts, client.GracePeriodSeconds(0))
		}
	})

	It("wakes a reconcile that asks Sentinel and leaves a healthy master alone", func() {
		before := len(fakeFactory.Calls())
		sendEvent(events, key)

		// The woken pass consults Sentinel: that query is the source of truth
		// the label is derived from.
		Eventually(func() int {
			return countSuffix(callsSince(before), ":GetMasterAddrFromPool")
		}, 10*time.Second, 100*time.Millisecond).Should(BeNumerically(">=", 1),
			"the event never woke a reconcile that asked Sentinel for the master")

		// Sentinel still names pod 0, so however loudly the event arrived,
		// nothing may move and nobody may be demoted.
		Consistently(func(g Gomega) {
			for _, c := range callsSince(before) {
				g.Expect(c).NotTo(HaveSuffix(":SlaveOf"),
					"an event with an unchanged master must not reconfigure replication")
			}
		}, 1500*time.Millisecond, 200*time.Millisecond).Should(Succeed())
		expectMasterLabelOnlyOn(ctx, podNames, podNames[0])
	})

	It("does nothing for an event naming a cluster that does not exist", func() {
		before := len(fakeFactory.Calls())
		sendEvent(events, inDefault("no-such-cluster"))

		// The reconcile finds no CR and returns before dialing anything; a
		// forged or stale event for a deleted cluster costs nothing.
		Consistently(func() []string {
			return callsSince(before)
		}, 1500*time.Millisecond, 200*time.Millisecond).Should(BeEmpty(),
			"an event for a nonexistent RedisSentinel reached Redis or Sentinel")
	})

	It("moves the label only where Sentinel confirms the master", func() {
		// The promotion happened: Sentinel's answer changes, and the event --
		// which carries no address at all -- merely reports that it is worth
		// asking again.
		fakeFactory.SetMaster(podIPs[1] + ":6379")
		before := len(fakeFactory.Calls())
		sendEvent(events, key)

		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			g.Expect(rs.Status.MasterNode).To(Equal(podIPs[1]+":6379"),
				"the status does not track the promoted master")
			expectMasterLabelOnlyOn(ctx, podNames, podNames[1])
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		// Replication was reconfigured only after the reconcile re-verified
		// the promotion with Sentinel: quorum first, then the master address
		// again, and only then any SLAVEOF.
		calls := callsSince(before)
		firstSlaveOf, firstQuorum, firstMasterQuery := -1, -1, -1
		for i, c := range calls {
			switch {
			case strings.HasSuffix(c, ":SlaveOf") && firstSlaveOf < 0:
				firstSlaveOf = i
			case strings.HasSuffix(c, ":CheckQuorumFromPool") && firstQuorum < 0:
				firstQuorum = i
			case strings.HasSuffix(c, ":GetMasterAddrFromPool") && firstMasterQuery < 0:
				firstMasterQuery = i
			}
		}
		Expect(firstSlaveOf).To(BeNumerically(">", 0),
			"the old master was never demoted, calls: %v", calls)
		Expect(firstQuorum).To(BeNumerically(">=", 0), "quorum was never verified")
		Expect(firstMasterQuery).To(BeNumerically(">=", 0), "the master was never re-queried")
		Expect(firstSlaveOf).To(BeNumerically(">", firstQuorum),
			"SLAVEOF was issued before the quorum check")
		Expect(firstSlaveOf).To(BeNumerically(">", firstMasterQuery),
			"SLAVEOF was issued before Sentinel confirmed the master")
	})
})
