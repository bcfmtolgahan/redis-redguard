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
	"crypto/tls"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/tlsutil"
	custmetrics "github.com/redguard/redguard/pkg/metrics"
)

const redisUserFinalizer = "redis.redguard.io/redisuser-finalizer"

// userRetryInterval paces a retry after a failure that is recorded on the CR
// rather than returned, so that controller-runtime keeps the delay instead of
// replacing it with rate-limited backoff.
const userRetryInterval = 30 * time.Second

// RedisUserReconciler reconciles a RedisUser object
type RedisUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RedisFactory builds Redis clients; nil means DefaultFactory.
	RedisFactory redisclient.Factory
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *RedisUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the RedisUser instance
	redisUser := &redisv1alpha1.RedisUser{}
	if err := r.Get(ctx, req.NamespacedName, redisUser); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisUser")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !redisUser.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, redisUser)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(redisUser, redisUserFinalizer) {
		controllerutil.AddFinalizer(redisUser, redisUserFinalizer)
		if err := r.Update(ctx, redisUser); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Admission enforces the same constraints via CRD validation; this re-check
	// keeps a stale CRD from letting a hijacking spec through. The failure is
	// terminal until the spec is edited, so no requeue follows.
	if err := validateRedisUserSpec(&redisUser.Spec); err != nil {
		logger.Error(err, "Rejecting invalid RedisUser spec")
		r.setDegraded(ctx, redisUser, "SpecRejected", err.Error())
		return ctrl.Result{}, nil
	}

	// Every failure below is reported on the CR and retried after
	// userRetryInterval. Returning the error instead would make
	// controller-runtime discard the interval and retry through the rate
	// limiter, which starts at 5ms.

	// Get the RedisSentinel cluster
	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisUser.Spec.RedisClusterRef,
		Namespace: redisUser.Namespace,
	}, redisSentinel); err != nil {
		logger.Error(err, "Failed to get RedisSentinel cluster")
		r.updateStatus(ctx, redisUser, "Error", nil,
			fmt.Sprintf("RedisSentinel %q not readable: %v", redisUser.Spec.RedisClusterRef, err))
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	// Get password from secret
	password, err := r.getPasswordFromSecret(ctx, redisUser)
	if err != nil {
		logger.Error(err, "Failed to get password from secret")
		r.updateStatus(ctx, redisUser, "Error", nil, fmt.Sprintf("Password secret not readable: %v", err))
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	// Get all Redis pod addresses (ACL must be applied to all nodes, not just master)
	allAddresses, err := r.getAllRedisPodAddresses(ctx, redisSentinel)
	if err != nil {
		logger.Error(err, "Failed to get Redis pod addresses")
		r.updateStatus(ctx, redisUser, "Error", nil, fmt.Sprintf("Redis pods not listable: %v", err))
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	if len(allAddresses) == 0 {
		logger.Info("No running Redis pods found, will retry")
		r.updateStatus(ctx, redisUser, "Error", nil, "No running Redis pods")
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	// Get Redis admin password for connection
	adminPassword := ""
	if redisSentinel.Spec.RedisConfig.Auth != nil && redisSentinel.Spec.RedisConfig.Auth.SecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      redisSentinel.Spec.RedisConfig.Auth.SecretName,
			Namespace: redisSentinel.Namespace,
		}, secret); err == nil {
			adminPassword = string(secret.Data["password"])
		}
	}

	// Resolved once, reused for every pod below.
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		logger.Error(err, "Failed to resolve client TLS config")
		r.updateStatus(ctx, redisUser, "Error", nil, err.Error())
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	// ACL commands are not replicated, so every node needs its own copy for the
	// user to survive a failover.
	appliedTo := []string{}
	var lastErr error
	for _, addr := range allAddresses {
		redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
		if err := r.applyACL(ctx, redisClient, redisUser, password); err != nil {
			logger.Error(err, "Failed to apply ACL to pod", "address", addr)
			lastErr = err
		} else {
			appliedTo = append(appliedTo, addr)
			logger.Info("Applied ACL to pod", "address", addr, "username", redisUser.Spec.Username)
		}
		redisClient.Close()
	}

	// Anything short of every ready pod means part of the cluster does not know
	// the user, which a Ready status would hide.
	if len(appliedTo) < len(allAddresses) {
		message := fmt.Sprintf("ACL applied to %d of %d ready Redis pods; last error: %v",
			len(appliedTo), len(allAddresses), lastErr)
		logger.Info("ACL application incomplete, will retry", "applied", len(appliedTo), "total", len(allAddresses))
		redisUser.Status.AppliedTo = appliedTo
		r.setDegraded(ctx, redisUser, "ACLPartiallyApplied", message)
		return ctrl.Result{RequeueAfter: userRetryInterval}, nil
	}

	// Update status
	r.updateStatus(ctx, redisUser, "Ready", appliedTo, "")

	logger.Info("Successfully reconciled RedisUser", "username", redisUser.Spec.Username)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// handleDeletion removes the ACL user from every ready Redis pod before it lets
// the object go. The finalizer stays whenever the removal could not be carried
// out on all of them: dropping it early leaves working credentials behind on
// every node that was unreachable at that moment.
func (r *RedisUserReconciler) handleDeletion(ctx context.Context, redisUser *redisv1alpha1.RedisUser) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// The CR is doomed from here, so its series go now rather than lingering
	// as a permanent ACL alert for an object that no longer exists.
	custmetrics.DeleteUserSeries(redisUser.Namespace, redisUser.Name)

	if !controllerutil.ContainsFinalizer(redisUser, redisUserFinalizer) {
		return ctrl.Result{}, nil
	}

	// A rejected spec was never applied, so there is nothing to remove; issuing
	// ACL DELUSER for such a name ('default' is the admin account) must never
	// happen.
	if err := validateUsername(redisUser.Spec.Username); err != nil {
		logger.Info("Skipping ACL cleanup for a username the operator refuses to manage")
		return r.removeFinalizer(ctx, redisUser)
	}

	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisUser.Spec.RedisClusterRef,
		Namespace: redisUser.Namespace,
	}, redisSentinel); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get RedisSentinel for ACL cleanup")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		// The cluster is gone, so no server is left holding the account.
		return r.removeFinalizer(ctx, redisUser)
	}

	allAddresses, err := r.getAllRedisPodAddresses(ctx, redisSentinel)
	if err != nil {
		logger.Error(err, "Failed to list Redis pods for ACL cleanup")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if len(allAddresses) == 0 {
		logger.Info("No ready Redis pod to remove the ACL user from; keeping the finalizer",
			"username", redisUser.Spec.Username)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	adminPassword := ""
	if redisSentinel.Spec.RedisConfig.Auth != nil && redisSentinel.Spec.RedisConfig.Auth.SecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      redisSentinel.Spec.RedisConfig.Auth.SecretName,
			Namespace: redisSentinel.Namespace,
		}, secret); err == nil {
			adminPassword = string(secret.Data["password"])
		}
	}

	// Without the TLS settings no pod is reachable, so retry the cleanup later
	// rather than dial plaintext.
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		logger.Error(err, "Cannot resolve client TLS config for ACL cleanup, will retry")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	failedPods := []string{}
	for _, addr := range allAddresses {
		if err := r.deleteACL(ctx, addr, adminPassword, tlsCfg, redisUser.Spec.Username); err != nil {
			logger.Error(err, "Failed to remove ACL user from pod",
				"username", redisUser.Spec.Username, "address", addr)
			failedPods = append(failedPods, addr)
			continue
		}
		logger.Info("Removed ACL user from pod", "username", redisUser.Spec.Username, "address", addr)
	}
	if len(failedPods) > 0 {
		logger.Info("ACL cleanup incomplete, will retry", "failedPods", failedPods)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return r.removeFinalizer(ctx, redisUser)
}

func (r *RedisUserReconciler) removeFinalizer(ctx context.Context, redisUser *redisv1alpha1.RedisUser) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(redisUser, redisUserFinalizer)
	if err := r.Update(ctx, redisUser); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("Finalizer removed, RedisUser cleanup complete", "username", redisUser.Spec.Username)
	return ctrl.Result{}, nil
}

// deleteACL removes the user from one node and persists the removal. Without
// the save the account is restored from the ACL file on the next restart.
func (r *RedisUserReconciler) deleteACL(ctx context.Context, addr, adminPassword string, tlsCfg *tls.Config, username string) error {
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	// An already absent user is the desired end state, not a failure.
	if err := redisClient.ACLDelUser(ctx, username); err != nil && !strings.Contains(err.Error(), "ERR The user") {
		return err
	}
	if err := redisClient.ACLSave(ctx); err != nil {
		return fmt.Errorf("persisting ACL removal: %w", err)
	}
	return nil
}

// usernamePattern mirrors the CRD's Pattern marker so a stale CRD cannot let
// an unvalidated name through.
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validateUsername re-checks the CRD's username constraints at reconcile.
// Redis usernames are case-sensitive, so only the exact name 'default' is the
// admin account.
func validateUsername(username string) error {
	if username == "default" {
		return fmt.Errorf("username %q is reserved: it is the Redis admin account whose password is requirepass, and redefining it would reset the cluster password and lock out the operator", username)
	}
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("username %q must match %s", username, usernamePattern)
	}
	return nil
}

// redisConfinementFloor is appended after a user's positive grants. A +@category
// grant bundles commands that carry no key specification and so ignore any
// ~pattern: @read pulls in KEYS/SCAN/RANDOMKEY (full key-name enumeration),
// @write pulls in FLUSHALL/FLUSHDB/SWAPDB (keyspace wipe), and every category
// can reach the @admin/@dangerous families. Redis applies rules left to right,
// so these trailing removals win and hold the user inside its declared key and
// channel scope. -pubsub drops the channel-name enumerators while leaving
// SUBSCRIBE/PUBLISH; -function and -script drop server-global state management
// while leaving the key-scoped EVAL/FCALL.
var redisConfinementFloor = []string{
	"-@admin", "-@dangerous",
	"-scan", "-randomkey", "-dbsize",
	"-pubsub", "-function", "-script",
}

// deniedACLKeywords are standalone rules that weaken authentication or fight
// the fields the operator renders itself (password, on/off, reset).
var deniedACLKeywords = map[string]string{
	"nopass":      "it creates a passwordless account",
	"reset":       "it discards the operator-managed password and state",
	"resetpass":   "it discards the operator-managed password",
	"on":          "user activation is controlled by spec.enabled",
	"off":         "user activation is controlled by spec.enabled",
	"allcommands": "it grants every command, including administrative ones",
}

// The spec.aclRules fields. Redis flattens all four into one SETUSER rule list
// and honours a rule wherever it appears, so the grouping is not enforced by
// the server: a grant written under keys or channels is a real grant, and the
// confinement floor below follows only the category and command fields. Each
// field is therefore held to its own rule shape.
const (
	aclFieldCategories = "categories"
	aclFieldCommands   = "commands"
	aclFieldKeys       = "keys"
	aclFieldChannels   = "channels"
)

// aclScopeKeywords are the standalone rules each field accepts beside its
// pattern or grant syntax. None can widen access beyond the user's own scope.
// The sets are disjoint, so a keyword names exactly one field.
var aclScopeKeywords = map[string]map[string]struct{}{
	aclFieldKeys:     {"allkeys": {}, "resetkeys": {}},
	aclFieldChannels: {"allchannels": {}, "resetchannels": {}},
	// clearselectors and the payload flags change how granted commands execute,
	// so they ride with the command grants.
	aclFieldCommands: {
		"nocommands":         {},
		"clearselectors":     {},
		"sanitize-payload":   {},
		"nosanitize-payload": {},
	},
}

// aclFieldShapes says what each field accepts, for the rejection message.
var aclFieldShapes = map[string]string{
	aclFieldCategories: "a category grant such as +@read or -@dangerous",
	aclFieldCommands:   "a command grant such as +get, -del or nocommands",
	aclFieldKeys:       "a key pattern such as ~app:*, %R~cache:*, allkeys or resetkeys",
	aclFieldChannels:   "a channel pattern such as &events:*, allchannels or resetchannels",
}

// validateACLRules confines a RedisUser to its declared scope. A denylist is not
// enough: Redis adds commands over time and every future one would default to
// permitted, and the escape vectors are open-ended (KEYS, SCAN, FLUSHALL,
// MONITOR, PSYNC, MIGRATE, ...). So command grants are checked against an
// allowlist derived from Redis: a command may be granted only if it carries a
// key specification, so a ~pattern confines it, or it touches neither keys nor
// channels. Category grants are limited to the non-administrative categories and
// then narrowed by the confinement floor at render time. Every rule is checked
// against the shape of the field it was written in as well, because the server
// ignores that grouping while the floor depends on it.
func validateACLRules(field string, rules []string) error {
	for _, rule := range rules {
		if err := validateACLRule(field, rule); err != nil {
			return err
		}
	}
	return nil
}

func validateACLRule(field, rule string) error {
	if rule == "" {
		return fmt.Errorf("aclRules.%s: empty rule", field)
	}
	if strings.ContainsFunc(rule, unicode.IsSpace) {
		return fmt.Errorf("aclRules.%s: rule %q must be a single token", field, rule)
	}
	if strings.ContainsAny(rule, "()") {
		return fmt.Errorf("aclRules.%s: rule %q: selector syntax is not allowed", field, rule)
	}
	switch rule[0] {
	case '>', '<', '#', '!':
		return fmt.Errorf("aclRules.%s: rule %q: passwords are managed via passwordSecretRef", field, rule)
	}
	norm := strings.ToLower(rule)
	if why, ok := deniedACLKeywords[norm]; ok {
		return fmt.Errorf("aclRules.%s: rule %q is not allowed: %s", field, rule, why)
	}
	if _, ok := aclScopeKeywords[field][norm]; ok {
		return nil
	}
	switch field {
	case aclFieldKeys:
		return validateKeyPattern(rule, norm)
	case aclFieldChannels:
		return validateChannelPattern(rule, norm)
	case aclFieldCategories:
		return validateCategoryRule(rule, norm)
	case aclFieldCommands:
		return validateCommandRule(rule, norm)
	}
	return fmt.Errorf("aclRules: unknown field %q", field)
}

// aclFieldFor reports the field a rule token belongs in, so a misplaced rule is
// rejected with the move to make instead of a generic parse error. It reports ""
// for a token that fits no field.
func aclFieldFor(norm string) string {
	for field, keywords := range aclScopeKeywords {
		if _, ok := keywords[norm]; ok {
			return field
		}
	}
	switch {
	case strings.HasPrefix(norm, "~"), strings.HasPrefix(norm, "%"):
		return aclFieldKeys
	case strings.HasPrefix(norm, "&"):
		return aclFieldChannels
	case strings.HasPrefix(norm, "+@"), strings.HasPrefix(norm, "-@"):
		return aclFieldCategories
	case strings.HasPrefix(norm, "+"), strings.HasPrefix(norm, "-"):
		return aclFieldCommands
	}
	return ""
}

func errACLFieldMismatch(field, rule, norm string) error {
	if want := aclFieldFor(norm); want != "" && want != field {
		return fmt.Errorf("aclRules.%s: rule %q belongs in spec.aclRules.%s; %s accepts only %s",
			field, rule, want, field, aclFieldShapes[field])
	}
	return fmt.Errorf("aclRules.%s: rule %q is not %s", field, rule, aclFieldShapes[field])
}

// validateKeyPattern accepts only a key-scope declaration: ~pattern or its
// %R~/%W~/%RW~ qualified forms. A grant here would reach SETUSER with nothing
// confining it, since the floor trails the category and command fields only.
func validateKeyPattern(rule, norm string) error {
	var pattern string
	switch {
	case strings.HasPrefix(norm, "~"):
		pattern = norm[1:]
	case strings.HasPrefix(norm, "%"):
		flags, rest, found := strings.Cut(norm[1:], "~")
		if !found {
			return fmt.Errorf("aclRules.%s: rule %q: a %% qualifier must be followed by ~pattern", aclFieldKeys, rule)
		}
		if flags != "r" && flags != "w" && flags != "rw" {
			return fmt.Errorf("aclRules.%s: rule %q: only %%R~, %%W~ and %%RW~ qualify a key pattern", aclFieldKeys, rule)
		}
		pattern = rest
	default:
		return errACLFieldMismatch(aclFieldKeys, rule, norm)
	}
	if pattern == "" {
		return fmt.Errorf("aclRules.%s: rule %q: empty key pattern", aclFieldKeys, rule)
	}
	return nil
}

// validateChannelPattern accepts only &pattern, for the same reason.
func validateChannelPattern(rule, norm string) error {
	pattern, ok := strings.CutPrefix(norm, "&")
	if !ok {
		return errACLFieldMismatch(aclFieldChannels, rule, norm)
	}
	if pattern == "" {
		return fmt.Errorf("aclRules.%s: rule %q: empty channel pattern", aclFieldChannels, rule)
	}
	return nil
}

// validateCategoryRule accepts +@category only for a non-administrative category
// Redis defines; the confinement floor strips the keyspace-wide and
// administrative commands the category still bundles, so an accepted category
// stays inside the user's scope. A -@category can only remove permission.
func validateCategoryRule(rule, norm string) error {
	if negated, ok := strings.CutPrefix(norm, "-@"); ok {
		if negated == "" {
			return fmt.Errorf("aclRules.%s: rule %q names no category", aclFieldCategories, rule)
		}
		return nil
	}
	category, ok := strings.CutPrefix(norm, "+@")
	if !ok {
		return errACLFieldMismatch(aclFieldCategories, rule, norm)
	}
	switch category {
	case "all", "admin", "dangerous":
		return fmt.Errorf("aclRules.%s: rule %q grants administrative or unscoped control; grant a data category such as +@read or +@write instead", aclFieldCategories, rule)
	}
	if _, ok := redisACLCategories[category]; !ok {
		return fmt.Errorf("aclRules.%s: rule %q names an unknown ACL category; grant a data category such as +@read or +@write instead", aclFieldCategories, rule)
	}
	return nil
}

// validateCommandRule accepts +command only for a command Redis confines to a
// key scope or that touches no keys or channels. Everything else runs
// server-wide or outside the user's scope. A -command can only remove
// permission.
func validateCommandRule(rule, norm string) error {
	if revoked, ok := strings.CutPrefix(norm, "-"); ok && !strings.HasPrefix(norm, "-@") {
		if revoked == "" {
			return fmt.Errorf("aclRules.%s: rule %q names no command", aclFieldCommands, rule)
		}
		return nil
	}
	command, ok := strings.CutPrefix(norm, "+")
	if !ok || strings.HasPrefix(norm, "+@") {
		return errACLFieldMismatch(aclFieldCommands, rule, norm)
	}
	if _, ok := redisAllowedCommands[command]; ok {
		return nil
	}
	return fmt.Errorf("aclRules.%s: rule %q is not permitted: %s has no key specification and runs across the whole keyspace or the server, escaping the user's scope; grant a scoped category such as +@read or a key-scoped command such as +get instead", aclFieldCommands, rule, command)
}

// validateRedisUserSpec re-checks at reconcile what the CRD rejects at
// admission, so a stale CRD cannot smuggle through a spec that hijacks the
// 'default' admin account or escalates its ACL grants.
func validateRedisUserSpec(spec *redisv1alpha1.RedisUserSpec) error {
	if err := validateUsername(spec.Username); err != nil {
		return err
	}
	for _, f := range []struct {
		name  string
		rules []string
	}{
		{aclFieldCategories, spec.ACLRules.Categories},
		{aclFieldCommands, spec.ACLRules.Commands},
		{aclFieldKeys, spec.ACLRules.Keys},
		{aclFieldChannels, spec.ACLRules.Channels},
	} {
		if err := validateACLRules(f.name, f.rules); err != nil {
			return err
		}
	}
	return nil
}

// applyACL writes the user to one node and persists it to that node's ACL file.
// Skipping the save leaves the account in memory only, so the next restart of
// that pod comes back without it.
func (r *RedisUserReconciler) applyACL(ctx context.Context, redisClient redisclient.Client, redisUser *redisv1alpha1.RedisUser, password string) error {
	if err := redisClient.ACLSetUser(ctx, redisUser.Spec.Username, buildACLRules(redisUser, password)...); err != nil {
		return err
	}
	if err := redisClient.ACLSave(ctx); err != nil {
		return fmt.Errorf("persisting ACL: %w", err)
	}
	return nil
}

// buildACLRules renders the spec as one ACL SETUSER rule list. It opens with
// 'reset' so the account ends up exactly as the spec describes, and it adds no
// implicit grant: a user that asks for no keys, channels or commands reaches
// none of them.
func buildACLRules(redisUser *redisv1alpha1.RedisUser, password string) []string {
	rules := []string{"reset", ">" + password}

	if redisUser.Spec.Enabled == nil || *redisUser.Spec.Enabled {
		rules = append(rules, "on")
	} else {
		rules = append(rules, "off")
	}

	rules = append(rules, redisUser.Spec.ACLRules.Categories...)
	rules = append(rules, redisUser.Spec.ACLRules.Commands...)
	rules = append(rules, redisUser.Spec.ACLRules.Keys...)
	rules = append(rules, redisUser.Spec.ACLRules.Channels...)

	// A category or command grant can pull in commands that ignore ~patterns.
	// The floor removes them last, after the grants, so it wins. With nothing
	// granted there is nothing to confine. Gating on these two fields is only
	// sound because per-field validation keeps grants out of keys and channels.
	if len(redisUser.Spec.ACLRules.Categories) > 0 || len(redisUser.Spec.ACLRules.Commands) > 0 {
		rules = append(rules, redisConfinementFloor...)
	}

	return rules
}

func (r *RedisUserReconciler) getPasswordFromSecret(ctx context.Context, redisUser *redisv1alpha1.RedisUser) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisUser.Spec.PasswordSecretRef,
		Namespace: redisUser.Namespace,
	}, secret); err != nil {
		return "", err
	}

	password, ok := secret.Data["password"]
	if !ok {
		return "", fmt.Errorf("password key not found in secret")
	}

	return string(password), nil
}

// getAllRedisPodAddresses returns all running Redis pod addresses
// ACL rules must be applied to all nodes to ensure they persist after failover
func (r *RedisUserReconciler) getAllRedisPodAddresses(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) ([]string, error) {
	// List all Redis pods
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(redisSentinel.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/instance":  redisSentinel.Name,
			"app.kubernetes.io/component": "redis",
		},
	}

	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, err
	}

	addresses := []string{}
	for i := range podList.Items {
		if redisPodIsReady(&podList.Items[i]) {
			addresses = append(addresses, fmt.Sprintf("%s:6379", podList.Items[i].Status.PodIP))
		}
	}

	return addresses, nil
}

// redisPodIsReady reports whether a pod can currently take an ACL command.
func redisPodIsReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// redisUsersForPod maps a Redis pod that just became ready to the RedisUsers of
// its cluster. A restarted pod comes back with whatever its ACL file held, so
// without this the account could be missing until the next periodic resync.
func (r *RedisUserReconciler) redisUsersForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if pod.Labels["app.kubernetes.io/managed-by"] != "redguard-operator" ||
		pod.Labels["app.kubernetes.io/component"] != "redis" {
		return nil
	}
	cluster := pod.Labels["app.kubernetes.io/instance"]
	if cluster == "" || !redisPodIsReady(pod) {
		return nil
	}

	users := &redisv1alpha1.RedisUserList{}
	if err := r.List(ctx, users, client.InNamespace(pod.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list RedisUsers for a Redis pod event", "pod", pod.Name)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(users.Items))
	for _, user := range users.Items {
		if user.Spec.RedisClusterRef != cluster {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: user.Name, Namespace: user.Namespace},
		})
	}
	return requests
}

// reportACLStatus publishes whether the user is applied on every node. The
// CR's existing series are dropped first so a renamed spec.username leaves no
// stale series claiming the old account is still applied.
func (r *RedisUserReconciler) reportACLStatus(redisUser *redisv1alpha1.RedisUser, applied bool) {
	custmetrics.DeleteUserSeries(redisUser.Namespace, redisUser.Name)

	value := 0.0
	if applied {
		value = 1.0
	}
	custmetrics.RedisUserACLStatus.
		WithLabelValues(redisUser.Namespace, redisUser.Name, redisUser.Spec.Username).
		Set(value)
}

// setDegraded reports a partially applied user: the next pass that reaches
// Ready flips the condition back to False.
func (r *RedisUserReconciler) setDegraded(ctx context.Context, redisUser *redisv1alpha1.RedisUser, reason, message string) {
	redisUser.Status.Phase = phaseDegraded
	redisUser.Status.ObservedGeneration = redisUser.Generation
	r.reportACLStatus(redisUser, false)
	for _, cond := range []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse},
		{Type: "Degraded", Status: metav1.ConditionTrue},
	} {
		cond.ObservedGeneration = redisUser.Generation
		cond.Reason = reason
		cond.Message = message
		meta.SetStatusCondition(&redisUser.Status.Conditions, cond)
	}
	if err := r.Status().Update(ctx, redisUser); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisUser status")
	}
}

func (r *RedisUserReconciler) updateStatus(ctx context.Context, redisUser *redisv1alpha1.RedisUser, phase string, appliedTo []string, errorMsg string) {
	redisUser.Status.Phase = phase
	redisUser.Status.ObservedGeneration = redisUser.Generation
	if appliedTo != nil {
		redisUser.Status.AppliedTo = appliedTo
	}
	// Written here rather than at each call site so no failure path can leave
	// the gauge at 1 and make a broken user look applied.
	r.reportACLStatus(redisUser, phase == "Ready")

	ready := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: redisUser.Generation,
		Reason:             "Error",
		Message:            errorMsg,
	}
	if phase == "Ready" {
		ready.Status = metav1.ConditionTrue
		ready.Reason = "ACLApplied"
		ready.Message = "ACL successfully applied to Redis cluster"
	}

	// The ACL is re-applied on every pass, so only a pass that actually moved the
	// condition restamps the timestamp: a steady-state pass that changes nothing
	// must leave the status byte-identical, or the controller's own watch turns
	// each reconcile into the next one.
	changed := meta.SetStatusCondition(&redisUser.Status.Conditions, ready)
	if changed && phase == "Ready" {
		now := metav1.Now()
		redisUser.Status.LastPasswordChange = &now
	}

	// Only a fully applied user proves the partial application is over.
	if phase == "Ready" {
		meta.SetStatusCondition(&redisUser.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: redisUser.Generation,
			Reason:             "ACLApplied",
			Message:            "The user is applied on every ready Redis node",
		})
	}

	if err := r.Status().Update(ctx, redisUser); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisUser status")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RedisFactory == nil {
		r.RedisFactory = redisclient.DefaultFactory{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisUser{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.redisUsersForPod)).
		Named("redisuser").
		Complete(r)
}
