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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bcfmtolgahan/redis-redguard/internal/builder"
	redisfake "github.com/bcfmtolgahan/redis-redguard/internal/redisclient/fake"
)

// A Redis pod deleted while it is master comes back before Sentinel has
// finished promoting a replacement, so its start-up script asks Sentinel for
// the master and is told the address it used to hold itself. It then follows a
// dead address forever: the failover branch reacts to the master changing, and
// that change is already in the past by the time the pod boots. Observed on a
// three-node EKS cluster from nothing more exotic than deleting the master pod.
var _ = Describe("RedisSentinel stray replica repair", func() {
	const resourceName = "stray-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	podIPs := []string{"10.244.51.10", "10.244.51.11", "10.244.51.12"}

	var (
		fakeFactory *redisfake.Factory
		reconciler  *RedisSentinelReconciler
		recorder    *record.FakeRecorder
		podNames    []string
	)

	BeforeEach(func() {
		fakeFactory = redisfake.NewFactory()
		recorder = record.NewFakeRecorder(2000)
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     recorder,
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
	})

	AfterEach(func() {
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

	It("repoints a replica left following an address Sentinel no longer reports", func() {
		// The failover happened and was already handled: the master moved to
		// pod 1 and the status caught up before the old master came back.
		fakeFactory.SetMaster(podIPs[1] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		expectMasterLabelOnlyOn(ctx, podNames, podNames[1])

		// Pod 0 returns following the address it held before it was deleted.
		fakeFactory.SetReplicaOf(podIPs[0]+":6379", podIPs[0]+":6379")

		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)

			client := fakeFactory.NewClient(podIPs[0]+":6379", "", nil)
			defer client.Close()
			info, err := client.GetReplicationInfo(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(info["role"]).To(Equal("slave"))
			g.Expect(info["master_host"]).To(Equal(podIPs[1]),
				"a replica following a dead address never recovers on its own: nothing re-runs the start-up script")
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("does not demote a pod that reports itself master", func() {
		// Deciding between two masters belongs to the failover path, which
		// re-queries Sentinel first. Demoting here on a stale view is how an
		// acknowledged write is lost.
		before := len(fakeFactory.Calls())
		reconcileOnce(Default, reconciler, key)

		for _, call := range fakeFactory.Calls()[before:] {
			Expect(call).NotTo(Equal(podIPs[0]+":6379:SlaveOf"),
				"the master must not be demoted by the steady-state repair")
		}
	})

	It("leaves a correctly following replica alone", func() {
		before := len(fakeFactory.Calls())
		reconcileOnce(Default, reconciler, key)

		var slaveOfCalls []string
		for _, call := range fakeFactory.Calls()[before:] {
			if strings.HasSuffix(call, ":SlaveOf") {
				slaveOfCalls = append(slaveOfCalls, call)
			}
		}
		Expect(slaveOfCalls).To(BeEmpty(),
			"repointing a replica that already follows the master is pointless write traffic on every pass")
	})
})
