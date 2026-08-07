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
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
)

// TestSecretWatchIsRegistered guards the wiring the map function alone cannot
// prove: a reconciler that maps Secrets correctly but never watches them still
// leaves a rotated password sitting in the Secret.
func TestSecretWatchIsRegistered(t *testing.T) {
	src, err := os.ReadFile("redissentinel_controller.go")
	if err != nil {
		t.Fatal(err)
	}
	setup := string(src)
	if i := strings.Index(setup, "func (r *RedisSentinelReconciler) SetupWithManager"); i >= 0 {
		setup = setup[i:]
	}
	if !strings.Contains(setup, "Watches(&corev1.Secret{}") {
		t.Error("SetupWithManager does not watch Secrets; rotating the auth Secret would never reach the running pods")
	}
}

var _ = Describe("RedisSentinel configuration rollout", func() {
	const (
		resourceName = "roll-rs"
		secretName   = "roll-auth"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	secretKey := types.NamespacedName{Name: secretName, Namespace: "default"}

	var reconciler *RedisSentinelReconciler

	// podTemplateAnnotations returns the annotations of a StatefulSet pod
	// template, driving the reconciler until the value settles. The Manager runs
	// the same reconciler against a cached client, so a value written here can be
	// overwritten once from a cache that has not caught up yet.
	podTemplateAnnotations := func(g Gomega, name string) map[string]string {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, inDefault(name), sts)).To(Succeed())
		return sts.Spec.Template.Annotations
	}

	BeforeEach(func() {
		reconciler = &RedisSentinelReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder,
		}

		ensureSecret(secretName, "default", map[string][]byte{"password": []byte("initial-password")})

		rs := newTestSentinel(resourceName, "default")
		rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: secretName}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(errors.IsNotFound(k8sClient.Get(ctx, key, rs))).To(BeTrue())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
		drainEvents()
	})

	It("stamps the rendered config and the auth Secret version on both pod templates", func() {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			Eventually(func(g Gomega) {
				annotations := podTemplateAnnotations(g, name)
				g.Expect(annotations).To(HaveKey(builder.ConfigHashAnnotation))
				g.Expect(annotations).To(HaveKeyWithValue(
					builder.AuthSecretVersionAnnotation, secret.ResourceVersion),
					"%s pods do not record which version of the password they read", name)
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		}
	})

	It("changes the config hash when customConfig changes", func() {
		var before map[string]string
		Eventually(func(g Gomega) {
			before = podTemplateAnnotations(g, resourceName+"-redis")
			g.Expect(before).To(HaveKey(builder.ConfigHashAnnotation))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		sentinelBefore := ""
		Eventually(func(g Gomega) {
			sentinelBefore = podTemplateAnnotations(g, resourceName+"-sentinel")[builder.ConfigHashAnnotation]
			g.Expect(sentinelBefore).NotTo(BeEmpty())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("editing spec.redisConfig.customConfig")
		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			rs.Spec.RedisConfig.CustomConfig = map[string]string{"maxmemory-policy": "allkeys-lru"}
			g.Expect(k8sClient.Update(ctx, rs)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(podTemplateAnnotations(g, resourceName+"-redis")[builder.ConfigHashAnnotation]).
				NotTo(Equal(before[builder.ConfigHashAnnotation]),
					"the Redis pods keep the configuration they started with")
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("leaving the Sentinel pods alone")
		Consistently(func(g Gomega) {
			g.Expect(podTemplateAnnotations(g, resourceName+"-sentinel")[builder.ConfigHashAnnotation]).
				To(Equal(sentinelBefore),
					"a redis-only edit restarted the Sentinel pods as well")
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("changes the recorded Secret version when the password is rotated", func() {
		var before string
		Eventually(func(g Gomega) {
			before = podTemplateAnnotations(g, resourceName+"-redis")[builder.AuthSecretVersionAnnotation]
			g.Expect(before).NotTo(BeEmpty())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("rotating the password")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data["password"] = []byte("rotated-password")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			Eventually(func(g Gomega) {
				reconcileOnce(g, reconciler, key)
				g.Expect(podTemplateAnnotations(g, name)).To(HaveKeyWithValue(
					builder.AuthSecretVersionAnnotation, secret.ResourceVersion),
					"%s pods keep serving the password they read at startup", name)
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		}
		Expect(secret.ResourceVersion).NotTo(Equal(before))

		// A rotation is not just a restart: while it runs, restarted nodes
		// present the new password to peers that still require the old one.
		Expect(findEvent("ConfigRollout")).To(ContainSubstring("auth Secret changed"),
			"the one rollout that breaks replication while it runs was announced as an ordinary one")
	})

	It("warns that a config change restarts the pods before it does", func() {
		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			rs.Spec.RedisConfig.CustomConfig = map[string]string{"maxmemory-policy": "volatile-lru"}
			g.Expect(k8sClient.Update(ctx, rs)).To(Succeed())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		reconcileUntilSettled(ctx, reconciler, key)

		Expect(findEvent("ConfigRollout")).To(ContainSubstring("Warning"),
			"an unannounced restart of the master is a surprise: Sentinel promotes a replica when it goes")
	})

	It("maps an auth Secret back to the RedisSentinels that reference it", func() {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())

		Expect(reconciler.sentinelsForAuthSecret(ctx, secret)).To(ContainElement(
			reconcile.Request{NamespacedName: key}))

		other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "unreferenced-secret", Namespace: "default"}}
		Expect(reconciler.sentinelsForAuthSecret(ctx, other)).To(BeEmpty(),
			"an unrelated Secret must not restart a Redis cluster")

		elsewhere := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: secretName, Namespace: "kube-system"}}
		Expect(reconciler.sentinelsForAuthSecret(ctx, elsewhere)).To(BeEmpty(),
			"a same-named Secret in another namespace is a different Secret")
	})
})

var _ = Describe("RedisSentinel configuration rollout without auth", func() {
	const resourceName = "roll-noauth-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	var reconciler *RedisSentinelReconciler

	BeforeEach(func() {
		reconciler = &RedisSentinelReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder,
		}
		Expect(k8sClient.Create(ctx, newTestSentinel(resourceName, "default"))).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(errors.IsNotFound(k8sClient.Get(ctx, key, rs))).To(BeTrue())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		drainEvents()
	})

	It("stamps a config hash but no Secret version", func() {
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			Eventually(func(g Gomega) {
				sts := &appsv1.StatefulSet{}
				g.Expect(k8sClient.Get(ctx, inDefault(name), sts)).To(Succeed())
				g.Expect(sts.Spec.Template.Annotations).To(HaveKey(builder.ConfigHashAnnotation))
				g.Expect(sts.Spec.Template.Annotations).NotTo(HaveKey(builder.AuthSecretVersionAnnotation),
					"there is no auth Secret to record a version of")
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		}
	})
})

// reconcileOnce drives a reconcile inside an Eventually block, failing the
// attempt rather than the spec when it loses a race against the Manager.
func reconcileOnce(g Gomega, r *RedisSentinelReconciler, key types.NamespacedName) {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
}

// drainEvents empties testRecorder. Its channel send blocks once the buffer is
// full, which would wedge whichever reconciler recorded the event next.
func drainEvents() {
	for {
		select {
		case <-testRecorder.Events:
		default:
			return
		}
	}
}

// findEvent returns the first recorded event whose reason matches, draining
// everything recorded before it.
func findEvent(reason string) string {
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-testRecorder.Events:
			if strings.Contains(e, " "+reason+" ") {
				return e
			}
		case <-deadline:
			return ""
		}
	}
}
