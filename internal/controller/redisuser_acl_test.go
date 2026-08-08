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
	"fmt"
	"slices"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// TestEmptyKeysGrantsNothing pins the least-privilege default: a RedisUser that
// asks for no permissions gets none.
func TestEmptyKeysGrantsNothing(t *testing.T) {
	user := &redisv1alpha1.RedisUser{
		Spec: redisv1alpha1.RedisUserSpec{Username: "appuser"},
	}

	rules := buildACLRules(user, "secret")

	for _, granted := range []string{"~*", "allkeys", "&*", "allchannels", "+@read", "+@all"} {
		if slices.Contains(rules, granted) {
			t.Errorf("empty aclRules produced %q; an unconfigured user must reach nothing, got %q", granted, rules)
		}
	}
}

func TestBuildACLRulesHonoursEnabled(t *testing.T) {
	cases := map[string]struct {
		enabled *bool
		want    string
	}{
		"unset defaults to enabled": {enabled: nil, want: "on"},
		"explicitly enabled":        {enabled: ptr.To(true), want: "on"},
		"explicitly disabled":       {enabled: ptr.To(false), want: "off"},
	}
	for name, tc := range cases {
		user := &redisv1alpha1.RedisUser{
			Spec: redisv1alpha1.RedisUserSpec{Username: "appuser", Enabled: tc.enabled},
		}
		rules := buildACLRules(user, "secret")
		if !slices.Contains(rules, tc.want) {
			t.Errorf("%s: rules %q missing %q", name, rules, tc.want)
		}
	}
}

// ensureReadyRedisPod creates a Running+Ready Redis pod for cluster with the
// labels getAllRedisPodAddresses selects on. envtest runs no kubelet, so both
// the pod and its status are written by the test.
func ensureReadyRedisPod(name, ns, cluster, ip string) {
	GinkgoHelper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "redguard",
				"app.kubernetes.io/instance":   cluster,
				"app.kubernetes.io/component":  "redis",
				"app.kubernetes.io/managed-by": "redguard-operator",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7-alpine"}},
		},
	}
	err := k8sClient.Create(ctx, pod)
	if apierrors.IsAlreadyExists(err) {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
	} else {
		Expect(err).NotTo(HaveOccurred())
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = ip
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// deleteRedisPods removes every Redis pod of cluster without waiting for a
// kubelet that envtest does not run.
func deleteRedisPods(ns, cluster string) {
	GinkgoHelper()

	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Pod{},
		client.InNamespace(ns),
		client.MatchingLabels{
			"app.kubernetes.io/instance":  cluster,
			"app.kubernetes.io/component": "redis",
		},
		client.GracePeriodSeconds(0),
	)).To(Succeed())

	Eventually(func(g Gomega) {
		pods := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, pods, client.InNamespace(ns),
			client.MatchingLabels{
				"app.kubernetes.io/instance":  cluster,
				"app.kubernetes.io/component": "redis",
			})).To(Succeed())
		g.Expect(pods.Items).To(BeEmpty())
	}).Should(Succeed())
}

var _ = Describe("RedisUser ACL application", func() {
	const (
		ns      = "default"
		cluster = "acl-cluster"
	)

	var (
		factory    *redisfake.Factory
		reconciler *RedisUserReconciler
		addresses  []string
	)

	newACLUser := func(name, username string) *redisv1alpha1.RedisUser {
		return &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   cluster,
				Username:          username,
				PasswordSecretRef: "acl-user-pass",
				ACLRules: redisv1alpha1.ACLRule{
					Categories: []string{"+@read"},
					Keys:       []string{"~app:*"},
				},
			},
		}
	}

	// removeUser deletes the CR and reconciles until it is gone, restoring the
	// pods first so the cleanup path can reach Redis.
	removeUser := func(user *redisv1alpha1.RedisUser) {
		GinkgoHelper()

		key := client.ObjectKeyFromObject(user)
		current := &redisv1alpha1.RedisUser{}
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, current)) {
			return
		}
		for i, addr := range addresses {
			ensureReadyRedisPod(fmt.Sprintf("%s-redis-%d", cluster, i), ns, cluster, hostOf(addr))
		}
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisUser{}))).To(BeTrue())
		}).Should(Succeed())
	}

	BeforeEach(func() {
		ensureTestSentinel(cluster, ns)
		ensureSecret("acl-user-pass", ns, map[string][]byte{"password": []byte("user-password")})

		addresses = []string{"10.244.1.1:6379", "10.244.1.2:6379", "10.244.1.3:6379"}
		for i, addr := range addresses {
			ensureReadyRedisPod(fmt.Sprintf("%s-redis-%d", cluster, i), ns, cluster, hostOf(addr))
		}

		factory = redisfake.NewFactory()
		factory.SetMaster(addresses[0])
		reconciler = &RedisUserReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			RedisFactory: factory,
		}
	})

	AfterEach(func() {
		deleteRedisPods(ns, cluster)
	})

	It("applies and persists the ACL on every ready pod", func() {
		user := newACLUser("acl-everypod", "appuser")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { removeUser(user) })

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		calls := factory.Calls()
		for _, addr := range addresses {
			Expect(calls).To(ContainElement(addr+":ACLSetUser"), "ACL never applied to %s", addr)
			// Without ACL SAVE the user lives only in memory and is gone the
			// next time that pod restarts.
			Expect(calls).To(ContainElement(addr+":ACLSave"), "ACL never persisted on %s", addr)
			Expect(factory.SavedUsers(addr)).To(HaveKey("appuser"),
				"appuser would not survive a restart of %s", addr)
		}

		updated := &redisv1alpha1.RedisUser{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal("Ready"))
		Expect(updated.Status.AppliedTo).To(ConsistOf(addresses))
	})

	It("reports Degraded when a pod did not take the ACL", func() {
		factory.SetError(addresses[2], "ACLSetUser", fmt.Errorf("connection refused"))

		user := newACLUser("acl-partial", "partialuser")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { removeUser(user) })

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		updated := &redisv1alpha1.RedisUser{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal("Degraded"),
			"two of three pods carry the user; reporting Ready hides the gap")
		Expect(updated.Status.AppliedTo).To(ConsistOf(addresses[0], addresses[1]))

		ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))

		degraded := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Message).To(ContainSubstring("2"))
	})

	It("keeps the finalizer when no pod is reachable for cleanup", func() {
		user := newACLUser("acl-nopods", "leaver")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { removeUser(user) })

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("losing every Redis pod, then deleting the user")
		deleteRedisPods(ns, cluster)
		current := &redisv1alpha1.RedisUser{}
		Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero(), "cleanup must be retried, not abandoned")

		still := &redisv1alpha1.RedisUser{}
		Expect(k8sClient.Get(ctx, key, still)).To(Succeed())
		Expect(still.Finalizers).To(ContainElement(redisUserFinalizer),
			"dropping the finalizer here leaves the ACL user alive on every pod that comes back")

		By("bringing a pod back so the cleanup can complete")
		ensureReadyRedisPod(cluster+"-redis-0", ns, cluster, hostOf(addresses[0]))
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisUser{}))).To(BeTrue())
		}).Should(Succeed())

		Expect(factory.Calls()).To(ContainElement(addresses[0] + ":ACLDelUser"))
		// A DELUSER that is not saved comes back on the next restart.
		Expect(factory.SavedUsers(addresses[0])).NotTo(HaveKey("leaver"))
	})

	It("enqueues the cluster's users when a Redis pod becomes ready", func() {
		user := newACLUser("acl-watched", "watcheduser")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { removeUser(user) })

		other := newACLUser("acl-other-cluster", "otheruser")
		other.Spec.RedisClusterRef = "some-other-cluster"
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, other)).To(Succeed()) })

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster + "-redis-0", Namespace: ns}, pod)).To(Succeed())

		// A restarted pod comes back with only what its ACL file held, so the
		// users of that cluster have to be reconciled without waiting for the
		// five-minute resync.
		requests := reconciler.redisUsersForPod(ctx, pod)
		Expect(requests).To(ContainElement(reconcile.Request{
			NamespacedName: types.NamespacedName{Name: user.Name, Namespace: ns},
		}))
		Expect(requests).NotTo(ContainElement(reconcile.Request{
			NamespacedName: types.NamespacedName{Name: other.Name, Namespace: ns},
		}), "a pod of one cluster must not reconcile another cluster's users")

		By("ignoring pods that are not ready")
		notReady := pod.DeepCopy()
		notReady.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		Expect(reconciler.redisUsersForPod(ctx, notReady)).To(BeEmpty())

		By("ignoring pods the operator does not manage")
		foreign := pod.DeepCopy()
		foreign.Labels["app.kubernetes.io/managed-by"] = "someone-else"
		Expect(reconciler.redisUsersForPod(ctx, foreign)).To(BeEmpty())
	})

	It("records ownership before any node holds the account", func() {
		for _, addr := range addresses {
			factory.SetError(addr, "ACLSetUser", fmt.Errorf("connection refused"))
		}
		user := newACLUser("acl-ledger-first", "ledgerfirst")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() {
			for _, addr := range addresses {
				factory.SetError(addr, "ACLSetUser", nil)
			}
			removeUser(user)
		})

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// Every SETUSER failed, so no node knows the account; the record must
		// exist anyway, or a crash between the two writes leaves a live
		// account nothing marks as operator-created, unprunable forever.
		Expect(factory.Users()).NotTo(HaveKey("ledgerfirst"))
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: aclOwnersName(cluster), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue("ledgerfirst", "acl-ledger-first"))
		Expect(cm.OwnerReferences).To(BeEmpty(),
			"an owner reference would garbage-collect the record with the cluster, but the aclfile it describes outlives the cluster on the retained volume")
	})

	It("drops the ownership record when the account is removed from every node", func() {
		user := newACLUser("acl-ledger-drop", "ledgerdrop")
		Expect(k8sClient.Create(ctx, user)).To(Succeed())

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: aclOwnersName(cluster), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue("ledgerdrop", "acl-ledger-drop"))

		removeUser(user)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: aclOwnersName(cluster), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data).NotTo(HaveKey("ledgerdrop"),
			"a record kept after a clean removal would mark a future hand-made account of the same name for deletion")
	})

	It("keeps the ownership record when the cluster is gone at deletion", func() {
		const goneCluster = "acl-gone-cluster"
		ensureTestSentinel(goneCluster, ns)
		ensureReadyRedisPod(goneCluster+"-redis-0", ns, goneCluster, "10.244.2.1")
		DeferCleanup(func() { deleteRedisPods(ns, goneCluster) })

		user := &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: "acl-tombstone", Namespace: ns},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   goneCluster,
				Username:          "tombstoned",
				PasswordSecretRef: "acl-user-pass",
			},
		}
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("deleting the cluster while its retained volume still holds the account")
		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: goneCluster, Namespace: ns}, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		// The managed reconciler removes the cluster finalizer in the background.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: goneCluster, Namespace: ns}, &redisv1alpha1.RedisSentinel{}))
		}, 20*time.Second, 200*time.Millisecond).Should(BeTrue())

		Expect(k8sClient.Delete(ctx, user)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisUser{}))).To(BeTrue())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: aclOwnersName(goneCluster), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue("tombstoned", "acl-tombstone"),
			"the record is the only thing that lets a recreated cluster prune this account")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })
	})

	It("keeps enabled: false across the finalizer write", func() {
		user := newACLUser("acl-disabled", "disableduser")
		user.Spec.Enabled = ptr.To(false)
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { removeUser(user) })

		key := client.ObjectKeyFromObject(user)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		updated := &redisv1alpha1.RedisUser{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Spec.Enabled).NotTo(BeNil())
		Expect(*updated.Spec.Enabled).To(BeFalse(),
			"the finalizer Update round-trip re-enabled a disabled account")

		Expect(factory.Users()).To(HaveKey("disableduser"))
		Expect(factory.Users()["disableduser"]).To(ContainElement("off"))
	})
})

// hostOf returns the host part of a "host:port" dial address.
func hostOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
