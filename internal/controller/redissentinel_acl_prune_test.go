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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// The aclfile lives on the retained data volume, so deleting a RedisSentinel
// and recreating it under the same name restores every account it ever saved
// -- including one whose RedisUser was revoked in between, which then holds a
// password nothing rotates and no CR references. These specs pin the prune
// pass that makes the declared RedisUser set authoritative again, and the
// guards that keep it from deleting anything whose owner was merely invisible.
var _ = Describe("RedisSentinel orphaned ACL account pruning", func() {
	const resourceName = "acl-prune-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	recordKey := types.NamespacedName{Name: aclOwnersName(resourceName), Namespace: "default"}
	podIPs := []string{"10.244.61.10", "10.244.61.11", "10.244.61.12"}

	var (
		fakeFactory *redisfake.Factory
		reconciler  *RedisSentinelReconciler
		recorder    *record.FakeRecorder
		podNames    []string
	)

	// findRecordedEvent scans everything recorded so far without blocking.
	findRecordedEvent := func(reason string) string {
		for {
			select {
			case e := <-recorder.Events:
				if strings.Contains(e, " "+reason+" ") {
					return e
				}
			default:
				return ""
			}
		}
	}

	// writeOwnershipRecord stands in for what the RedisUser controller wrote
	// when it applied these accounts, before their CRs were deleted.
	writeOwnershipRecord := func(entries map[string]string) {
		GinkgoHelper()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: recordKey.Name, Namespace: recordKey.Namespace},
			Data:       entries,
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
	}

	delUserOps := func() []string {
		var out []string
		for _, op := range fakeFactory.Ops() {
			if strings.Contains(op, ":ACLDelUser:") {
				out = append(out, op)
			}
		}
		return out
	}

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
	})

	AfterEach(func() {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = recordKey.Name, recordKey.Namespace
		_ = k8sClient.Delete(ctx, cm)

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

	It("removes an account whose RedisUser no longer exists", func() {
		for _, ip := range podIPs {
			fakeFactory.SeedUser(ip+":6379", "ghost", "on")
		}
		writeOwnershipRecord(map[string]string{"ghost": "long-deleted-user"})

		reconcileUntilSettled(ctx, reconciler, key)

		for _, ip := range podIPs {
			addr := ip + ":6379"
			Expect(delUserOps()).To(ContainElement(addr+":ACLDelUser:ghost"),
				"the revoked account is still live on %s", addr)
			Expect(fakeFactory.SavedUsers(addr)).NotTo(HaveKey("ghost"),
				"the aclfile on %s still holds the account, so its next restart brings it back", addr)
		}

		event := findRecordedEvent("ACLUserPruned")
		Expect(event).NotTo(BeEmpty(),
			"an account disappearing without an event cannot be explained afterwards")
		Expect(event).To(ContainSubstring("ghost"))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, recordKey, cm)).To(Succeed())
		Expect(cm.Data).NotTo(HaveKey("ghost"),
			"a record kept after a complete prune would mark a future hand-made account of the same name for deletion")
	})

	It("keeps an account a live RedisUser declares", func() {
		user := &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-kept", Namespace: "default"},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   resourceName,
				Username:          "kept",
				PasswordSecretRef: "prune-kept-pass",
			},
		}
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, user) })

		for _, ip := range podIPs {
			fakeFactory.SeedUser(ip+":6379", "kept", "on")
		}
		writeOwnershipRecord(map[string]string{"kept": "prune-kept"})

		reconcileUntilSettled(ctx, reconciler, key)

		Expect(delUserOps()).To(BeEmpty())
		for _, ip := range podIPs {
			Expect(fakeFactory.SavedUsers(ip + ":6379")).To(HaveKey("kept"))
		}
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, recordKey, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("kept"),
			"the ownership record must outlive the pass, or the account could never be pruned later")
	})

	It("never touches 'default', even when the ownership record names it", func() {
		// Every node reports 'default' and no RedisUser may declare it, so
		// without its own guard this record entry reads as an orphan.
		writeOwnershipRecord(map[string]string{"default": "hand-edited-entry"})

		reconcileUntilSettled(ctx, reconciler, key)

		Expect(delUserOps()).To(BeEmpty(),
			"'default' is the account the operator authenticates as; deleting it locks the operator out")
	})

	It("prunes nothing when the RedisUser list cannot be read", func() {
		for _, ip := range podIPs {
			fakeFactory.SeedUser(ip+":6379", "ghost", "on")
		}
		writeOwnershipRecord(map[string]string{"ghost": "long-deleted-user"})

		blinded := *reconciler
		blinded.Client = failingRedisUserList{Client: k8sClient}
		reconcileUntilSettled(ctx, &blinded, key)

		Expect(delUserOps()).To(BeEmpty(),
			"an unreadable RedisUser list is not an empty one; the owner may simply not be visible")
		for _, ip := range podIPs {
			Expect(fakeFactory.SavedUsers(ip + ":6379")).To(HaveKey("ghost"))
		}
	})

	It("does not report a partial prune as success while a node is unreachable", func() {
		for _, ip := range podIPs {
			fakeFactory.SeedUser(ip+":6379", "ghost", "on")
		}
		fakeFactory.SetError(podIPs[2]+":6379", "ACLUsers", fmt.Errorf("connection refused"))
		writeOwnershipRecord(map[string]string{"ghost": "long-deleted-user"})

		reconcileUntilSettled(ctx, reconciler, key)

		// The reachable nodes are cleaned: a revoked account must not stay
		// live on them because one peer is down.
		for _, ip := range podIPs[:2] {
			Expect(fakeFactory.SavedUsers(ip + ":6379")).NotTo(HaveKey("ghost"))
		}
		Expect(fakeFactory.SavedUsers(podIPs[2] + ":6379")).To(HaveKey("ghost"))

		Expect(findRecordedEvent("ACLPruneIncomplete")).NotTo(BeEmpty(),
			"a partial prune reported as success is unexplainable when the account reappears")

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, recordKey, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("ghost"),
			"dropping the record now would leave the account on the unreachable node unowned forever")
	})
})

// failingRedisUserList makes listing RedisUsers fail while every other read
// works, which is the exact state pruning must treat as "owners unknown"
// rather than "no owners".
type failingRedisUserList struct {
	client.Client
}

func (f failingRedisUserList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*redisv1alpha1.RedisUserList); ok {
		return fmt.Errorf("injected RedisUser list failure")
	}
	return f.Client.List(ctx, list, opts...)
}
