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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	"github.com/redguard/redguard/internal/redisclient/fake"
)

// specMonitorConfig is the monitor state that matches newTestSentinel, so a
// fake seeded with it reports no drift.
func specMonitorConfig() map[string]string {
	return map[string]string{
		"quorum":                  "2",
		"down-after-milliseconds": "5000",
		"failover-timeout":        "10000",
		"parallel-syncs":          "1",
	}
}

// firstOpIndex returns the position of the first op containing substr, or -1.
func firstOpIndex(ops []string, substr string) int {
	for i, op := range ops {
		if strings.Contains(op, substr) {
			return i
		}
	}
	return -1
}

var _ = Describe("RedisSentinel credential rotation", func() {
	const (
		resourceName = "rotate-rs"
		secretName   = "rotate-auth"
		appliedName  = resourceName + "-auth-state"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	secretKey := types.NamespacedName{Name: secretName, Namespace: "default"}
	redisIPs := []string{"10.244.31.10", "10.244.31.11", "10.244.31.12"}
	sentinelIPs := []string{"10.244.31.20", "10.244.31.21", "10.244.31.22"}

	var (
		fakeFactory *fake.Factory
		reconciler  *RedisSentinelReconciler
		podNames    []string
	)

	redisAddr := func(i int) string { return redisIPs[i] + ":6379" }
	sentinelAddr := func(i int) string { return sentinelIPs[i] + ":26379" }

	templateAnnotation := func(g Gomega, stsName, annotation string) string {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, inDefault(stsName), sts)).To(Succeed())
		return sts.Spec.Template.Annotations[annotation]
	}

	BeforeEach(func() {
		fakeFactory = fake.NewFactory()
		fakeFactory.SetMonitorConfig(specMonitorConfig())
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fakeFactory,
		}

		ensureSecret(secretName, "default", map[string][]byte{"password": []byte("old-password")})

		rs := newTestSentinel(resourceName, "default")
		rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: secretName}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		redisTemplate := builder.BuildRedisStatefulSet(rs).Spec.Template
		sentinelTemplate := builder.BuildSentinelStatefulSet(rs).Spec.Template
		podNames = nil
		for i, ip := range redisIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, redisTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		for i, ip := range sentinelIPs {
			name := fmt.Sprintf("%s-sentinel-%d", resourceName, i)
			createRunningPod(ctx, name, sentinelTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		fakeFactory.SetMaster(redisAddr(0))

		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		for _, name := range []string{secretName, appliedName} {
			secret := &corev1.Secret{}
			secret.Name, secret.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
		}
		drainEvents()
	})

	It("records the password the cluster accepts in an owned applied-credentials secret", func() {
		owner := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, owner)).To(Succeed())
		userSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, userSecret)).To(Succeed())

		applied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(appliedName), applied)).To(Succeed(),
			"without a record of the credential the cluster currently accepts, the operator cannot authenticate during a rotation")
		Expect(string(applied.Data["password"])).To(Equal("old-password"))
		Expect(string(applied.Data["authSecretResourceVersion"])).To(Equal(userSecret.ResourceVersion))
		expectControlledBy(applied, owner)
	})

	It("pushes nothing while the password is unchanged", func() {
		reconcileUntilSettled(ctx, reconciler, key)

		for _, op := range fakeFactory.Ops() {
			Expect(op).NotTo(ContainSubstring("requirepass"),
				"a steady-state pass must not rewrite credentials")
			Expect(op).NotTo(ContainSubstring("PasswordAll"),
				"a steady-state pass must not touch sentinel credentials")
		}
	})

	It("pushes a rotated password in phases and only then re-stamps the pod templates", func() {
		By("rotating the password in the auth Secret")
		userSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, userSecret)).To(Succeed())
		userSecret.Data["password"] = []byte("new-password")
		Expect(k8sClient.Update(ctx, userSecret)).To(Succeed())
		Expect(k8sClient.Get(ctx, secretKey, userSecret)).To(Succeed())

		By("reconciling until the new version is stamped")
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(templateAnnotation(g, resourceName+"-redis", builder.AuthSecretVersionAnnotation)).
				To(Equal(userSecret.ResourceVersion))
			g.Expect(templateAnnotation(g, resourceName+"-sentinel", builder.AuthSecretVersionAnnotation)).
				To(Equal(userSecret.ResourceVersion))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("recording the new password as applied")
		applied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(appliedName), applied)).To(Succeed())
		Expect(string(applied.Data["password"])).To(Equal("new-password"))

		By("having pushed the credential to every node")
		ops := fakeFactory.Ops()
		for i := range redisIPs {
			Expect(ops).To(ContainElement(redisAddr(i)+":ACLSetUser:default:>new-password"),
				"node %d never learned to accept the new password", i)
			Expect(ops).To(ContainElement(redisAddr(i)+":ConfigSet:masterauth=new-password"),
				"node %d would reauthenticate to the master with the old password", i)
			Expect(ops).To(ContainElement(redisAddr(i)+":ConfigSet:requirepass=new-password"),
				"node %d still accepts the old password", i)
		}
		for i := range sentinelIPs {
			Expect(ops).To(ContainElement(sentinelAddr(i)+":AddPasswordAll:new-password"),
				"sentinel %d never learned to accept the new password", i)
			Expect(ops).To(ContainElement(sentinelAddr(i)+":SetOutboundPasswordAll:"+resourceName+"-master:new-password"),
				"sentinel %d still presents the old password to redis and its peers", i)
			Expect(ops).To(ContainElement(sentinelAddr(i)+":ResetPasswordAll:new-password"),
				"sentinel %d still accepts the old password", i)
		}

		By("ordering the push: widen everywhere, then outbound, then narrow")
		lastWiden, firstOutbound, lastOutbound, firstNarrow := -1, len(ops), -1, len(ops)
		for i := range redisIPs {
			if idx := firstOpIndex(ops, redisAddr(i)+":ACLSetUser:default:>new-password"); idx > lastWiden {
				lastWiden = idx
			}
			if idx := firstOpIndex(ops, redisAddr(i)+":ConfigSet:masterauth=new-password"); idx < firstOutbound {
				firstOutbound = idx
			}
			if idx := firstOpIndex(ops, redisAddr(i)+":ConfigSet:masterauth=new-password"); idx > lastOutbound {
				lastOutbound = idx
			}
			if idx := firstOpIndex(ops, redisAddr(i)+":ConfigSet:requirepass=new-password"); idx < firstNarrow {
				firstNarrow = idx
			}
		}
		for i := range sentinelIPs {
			if idx := firstOpIndex(ops, sentinelAddr(i)+":AddPasswordAll:new-password"); idx > lastWiden {
				lastWiden = idx
			}
			if idx := firstOpIndex(ops, sentinelAddr(i)+":SetOutboundPasswordAll:"); idx < firstOutbound {
				firstOutbound = idx
			}
			if idx := firstOpIndex(ops, sentinelAddr(i)+":SetOutboundPasswordAll:"); idx > lastOutbound {
				lastOutbound = idx
			}
			if idx := firstOpIndex(ops, sentinelAddr(i)+":ResetPasswordAll:new-password"); idx < firstNarrow {
				firstNarrow = idx
			}
		}
		Expect(lastWiden).To(BeNumerically("<", firstOutbound),
			"a node started presenting the new password before every node accepted it: ops=%v", ops)
		Expect(lastOutbound).To(BeNumerically("<", firstNarrow),
			"a node stopped accepting the old password while another still presented it: ops=%v", ops)

		By("announcing the rotation")
		Expect(findEvent("CredentialsRotated")).NotTo(BeEmpty(),
			"an online credential rotation across the whole cluster deserves an event")
	})

	It("keeps the old pod template and degrades when the push cannot reach a node", func() {
		annotationBefore := ""
		Eventually(func(g Gomega) {
			annotationBefore = templateAnnotation(g, resourceName+"-redis", builder.AuthSecretVersionAnnotation)
			g.Expect(annotationBefore).NotTo(BeEmpty())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		fakeFactory.SetError(redisAddr(1), "Ping", fmt.Errorf("connection refused"))

		userSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, userSecret)).To(Succeed())
		userSecret.Data["password"] = []byte("new-password")
		Expect(k8sClient.Update(ctx, userSecret)).To(Succeed())

		By("failing the pass instead of rolling into a split cluster")
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).To(HaveOccurred(),
				"an incomplete push must surface, not silently leave the cluster half-configured")
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("writing no credential anywhere before every node was reachable")
		for _, op := range fakeFactory.Ops() {
			Expect(op).NotTo(ContainSubstring("new-password"),
				"a partial push splits the cluster across two credentials: %s", op)
		}

		By("keeping the pod templates on the applied version")
		Expect(templateAnnotation(Default, resourceName+"-redis", builder.AuthSecretVersionAnnotation)).
			To(Equal(annotationBefore),
				"rolling the pods before the online push succeeded restarts them into a password their peers reject")

		applied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(appliedName), applied)).To(Succeed())
		Expect(string(applied.Data["password"])).To(Equal("old-password"))

		By("surfacing the failure as a condition")
		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		degraded := meta.FindStatusCondition(rs.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("CredentialRotationFailed"))

		By("recovering once the node is reachable again")
		fakeFactory.SetError(redisAddr(1), "Ping", nil)
		Expect(k8sClient.Get(ctx, secretKey, userSecret)).To(Succeed())
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(templateAnnotation(g, resourceName+"-redis", builder.AuthSecretVersionAnnotation)).
				To(Equal(userSecret.ResourceVersion))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

var _ = Describe("RedisSentinel sentinel monitor drift", func() {
	const resourceName = "drift-rs"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	redisIPs := []string{"10.244.32.10", "10.244.32.11", "10.244.32.12"}
	sentinelIPs := []string{"10.244.32.20", "10.244.32.21", "10.244.32.22"}

	var (
		fakeFactory *fake.Factory
		reconciler  *RedisSentinelReconciler
		podNames    []string
	)

	sentinelAddr := func(i int) string { return sentinelIPs[i] + ":26379" }

	BeforeEach(func() {
		fakeFactory = fake.NewFactory()
		fakeFactory.SetMonitorConfig(specMonitorConfig())
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fakeFactory,
		}

		rs := newTestSentinel(resourceName, "default")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		redisTemplate := builder.BuildRedisStatefulSet(rs).Spec.Template
		sentinelTemplate := builder.BuildSentinelStatefulSet(rs).Spec.Template
		podNames = nil
		for i, ip := range redisIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, redisTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		for i, ip := range sentinelIPs {
			name := fmt.Sprintf("%s-sentinel-%d", resourceName, i)
			createRunningPod(ctx, name, sentinelTemplate.Labels, ip)
			podNames = append(podNames, name)
		}
		fakeFactory.SetMaster(redisIPs[0] + ":6379")

		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		drainEvents()
	})

	It("repairs monitor parameters on every sentinel when they drift from the spec", func() {
		fakeFactory.SetMonitorConfig(map[string]string{
			"quorum":                  "3",
			"down-after-milliseconds": "9000",
			"failover-timeout":        "10000",
			"parallel-syncs":          "1",
		})

		reconcileUntilSettled(ctx, reconciler, key)

		ops := fakeFactory.Ops()
		for i := range sentinelIPs {
			Expect(ops).To(ContainElement(sentinelAddr(i)+":SetMasterOptionAll:down-after-milliseconds=5000"),
				"sentinel %d keeps the drifted timing: SENTINEL SET never propagates, every member needs its own repair", i)
			Expect(ops).To(ContainElement(sentinelAddr(i)+":SetMasterOptionAll:quorum=2"),
				"sentinel %d keeps the drifted quorum", i)
		}
		for _, op := range ops {
			Expect(op).NotTo(ContainSubstring("failover-timeout"),
				"a parameter that matches the spec must not be rewritten: %s", op)
			Expect(op).NotTo(ContainSubstring("parallel-syncs"),
				"a parameter that matches the spec must not be rewritten: %s", op)
		}

		Expect(findEvent("SentinelConfigRepaired")).To(ContainSubstring("down-after-milliseconds"),
			"a runtime repair of failover behavior must be visible")

		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		cond := meta.FindStatusCondition(rs.Status.Conditions, "SentinelConfigInSync")
		Expect(cond).NotTo(BeNil(), "the sync state of the sentinel monitor config must be a condition")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("leaves matching parameters alone", func() {
		reconcileUntilSettled(ctx, reconciler, key)

		for _, op := range fakeFactory.Ops() {
			Expect(op).NotTo(ContainSubstring("SetMasterOptionAll"),
				"an in-sync sentinel was rewritten: %s", op)
		}
		Expect(findEvent("SentinelConfigRepaired")).To(BeEmpty())
	})

	It("reports a sentinel that cannot be repaired without failing the pass", func() {
		fakeFactory.SetMonitorConfig(map[string]string{
			"quorum":                  "2",
			"down-after-milliseconds": "9000",
			"failover-timeout":        "10000",
			"parallel-syncs":          "1",
		})
		fakeFactory.SetError(sentinelAddr(1), "SetMasterOptionAll", fmt.Errorf("connection reset"))

		reconcileUntilSettled(ctx, reconciler, key)

		Eventually(func(g Gomega) {
			rs := &redisv1alpha1.RedisSentinel{}
			g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
			cond := meta.FindStatusCondition(rs.Status.Conditions, "SentinelConfigInSync")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"a sentinel left on drifted failover timings must be visible in status")
			g.Expect(cond.Message).To(ContainSubstring(sentinelIPs[1]))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

var _ = Describe("RedisSentinel TLS certificate rotation", func() {
	const (
		resourceName = "tlsroll-rs"
		certName     = "tlsroll-cert"
		caName       = "tlsroll-ca"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	var reconciler *RedisSentinelReconciler

	templateAnnotation := func(g Gomega, stsName, annotation string) string {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, inDefault(stsName), sts)).To(Succeed())
		return sts.Spec.Template.Annotations[annotation]
	}

	BeforeEach(func() {
		// A fake factory even though no spec dials anything: with the default
		// factory every reconcile pass resolves *.svc.cluster.local sentinel
		// addresses, which stalls for seconds per lookup outside a cluster.
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: fake.NewFactory(),
		}

		certPEM, keyPEM := selfSignedPEMForTest()
		caPEM, _ := selfSignedPEMForTest()
		ensureSecret(certName, "default", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM})
		ensureSecret(caName, "default", map[string][]byte{"ca.crt": caPEM})

		rs := newTestSentinel(resourceName, "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{
			Enabled:              true,
			CertificateSecretRef: certName,
			CASecretRef:          caName,
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)
		drainEvents()
	})

	AfterEach(func() {
		deleteSentinel(ctx, reconciler, key)
		for _, name := range []string{certName, caName} {
			secret := &corev1.Secret{}
			secret.Name, secret.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
		}
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		drainEvents()
	})

	It("stamps the TLS secret versions on both pod templates", func() {
		certSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(certName), certSecret)).To(Succeed())
		caSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(caName), caSecret)).To(Succeed())

		want := certSecret.ResourceVersion + "/" + caSecret.ResourceVersion
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			Eventually(func(g Gomega) {
				g.Expect(templateAnnotation(g, name, builder.TLSSecretVersionAnnotation)).To(Equal(want),
					"%s pods do not record which certificates they serve, so a renewal never reaches them", name)
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		}
	})

	It("re-stamps both pod templates when the certificate is renewed", func() {
		before := ""
		Eventually(func(g Gomega) {
			before = templateAnnotation(g, resourceName+"-redis", builder.TLSSecretVersionAnnotation)
			g.Expect(before).NotTo(BeEmpty())
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("renewing the certificate")
		certPEM, keyPEM := selfSignedPEMForTest()
		certSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, inDefault(certName), certSecret)).To(Succeed())
		certSecret.Data["tls.crt"] = certPEM
		certSecret.Data["tls.key"] = keyPEM
		Expect(k8sClient.Update(ctx, certSecret)).To(Succeed())

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			Eventually(func(g Gomega) {
				reconcileOnce(g, reconciler, key)
				g.Expect(templateAnnotation(g, name, builder.TLSSecretVersionAnnotation)).NotTo(Equal(before),
					"%s pods keep serving the certificate they started with", name)
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		}

		Expect(findEvent("ConfigRollout")).To(ContainSubstring("TLS"),
			"a restart caused by a certificate renewal must say so")
	})

	It("maps the TLS secrets back to the RedisSentinels that reference them", func() {
		for _, name := range []string{certName, caName} {
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, inDefault(name), secret)).To(Succeed())
			Expect(reconciler.sentinelsForReferencedSecret(ctx, secret)).To(ContainElement(
				reconcile.Request{NamespacedName: key}),
				"a change to %s never triggers a reconcile, so the pods keep the old certificate until an unrelated restart", name)
		}

		other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "unreferenced-tls-secret", Namespace: "default"}}
		Expect(reconciler.sentinelsForReferencedSecret(ctx, other)).To(BeEmpty())
	})
})

// selfSignedPEMForTest returns a fresh self-signed certificate and key pair.
// Every call generates new material, so a renewal is observable as a change.
func selfSignedPEMForTest() (certPEM, keyPEM []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "redguard-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
