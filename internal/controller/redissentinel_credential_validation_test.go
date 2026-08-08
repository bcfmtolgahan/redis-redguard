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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

const (
	credValNS      = "default"
	credValCluster = "credval-rs"
	credValSecret  = "credval-auth"
)

type credValFixture struct {
	r        *RedisSentinelReconciler
	factory  *redisfake.Factory
	recorder *record.FakeRecorder
	c        client.Client
	rs       *redisv1alpha1.RedisSentinel
}

// newCredValFixture builds a sentinel reconciler over a fake API server seeded
// with the CR, its auth Secret holding password, and any extra objects.
func newCredValFixture(t *testing.T, password string, extra ...client.Object) *credValFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	rs := newTestSentinel(credValCluster, credValNS)
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: credValSecret}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credValSecret, Namespace: credValNS},
		Data:       map[string][]byte{"password": []byte(password)},
	}

	objects := append([]client.Object{rs, secret}, extra...)
	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&redisv1alpha1.RedisSentinel{}).
		Build()

	factory := redisfake.NewFactory()
	recorder := record.NewFakeRecorder(50)
	return &credValFixture{
		r: &RedisSentinelReconciler{
			Client:       c,
			Scheme:       scheme,
			Recorder:     recorder,
			RedisFactory: factory,
		},
		factory:  factory,
		recorder: recorder,
		c:        c,
		rs:       rs,
	}
}

func (fx *credValFixture) recordedEvent(reason string) bool {
	for {
		select {
		case e := <-fx.recorder.Events:
			if strings.Contains(e, " "+reason+" ") {
				return true
			}
		default:
			return false
		}
	}
}

// TestAuthCredentials_RejectsInvalidPasswordBeforePush: an empty or
// unrepresentable password must never be pushed or stamped. Empty renders the
// default ACL user nopass on every node; whitespace or quotes kill every pod
// at config parse, and after the rotation narrowed the old password away that
// state is unrecoverable. The pass degrades instead.
func TestAuthCredentials_RejectsInvalidPasswordBeforePush(t *testing.T) {
	for name, password := range map[string]string{
		"empty":        "",
		"space":        "pass word",
		"double quote": `pass"word`,
		"backslash":    `pass\word`,
	} {
		t.Run(name, func(t *testing.T) {
			fx := newCredValFixture(t, password)
			ctx := context.Background()

			version, degraded, err := fx.r.reconcileAuthCredentials(ctx, fx.rs)
			if err != nil {
				t.Fatalf("reconcileAuthCredentials: %v", err)
			}
			if version != "" {
				t.Errorf("version = %q, want empty: a rejected password must not be stamped", version)
			}
			if degraded == "" {
				t.Error("degraded message empty; the pass would report a healthy rotation")
			}
			if calls := fx.factory.Calls(); len(calls) != 0 {
				t.Errorf("rejected password was pushed to the cluster: %v", calls)
			}
			if ops := fx.factory.Ops(); len(ops) != 0 {
				t.Errorf("rejected password reached a sentinel: %v", ops)
			}

			applied := &corev1.Secret{}
			err = fx.c.Get(ctx, types.NamespacedName{Name: appliedAuthSecretName(fx.rs), Namespace: credValNS}, applied)
			if !apierrors.IsNotFound(err) {
				t.Errorf("applied-credentials secret written for a rejected password (err=%v)", err)
			}

			if !fx.recordedEvent("InvalidAuthPassword") {
				t.Error("no InvalidAuthPassword event recorded")
			}
			rs := &redisv1alpha1.RedisSentinel{}
			if err := fx.c.Get(ctx, types.NamespacedName{Name: credValCluster, Namespace: credValNS}, rs); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(rs.Status.Conditions, "Degraded")
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "InvalidAuthPassword" {
				t.Errorf("Degraded condition = %+v, want True with reason InvalidAuthPassword", cond)
			}
		})
	}
}

// TestAuthCredentials_InvalidRotationCarriesPreviousStamp: rotating to a
// rejected password must keep the pods on the applied credential. Moving the
// stamp would roll them onto a value their peers never accepted and, for a
// parse-breaking one, leave the cluster accepting only a password no pod can
// boot with.
func TestAuthCredentials_InvalidRotationCarriesPreviousStamp(t *testing.T) {
	applied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credValCluster + "-auth-state", Namespace: credValNS},
		Data: map[string][]byte{
			appliedPasswordKey: []byte("old-password"),
			appliedVersionKey:  []byte("42"),
		},
	}
	fx := newCredValFixture(t, `broken"rotation`, applied)
	ctx := context.Background()

	version, degraded, err := fx.r.reconcileAuthCredentials(ctx, fx.rs)
	if err != nil {
		t.Fatalf("reconcileAuthCredentials: %v", err)
	}
	if version != "42" {
		t.Errorf("version = %q, want the previously applied stamp 42 carried forward", version)
	}
	if degraded == "" {
		t.Error("degraded message empty for a rejected rotation")
	}
	if calls := fx.factory.Calls(); len(calls) != 0 {
		t.Errorf("rejected rotation was pushed: %v", calls)
	}

	got := &corev1.Secret{}
	if err := fx.c.Get(ctx, types.NamespacedName{Name: applied.Name, Namespace: credValNS}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data[appliedPasswordKey]) != "old-password" || string(got.Data[appliedVersionKey]) != "42" {
		t.Errorf("applied-credentials secret rewritten to %q/%q; the cluster still accepts only the old password",
			got.Data[appliedPasswordKey], got.Data[appliedVersionKey])
	}
}
