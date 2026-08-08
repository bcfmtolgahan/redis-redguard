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
	"slices"
	"strings"
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

// aclCase is one spec.aclRules field and the rules written under it. Validation
// is per field because Redis flattens the four lists into a single rule list and
// honours a grant wherever it appears, while the confinement floor only follows
// two of them.
type aclCase struct {
	field string
	rules []string
}

// TestACLRulesRejectPrivilegeEscalation covers the reconcile-side denylist.
// Redis parses ACL rules case-insensitively and accepts multiple rules inside
// one (...) selector argument, so the obvious bypasses are part of the table.
func TestACLRulesRejectPrivilegeEscalation(t *testing.T) {
	denied := []aclCase{
		{"commands", []string{"nopass"}},
		{"commands", []string{"NOPASS"}},
		{"commands", []string{"NoPass"}},
		{"categories", []string{"+@all"}},
		{"categories", []string{"+@ALL"}},
		{"categories", []string{"+@All"}},
		{"categories", []string{"+@admin"}},
		{"categories", []string{"+@dangerous"}},
		{"commands", []string{"+acl"}},
		{"commands", []string{"+ACL"}},
		{"commands", []string{"+acl|setuser"}},
		{"commands", []string{"+ACL|SETUSER"}},
		{"commands", []string{"+config"}},
		{"commands", []string{"+config|set"}},
		{"commands", []string{"+shutdown"}},
		{"commands", []string{"+debug"}},
		{"commands", []string{"+failover"}},
		{"commands", []string{"+replicaof"}},
		{"commands", []string{"+slaveof"}},
		{"commands", []string{"+module"}},
		{"commands", []string{"+cluster"}},
		{"commands", []string{">otherpassword"}},
		{"commands", []string{"<oldpassword"}},
		{"commands", []string{"#0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		{"commands", []string{"!0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		{"keys", []string{">otherpassword"}},
		{"channels", []string{"nopass"}},
		{"commands", []string{"reset"}},
		{"commands", []string{"resetpass"}},
		{"commands", []string{"on"}},
		{"commands", []string{"off"}},
		{"commands", []string{"allcommands"}},
		{"commands", []string{"ALLCOMMANDS"}},
		{"categories", []string{"allcommands"}},
		{"commands", []string{"+get +@admin"}},
		{"categories", []string{"+@read\t+@admin"}},
		{"commands", []string{"(+@all ~*)"}},
		{"commands", []string{"(+get"}},
		{"commands", []string{""}},
		{"keys", []string{""}},
		// Every entry is checked, not only the first.
		{"keys", []string{"~*", "+@admin"}},
		{"categories", []string{"+@read", "+@admin"}},
	}
	for _, c := range denied {
		if err := validateACLRules(c.field, c.rules); err == nil {
			t.Errorf("validateACLRules(%q, %q) = nil, want privilege-escalation rejection", c.field, c.rules)
		}
	}
}

func TestACLRulesAllowScopedGrants(t *testing.T) {
	allowed := []aclCase{
		{"categories", []string{"+@read"}},
		{"categories", []string{"+@write"}},
		{"categories", []string{"+@keyspace"}},
		{"categories", []string{"+@string", "+@list", "+@set", "+@sortedset", "+@hash", "+@stream"}},
		{"categories", []string{"+@geo", "+@bitmap", "+@hyperloglog"}},
		{"categories", []string{"+@pubsub", "+@transaction", "+@connection", "+@scripting"}},
		{"categories", []string{"-@dangerous"}},
		{"categories", []string{"-@all"}},
		{"categories", []string{"-@admin"}},
		{"commands", []string{"+get", "+set", "-del"}},
		// Commands that carry a key specification stay inside the user's ~scope.
		{"commands", []string{"+dump", "+eval", "+evalsha", "+fcall", "+object|encoding", "+memory|usage"}},
		// Commands that touch neither keys nor channels are scope-neutral.
		{"commands", []string{"+ping", "+auth", "+hello", "+echo", "+command|info", "+client|id"}},
		{"commands", []string{"+subscribe", "+publish", "+psubscribe"}},
		{"commands", []string{"nocommands"}},
		{"commands", []string{"clearselectors", "sanitize-payload", "nosanitize-payload"}},
		{"keys", []string{"~app:*"}},
		{"keys", []string{"~*"}},
		{"keys", []string{"%R~cache:*", "%W~cache:*", "%RW~cache:*"}},
		{"keys", []string{"allkeys"}},
		{"keys", []string{"resetkeys"}},
		{"channels", []string{"&notifications:*"}},
		{"channels", []string{"&*"}},
		{"channels", []string{"allchannels"}},
		{"channels", []string{"resetchannels"}},
	}
	for _, c := range allowed {
		if err := validateACLRules(c.field, c.rules); err != nil {
			t.Errorf("validateACLRules(%q, %q) = %v, want nil", c.field, c.rules, err)
		}
	}
}

// TestACLRulesRejectDataPlaneEscape covers the escapes a denylist misses: a
// command with no key specification runs across the whole keyspace regardless
// of a ~pattern, and the admin/dangerous families read or destroy data beyond
// the user's scope. Redis parses rules case-insensitively, so case variants and
// subcommand forms are part of the table.
func TestACLRulesRejectDataPlaneEscape(t *testing.T) {
	denied := []string{
		"+monitor",   // mirrors every command, including other clients' AUTH
		"+MONITOR",   //
		"+psync",     // streams a full-keyspace RDB
		"+sync",      //
		"+replconf",  //
		"+flushall",  // wipes the keyspace despite ~app:*
		"+flushdb",   //
		"+swapdb",    //
		"+FlushAll",  //
		"+keys",      // enumerates every key name past ~app:*
		"+scan",      //
		"+randomkey", //
		"+dbsize",    // counts the whole keyspace
		"+migrate",   // exfiltrates keys to an arbitrary external host
		"+function",  // registers global server-side functions
		"+function|load",
		"+script", // loads global server-side scripts
		"+script|load",
		"+bgsave",
		"+save",
		"+restore",         // arbitrary payload deserialization
		"+sort",            // BY/GET/STORE reach keys outside the ~pattern
		"+sort_ro",         //
		"+wait",            // no key specification
		"+lolwut",          //
		"+object",          // bare container grants object|help and every sub
		"+client",          // bare container grants client|no-evict and kill
		"+client|no-evict", //
		"+pubsub",          // enumerates channel names past &scope
		"+pubsub|channels", //
		"+cluster|nodes",   // discloses cluster topology
		"+notacommand",     // unknown command
	}
	for _, rule := range denied {
		if err := validateACLRule("commands", rule); err == nil {
			t.Errorf("validateACLRule(\"commands\", %q) = nil, want data-plane escape rejection", rule)
		}
	}
	if err := validateACLRule("categories", "+@nonsense"); err == nil {
		t.Error("validateACLRule(\"categories\", \"+@nonsense\") = nil, want unknown-category rejection")
	}
}

// TestACLRulesRejectGrantsInTheWrongField closes the field-placement bypass.
// Redis flattens the four lists into one SETUSER rule list, so a grant written
// under keys or channels takes effect exactly as if it had been written under
// commands — but the confinement floor only follows the category and command
// fields, so such a grant lands unconfined. Every field is covered in both
// directions.
func TestACLRulesRejectGrantsInTheWrongField(t *testing.T) {
	misplaced := []struct {
		field string
		rule  string
	}{
		// The reported escape and its channel twin.
		{"keys", "+@read"},
		{"channels", "+@read"},
		{"keys", "+@write"},
		{"keys", "+get"},
		{"channels", "+get"},
		{"keys", "-@dangerous"},
		{"keys", "&events:*"},
		{"keys", "allchannels"},
		{"keys", "resetchannels"},
		{"keys", "nocommands"},
		{"channels", "~app:*"},
		{"channels", "%R~cache:*"},
		{"channels", "allkeys"},
		{"channels", "resetkeys"},
		{"channels", "nocommands"},
		{"categories", "+get"},
		{"categories", "-del"},
		{"categories", "~app:*"},
		{"categories", "&events:*"},
		{"categories", "allkeys"},
		{"categories", "allchannels"},
		{"categories", "nocommands"},
		{"commands", "+@read"},
		{"commands", "-@dangerous"},
		{"commands", "~app:*"},
		{"commands", "&events:*"},
		{"commands", "allkeys"},
		{"commands", "resetchannels"},
		// Malformed patterns in their own field.
		{"keys", "~"},
		{"keys", "%X~app:*"},
		{"keys", "%app:*"},
		{"channels", "&"},
	}
	for _, m := range misplaced {
		if err := validateACLRule(m.field, m.rule); err == nil {
			t.Errorf("validateACLRule(%q, %q) = nil, want field-placement rejection", m.field, m.rule)
		}
	}
}

// TestSpecRejectsGrantsSmuggledThroughKeysOrChannels pins the whole-spec view of
// the bypass: "+@read" under keys is a real command grant that reaches the
// SETUSER rule list, and gating the confinement floor on the category and
// command fields leaves KEYS, SCAN and FLUSHALL re-enabled outside the declared
// key scope.
func TestSpecRejectsGrantsSmuggledThroughKeysOrChannels(t *testing.T) {
	specs := map[string]redisv1alpha1.ACLRule{
		"category grant under keys":     {Keys: []string{"+@read", "~*"}},
		"category grant under channels": {Channels: []string{"+@read", "&*"}},
		"command grant under keys":      {Keys: []string{"~app:*", "+get"}},
		"command grant under channels":  {Channels: []string{"&app:*", "+get"}},
		"key pattern under categories":  {Categories: []string{"~*"}},
		"key pattern under commands":    {Commands: []string{"~*"}},
	}
	for name, rules := range specs {
		spec := &redisv1alpha1.RedisUserSpec{Username: "appuser", ACLRules: rules}
		if err := validateRedisUserSpec(spec); err == nil {
			t.Errorf("%s: validateRedisUserSpec(%+v) = nil, want field-placement rejection", name, rules)
		}
	}
}

// TestACLRuleRejectionNamesRuleAndRemedy pins the operator contract: a rejected
// rule is quoted verbatim and the message points at what is permitted instead.
func TestACLRuleRejectionNamesRuleAndRemedy(t *testing.T) {
	err := validateACLRule("commands", "+flushall")
	if err == nil {
		t.Fatal("validateACLRule(\"commands\", \"+flushall\") = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "+flushall") {
		t.Errorf("message %q does not name the offending rule", err)
	}
	if !strings.Contains(err.Error(), "+@read") {
		t.Errorf("message %q does not say what is permitted instead", err)
	}
}

// A misplaced rule is only actionable if the message says where it belongs.
func TestMisplacedACLRuleNamesRuleAndBothFields(t *testing.T) {
	err := validateACLRule("keys", "+@read")
	if err == nil {
		t.Fatal("validateACLRule(\"keys\", \"+@read\") = nil, want rejection")
	}
	for _, want := range []string{"+@read", "keys", "categories"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

// TestBuildACLRulesAppendsConfinementFloor pins the defense-in-depth layer:
// a category grant carries commands with no key specification, so the rendered
// rules must end with the removals that pull them back inside the user's scope.
func TestBuildACLRulesAppendsConfinementFloor(t *testing.T) {
	user := &redisv1alpha1.RedisUser{
		Spec: redisv1alpha1.RedisUserSpec{
			Username: "appuser",
			ACLRules: redisv1alpha1.ACLRule{
				Categories: []string{"+@read", "+@write"},
				Keys:       []string{"~app:*"},
			},
		},
	}
	rules := buildACLRules(user, "secret")

	for _, floor := range redisConfinementFloor {
		if !slices.Contains(rules, floor) {
			t.Errorf("rendered rules %q missing confinement token %q", rules, floor)
		}
	}
	// The floor must win over the category grants, so it comes last.
	firstFloor := slices.Index(rules, redisConfinementFloor[0])
	lastGrant := slices.Index(rules, "+@write")
	if firstFloor < lastGrant {
		t.Errorf("confinement floor precedes the category grants; Redis applies rules left to right, so the removals would not win: %q", rules)
	}
}

// TestBuildACLRulesNoFloorWithoutGrants keeps an unconfigured user minimal: with
// nothing granted there is nothing to confine, so no removals are emitted.
func TestBuildACLRulesNoFloorWithoutGrants(t *testing.T) {
	user := &redisv1alpha1.RedisUser{
		Spec: redisv1alpha1.RedisUserSpec{Username: "appuser"},
	}
	rules := buildACLRules(user, "secret")
	for _, floor := range redisConfinementFloor {
		if slices.Contains(rules, floor) {
			t.Errorf("empty aclRules emitted confinement token %q; rules %q", floor, rules)
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

		It("degrades a data-plane escape and names the offending rule", func() {
			ensureTestSentinel("test-cluster", ns)
			ensureSecret("user-pass", ns, map[string][]byte{"password": []byte("pw")})

			// "+monitor" is a single clean token that clears the structural CRD
			// checks. It is not administrative, so a denylist of admin commands
			// would miss it, yet it mirrors every command on the server.
			user := newUser("rec-monitor")
			user.Spec.ACLRules.Commands = []string{"+monitor"}
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
			Expect(cond.Message).To(ContainSubstring("+monitor"))
			Expect(cond.Message).To(ContainSubstring("+@read"), "the message must say what is permitted instead")

			Expect(factory.Calls()).To(BeEmpty(), "a rejected spec must never reach Redis")

			ensureReadyRedisPod("test-cluster-redis-0", ns, "test-cluster", "10.244.5.2")
			defer deleteRedisPods(ns, "test-cluster")

			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisUser{}))).To(BeTrue())
			}).Should(Succeed())
		})

		It("degrades a grant smuggled through spec.aclRules.keys", func() {
			ensureTestSentinel("test-cluster", ns)
			ensureSecret("user-pass", ns, map[string][]byte{"password": []byte("pw")})

			// "+@read" under keys is a single clean token that clears the CRD
			// checks and reaches SETUSER as a real command grant, while the
			// confinement floor follows only the category and command fields.
			user := newUser("rec-keysgrant")
			user.Spec.ACLRules.Keys = []string{"+@read", "~*"}
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
			Expect(cond.Message).To(ContainSubstring("+@read"))
			Expect(cond.Message).To(ContainSubstring("keys"))
			Expect(cond.Message).To(ContainSubstring("categories"), "the message must name the field the rule belongs in")

			Expect(factory.Calls()).To(BeEmpty(), "a rejected spec must never reach Redis")

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
