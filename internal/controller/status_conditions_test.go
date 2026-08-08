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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

func statusTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// condition returns the named condition or fails the test.
func condition(t *testing.T, conditions []metav1.Condition, name string) *metav1.Condition {
	t.Helper()

	cond := meta.FindStatusCondition(conditions, name)
	if cond == nil {
		t.Fatalf("condition %q is absent", name)
	}
	return cond
}

// backdateCondition moves the named condition an hour into the past and writes
// it back. A controller that rebuilds its condition slice from scratch on every
// pass overwrites the stamp; one that reports the same state through
// meta.SetStatusCondition leaves it where it is.
func backdateCondition(t *testing.T, c client.Client, obj client.Object, conditions []metav1.Condition, name string) metav1.Time {
	t.Helper()

	// Second granularity: metav1.Time is serialized as RFC3339, so two writes
	// inside the same second are indistinguishable and would hide the churn.
	backdated := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	condition(t, conditions, name).LastTransitionTime = backdated
	if err := c.Status().Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	return backdated
}

// TestRedisUserStatusDoesNotChurn covers the self-triggering reconcile: the
// controller watches its own object, so a status write that carries a new
// timestamp on every pass schedules the next pass forever.
func TestRedisUserStatusDoesNotChurn(t *testing.T) {
	scheme := statusTestScheme(t)
	user := &redisv1alpha1.RedisUser{
		ObjectMeta: metav1.ObjectMeta{Name: "churn-user", Namespace: "default", Generation: 3},
		Spec: redisv1alpha1.RedisUserSpec{
			RedisClusterRef:   "missing-cluster",
			Username:          "appuser",
			PasswordSecretRef: "user-pass",
		},
	}

	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(user).
		WithStatusSubresource(&redisv1alpha1.RedisUser{}).
		Build()

	r := &RedisUserReconciler{Client: c, Scheme: scheme, RedisFactory: redisfake.NewFactory()}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "churn-user", Namespace: "default"}}
	get := func() *redisv1alpha1.RedisUser {
		t.Helper()
		got := &redisv1alpha1.RedisUser{}
		if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The referenced cluster does not exist, so every pass reports the same
	// terminal error and none of them is a new transition.
	_, _ = r.Reconcile(context.Background(), req)
	first := get()
	if first.Status.ObservedGeneration != first.Generation {
		t.Errorf("observedGeneration = %d, want %d", first.Status.ObservedGeneration, first.Generation)
	}
	backdated := backdateCondition(t, c, first, first.Status.Conditions, "Ready")

	_, _ = r.Reconcile(context.Background(), req)
	after := condition(t, get().Status.Conditions, "Ready").LastTransitionTime
	if !after.Equal(&backdated) {
		t.Errorf("Ready lastTransitionTime moved from %v to %v without a status change", backdated, after)
	}
}

// TestRedisUserReadyStatusDoesNotChurn covers the steady state: an applied user
// reconciles again every five minutes, and each of those passes must leave the
// status subresource byte-identical.
func TestRedisUserReadyStatusDoesNotChurn(t *testing.T) {
	scheme := statusTestScheme(t)
	rs := newTestSentinel("churn-cluster", "default")
	pod := readyRedisPod("churn-cluster-redis-0", "default", "churn-cluster", "10.244.9.1")
	secret := passwordSecret("user-pass", "default", "s3cret")
	user := &redisv1alpha1.RedisUser{
		ObjectMeta: metav1.ObjectMeta{Name: "churn-ready", Namespace: "default", Generation: 1},
		Spec: redisv1alpha1.RedisUserSpec{
			RedisClusterRef:   "churn-cluster",
			Username:          "appuser",
			PasswordSecretRef: "user-pass",
		},
	}

	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rs, pod, secret, user).
		WithStatusSubresource(&redisv1alpha1.RedisUser{}).
		Build()

	r := &RedisUserReconciler{Client: c, Scheme: scheme, RedisFactory: redisfake.NewFactory()}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "churn-ready", Namespace: "default"}}
	get := func() *redisv1alpha1.RedisUser {
		t.Helper()
		got := &redisv1alpha1.RedisUser{}
		if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first := get()
	if first.Status.Phase != "Ready" {
		t.Fatalf("phase = %q, want Ready", first.Status.Phase)
	}
	if first.Status.ObservedGeneration != first.Generation {
		t.Errorf("observedGeneration = %d, want %d", first.Status.ObservedGeneration, first.Generation)
	}

	stamp := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	first.Status.LastPasswordChange = &stamp
	backdated := backdateCondition(t, c, first, first.Status.Conditions, "Ready")

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	second := get()
	after := condition(t, second.Status.Conditions, "Ready").LastTransitionTime
	if !after.Equal(&backdated) {
		t.Errorf("Ready lastTransitionTime moved from %v to %v without a status change", backdated, after)
	}
	if !second.Status.LastPasswordChange.Equal(&stamp) {
		t.Errorf("lastPasswordChange moved from %v to %v without a password change",
			stamp, second.Status.LastPasswordChange)
	}
}

// TestRedisBackupRejectedDestinationStatusDoesNotChurn keeps the destination
// refusal on one timestamp: the refusal is re-evaluated on every hourly pass and
// none of them is a new transition.
func TestRedisBackupRejectedDestinationStatusDoesNotChurn(t *testing.T) {
	scheme := statusTestScheme(t)
	backup := testBackup()
	backup.Generation = 2
	backup.Spec.S3.Bucket = "exfil-bucket"
	backup.Spec.S3.UseIAMRole = true

	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(backup).
		WithStatusSubresource(&redisv1alpha1.RedisBackup{}).
		Build()

	r := backupReconcilerWithS3(newFakeS3Store())
	r.Client = c
	r.Scheme = scheme
	r.AllowedBuckets = []string{"corp-backups"}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nightly", Namespace: "prod"}}
	get := func() *redisv1alpha1.RedisBackup {
		t.Helper()
		got := &redisv1alpha1.RedisBackup{}
		if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first := get()
	if first.Status.ObservedGeneration != first.Generation {
		t.Errorf("observedGeneration = %d, want %d", first.Status.ObservedGeneration, first.Generation)
	}
	backdated := backdateCondition(t, c, first, first.Status.Conditions, "Ready")

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	after := condition(t, get().Status.Conditions, "Ready").LastTransitionTime
	if !after.Equal(&backdated) {
		t.Errorf("Ready lastTransitionTime moved from %v to %v without a status change", backdated, after)
	}
}

// TestRestoreStatusConditionKeepsTransitionTime pins the same rule on the
// restore controller, whose phase is re-asserted on every requeue while a
// download or a resynchronisation is still in progress.
func TestRestoreStatusConditionKeepsTransitionTime(t *testing.T) {
	fx := newRestoreFixture(t, nil)

	if err := fx.r.updateStatus(context.Background(), fx.get(t),
		redisv1alpha1.RestorePhaseDownloading, "Staging the backup payload", 0); err != nil {
		t.Fatalf("first status write: %v", err)
	}
	restore := fx.get(t)
	backdated := backdateCondition(t, fx.c, restore, restore.Status.Conditions, "Ready")

	if err := fx.r.updateStatus(context.Background(), fx.get(t),
		redisv1alpha1.RestorePhaseDownloading, "Staging the backup payload", 0); err != nil {
		t.Fatalf("second status write: %v", err)
	}
	after := condition(t, fx.get(t).Status.Conditions, "Ready").LastTransitionTime
	if !after.Equal(&backdated) {
		t.Errorf("Ready lastTransitionTime moved from %v to %v without a phase change", backdated, after)
	}
}

// readyRedisPod builds a Running, Ready Redis pod of cluster for the fake
// client, carrying the labels getAllRedisPodAddresses selects on.
func readyRedisPod(name, ns, cluster, ip string) *corev1.Pod {
	return &corev1.Pod{
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
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func passwordSecret(name, ns, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"password": []byte(password)},
	}
}
