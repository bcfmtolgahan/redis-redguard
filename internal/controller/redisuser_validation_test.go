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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// TestACLRulesRejectPrivilegeEscalation covers the reconcile-side denylist.
// Redis parses ACL rules case-insensitively and accepts multiple rules inside
// one (...) selector argument, so the obvious bypasses are part of the table.
func TestACLRulesRejectPrivilegeEscalation(t *testing.T) {
	denied := [][]string{
		{"nopass"},
		{"NOPASS"},
		{"NoPass"},
		{"+@all"},
		{"+@ALL"},
		{"+@All"},
		{"+@admin"},
		{"+@dangerous"},
		{"+acl"},
		{"+ACL"},
		{"+acl|setuser"},
		{"+ACL|SETUSER"},
		{"+config"},
		{"+config|set"},
		{"+shutdown"},
		{"+debug"},
		{"+failover"},
		{"+replicaof"},
		{"+slaveof"},
		{"+module"},
		{"+cluster"},
		{">otherpassword"},
		{"<oldpassword"},
		{"#0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"!0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"reset"},
		{"resetpass"},
		{"on"},
		{"off"},
		{"allcommands"},
		{"ALLCOMMANDS"},
		{"+get +@admin"},
		{"+@read\t+@admin"},
		{"(+@all ~*)"},
		{"(+get"},
		{""},
		{"~*", "+@admin"},
	}
	for _, rules := range denied {
		if err := validateACLRules(rules); err == nil {
			t.Errorf("validateACLRules(%q) = nil, want privilege-escalation rejection", rules)
		}
	}
}

func TestACLRulesAllowScopedGrants(t *testing.T) {
	allowed := [][]string{
		{"+@read"},
		{"+@write"},
		{"-@dangerous"},
		{"-@all"},
		{"+get", "+set", "-del"},
		{"~app:*"},
		{"~*"},
		{"%R~cache:*"},
		{"&notifications:*"},
		{"&*"},
		{"allkeys"},
		{"allchannels"},
		{"resetkeys"},
		{"resetchannels"},
	}
	for _, rules := range allowed {
		if err := validateACLRules(rules); err != nil {
			t.Errorf("validateACLRules(%q) = %v, want nil", rules, err)
		}
	}
}

// TestUsernameDefaultIsRejected covers the reconcile-side re-check of the CRD
// constraints. Redis usernames are case-sensitive, so only the exact admin
// account name 'default' is dangerous.
func TestUsernameDefaultIsRejected(t *testing.T) {
	if err := validateUsername("default"); err == nil {
		t.Error("validateUsername(\"default\") = nil; SETUSER on 'default' resets the cluster password")
	}
	for _, name := range []string{"", "bad user", "bad\nuser", "-leadingdash", ".leadingdot"} {
		if err := validateUsername(name); err == nil {
			t.Errorf("validateUsername(%q) = nil, want pattern rejection", name)
		}
	}
	for _, name := range []string{"appuser", "app.user-1", "Default", "A"} {
		if err := validateUsername(name); err != nil {
			t.Errorf("validateUsername(%q) = %v, want nil", name, err)
		}
	}
}

var _ = Describe("RedisUser validation", func() {
	const ns = "default"

	newUser := func(name string) *redisv1alpha1.RedisUser {
		return &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   "test-cluster",
				Username:          "appuser",
				PasswordSecretRef: "user-pass",
				Enabled:           ptr.To(true),
			},
		}
	}

	Context("at admission", func() {
		It("rejects username 'default'", func() {
			user := newUser("adm-default")
			user.Spec.Username = "default"

			err := k8sClient.Create(ctx, user)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("reserved"))
		})

		It("rejects usernames outside the allowed pattern", func() {
			user := newUser("adm-badname")
			user.Spec.Username = "bad user"

			Expect(k8sClient.Create(ctx, user)).NotTo(Succeed())
		})

		It("rejects ACL rules containing whitespace or selector syntax", func() {
			multiToken := newUser("adm-multitoken")
			multiToken.Spec.ACLRules.Categories = []string{"+@read +@admin"}
			Expect(k8sClient.Create(ctx, multiToken)).NotTo(Succeed())

			selector := newUser("adm-selector")
			selector.Spec.ACLRules.Commands = []string{"(+@all ~*)"}
			Expect(k8sClient.Create(ctx, selector)).NotTo(Succeed())
		})

		It("accepts a well-formed user", func() {
			user := newUser("adm-ok")
			user.Spec.ACLRules = redisv1alpha1.ACLRule{
				Categories: []string{"+@read"},
				Keys:       []string{"~app:*"},
			}

			Expect(k8sClient.Create(ctx, user)).To(Succeed())
			Expect(k8sClient.Delete(ctx, user)).To(Succeed())
		})
	})

	Context("at reconcile", func() {
		It("degrades an escalating spec without dialing Redis", func() {
			ensureTestSentinel("test-cluster", ns)
			ensureSecret("user-pass", ns, map[string][]byte{"password": []byte("pw")})

			// "+@all" is a single clean token, so it passes the structural CRD
			// checks; only the reconcile-side denylist can stop it.
			user := newUser("rec-escalator")
			user.Spec.ACLRules.Categories = []string{"+@all"}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			factory := redisfake.NewFactory()
			reconciler := &RedisUserReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				RedisFactory: factory,
			}
			key := types.NamespacedName{Name: user.Name, Namespace: ns}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			updated := &redisv1alpha1.RedisUser{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Degraded"))

			cond := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("+@all"))

			Expect(factory.Calls()).To(BeEmpty(), "a rejected spec must never reach Redis")

			// Deletion holds the finalizer until the ACL user has been removed
			// from a reachable pod, so the cleanup path needs one.
			ensureReadyRedisPod("test-cluster-redis-0", ns, "test-cluster", "10.244.5.2")
			defer deleteRedisPods(ns, "test-cluster")

			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisUser{}))).To(BeTrue())
			}).Should(Succeed())
		})
	})
})
