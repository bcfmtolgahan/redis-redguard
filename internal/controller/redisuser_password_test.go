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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	redisfake "github.com/bcfmtolgahan/redis-redguard/internal/redisclient/fake"
)

// TestRedisUser_RejectsInvalidPassword: a password the operator could not
// carry through every credential surface is refused before any ACL SETUSER is
// issued, with the refusal on the CR. An empty value in particular must never
// become an account passwords cannot protect.
func TestRedisUser_RejectsInvalidPassword(t *testing.T) {
	for name, password := range map[string]string{
		"empty":        "",
		"space":        "pass word",
		"double quote": `pass"word`,
	} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := redisv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			rs := newTestSentinel("userpw-rs", "default")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "userpw-secret", Namespace: "default"},
				Data:       map[string][]byte{"password": []byte(password)},
			}
			user := &redisv1alpha1.RedisUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "userpw",
					Namespace:  "default",
					Finalizers: []string{redisUserFinalizer},
				},
				Spec: redisv1alpha1.RedisUserSpec{
					RedisClusterRef:   "userpw-rs",
					Username:          "appuser",
					PasswordSecretRef: "userpw-secret",
				},
			}

			c := crfake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(rs, secret, user).
				WithStatusSubresource(&redisv1alpha1.RedisUser{}).
				Build()
			factory := redisfake.NewFactory()
			recorder := record.NewFakeRecorder(20)
			r := &RedisUserReconciler{Client: c, Scheme: scheme, RedisFactory: factory, Recorder: recorder}

			ctx := context.Background()
			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "userpw", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if res.RequeueAfter != userRetryInterval {
				t.Errorf("RequeueAfter = %v, want %v: a fixed Secret has no watch, only the retry", res.RequeueAfter, userRetryInterval)
			}
			if calls := factory.Calls(); len(calls) != 0 {
				t.Errorf("rejected password reached Redis: %v", calls)
			}

			got := &redisv1alpha1.RedisUser{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(user), got); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "InvalidPassword" {
				t.Errorf("Degraded condition = %+v, want True with reason InvalidPassword", cond)
			}

			event := ""
			select {
			case event = <-recorder.Events:
			default:
			}
			if !strings.Contains(event, "InvalidPassword") {
				t.Errorf("event = %q, want an InvalidPassword warning", event)
			}
		})
	}
}
