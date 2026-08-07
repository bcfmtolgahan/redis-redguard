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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// These specs exercise the CRD's CEL rules against a real API server. A spec
// with tls.enabled and no certificateSecretRef describes a cluster whose pods
// listen on TLS only while nothing mounts a certificate, so admission must
// reject it.
var _ = Describe("RedisSentinel TLS validation", func() {
	It("rejects tls.enabled without certificateSecretRef", func() {
		rs := newTestSentinel("tls-missing-cert", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true}

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("certificateSecretRef is required when TLS is enabled"))
	})

	It("accepts tls.enabled with certificateSecretRef", func() {
		rs := newTestSentinel("tls-with-cert", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{
			Enabled:              true,
			CertificateSecretRef: "demo-tls",
		}

		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	})

	It("accepts an explicitly disabled tls block without a certificate", func() {
		rs := newTestSentinel("tls-disabled", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: false}

		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	})
})

// customConfig is validated at reconcile, not at admission: the reserved-
// directive logic (case-insensitive names, tls- prefix, sentinel sub-
// directives) is not reasonably expressible in CEL. A rejected spec must
// degrade the CR with an actionable message and must never render a ConfigMap
// carrying the injected directive.
var _ = Describe("RedisSentinel customConfig validation", func() {
	It("degrades on an injected directive instead of rendering it", func() {
		rs := newTestSentinel("customconfig-inject", "default")
		rs.Spec.RedisConfig.CustomConfig = map[string]string{
			"maxmemory": "1gb\nrequirepass hacked",
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		key := types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}
		reconciler := &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: redisfake.NewFactory(),
		}

		// The manager-owned reconciler works the same CR in the background, so
		// a conflict on the finalizer update is possible; retry until the CR
		// reports the terminal state.
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())

			updated := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			g.Expect(updated.Status.Phase).To(Equal("Degraded"))

			cond := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Message).To(ContainSubstring("customConfig"))
		}).Should(Succeed())

		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name + "-redis-config", Namespace: rs.Namespace}, cm)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no ConfigMap may be rendered from a rejected spec")

		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisSentinel{}))).To(BeTrue())
		}).Should(Succeed())
	})

	It("degrades on a reserved sentinel directive override", func() {
		rs := newTestSentinel("customconfig-reserved", "default")
		rs.Spec.SentinelConfig.CustomConfig = map[string]string{
			"sentinel auth-pass": "other-master stolen",
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		key := types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}
		reconciler := &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: redisfake.NewFactory(),
		}

		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())

			updated := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			g.Expect(updated.Status.Phase).To(Equal("Degraded"))
		}).Should(Succeed())

		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name + "-sentinel-config", Namespace: rs.Namespace}, cm)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &redisv1alpha1.RedisSentinel{}))).To(BeTrue())
		}).Should(Succeed())
	})
})
