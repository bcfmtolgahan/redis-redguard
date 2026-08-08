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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// A credential push refuses to start unless every running Redis pod is
// dialable, so one partitioned pod fails it on every pass. That failure must
// degrade the pass, not abort it: updateStatus is the only place the master
// role label moves, and the label is the only thing pointing the write Service
// at a promoted master -- during exactly the kind of partition that causes
// both the stuck push and the failover.
var _ = Describe("RedisSentinel credential rotation under partition", func() {
	const (
		resourceName = "partition-rs"
		secretName   = "partition-auth"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	secretKey := types.NamespacedName{Name: secretName, Namespace: "default"}
	podIPs := []string{"10.244.41.10", "10.244.41.11", "10.244.41.12"}

	var (
		fakeFactory *redisfake.Factory
		reconciler  *RedisSentinelReconciler
		// recorder is spec-owned and large: the shared testRecorder blocks its
		// caller once its buffer fills, and this spec reconciles in a loop.
		recorder       *record.FakeRecorder
		podNames       []string
		appliedVersion string
	)

	// findRecordedEvent scans everything recorded so far without blocking.
	findRecordedEvent := func(reason string) string {
		for {
			select {
			case e := <-recorder.Events:
				if strings.Contains(e, " "+reason+" ") {
					return e
				}
			default:
				return ""
			}
		}
	}

	redisTemplateAnnotation := func(g Gomega) string {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, inDefault(resourceName+"-redis"), sts)).To(Succeed())
		return sts.Spec.Template.Annotations[builder.AuthSecretVersionAnnotation]
	}

	BeforeEach(func() {
		fakeFactory = redisfake.NewFactory()
		recorder = record.NewFakeRecorder(2000)
		reconciler = &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     recorder,
			RedisFactory: fakeFactory,
		}

		ensureSecret(secretName, "default", map[string][]byte{"password": []byte("old-password")})
		rs := newTestSentinel(resourceName, "default")
		rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: secretName}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		reconcileUntilSettled(ctx, reconciler, key)

		markStatefulSetReady(ctx, inDefault(resourceName+"-redis"), 3)
		markStatefulSetReady(ctx, inDefault(resourceName+"-sentinel"), 3)

		template := builder.BuildRedisStatefulSet(rs).Spec.Template
		podNames = nil
		for i, ip := range podIPs {
			name := fmt.Sprintf("%s-redis-%d", resourceName, i)
			createRunningPod(ctx, name, template.Labels, ip)
			podNames = append(podNames, name)
		}

		fakeFactory.SetMaster(podIPs[0] + ":6379")
		reconcileUntilSettled(ctx, reconciler, key)
		expectMasterLabelOnlyOn(ctx, podNames, podNames[0])

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		appliedVersion = secret.ResourceVersion
		Eventually(func(g Gomega) {
			g.Expect(redisTemplateAnnotation(g)).To(Equal(appliedVersion))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		// Drain the setup passes' events so the spec asserts only its own.
		_ = findRecordedEvent("\x00")
	})

	AfterEach(func() {
		for _, name := range podNames {
			pod := &corev1.Pod{}
			pod.Name, pod.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
		}
		deleteSentinel(ctx, reconciler, key)

		// envtest runs no garbage collector, so owned objects outlive the CR.
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed())
		}
		for _, name := range []string{secretName, resourceName + "-auth-state"} {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
		}
	})

	It("keeps moving the master label while a credential push cannot finish", func() {
		By("rotating the password while one pod is unreachable")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		secret.Data["password"] = []byte("new-password")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		fakeFactory.SetError(podIPs[2]+":6379", "Ping", fmt.Errorf("dial tcp %s:6379: i/o timeout", podIPs[2]))

		By("promoting another pod on the sentinel side")
		fakeFactory.SetMaster(podIPs[1] + ":6379")

		var result reconcile.Result
		Eventually(func(g Gomega) {
			var err error
			result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred(),
				"a stuck credential push must degrade the pass, not abort it before the master label moves")
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		Expect(result.RequeueAfter).NotTo(BeZero(), "the failed push must be retried on a requeue")

		By("following the promotion with the master label")
		expectMasterLabelOnlyOn(ctx, podNames, podNames[1])
		rs := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		Expect(rs.Status.MasterNode).To(Equal(podIPs[1] + ":6379"))

		By("reporting the stuck push instead of hiding it")
		degraded := meta.FindStatusCondition(rs.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal("CredentialRotationFailed"))
		Expect(findRecordedEvent("CredentialRotationFailed")).To(ContainSubstring("Warning"))

		By("carrying the previously applied stamp forward so nothing rolls early")
		Eventually(func(g Gomega) {
			g.Expect(redisTemplateAnnotation(g)).To(Equal(appliedVersion),
				"the pods were stamped with a password version that was never pushed")
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		By("completing the rotation once the pod is reachable again")
		fakeFactory.SetError(podIPs[2]+":6379", "Ping", nil)
		Eventually(func(g Gomega) {
			reconcileOnce(g, reconciler, key)
			g.Expect(redisTemplateAnnotation(g)).To(Equal(secret.ResourceVersion))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		degraded = meta.FindStatusCondition(rs.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
	})
})
