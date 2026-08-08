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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

var _ = Describe("RedisUser Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName = "test-resource"
			clusterName  = "test-cluster"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		redisuser := &redisv1alpha1.RedisUser{}

		var controllerReconciler *RedisUserReconciler

		BeforeEach(func() {
			By("creating the RedisSentinel cluster the user belongs to")
			ensureTestSentinel(clusterName, "default")

			By("creating the user password secret")
			ensureSecret("user-pass", "default", map[string][]byte{
				"password": []byte("test-password"),
			})

			By("creating the custom resource for the Kind RedisUser")
			err := k8sClient.Get(ctx, typeNamespacedName, redisuser)
			if err != nil && errors.IsNotFound(err) {
				resource := &redisv1alpha1.RedisUser{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: redisv1alpha1.RedisUserSpec{
						RedisClusterRef:   clusterName,
						Username:          "testuser",
						PasswordSecretRef: "user-pass",
						Enabled:           ptr.To(true),
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			controllerReconciler = &RedisUserReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				RedisFactory: redisfake.NewFactory(),
			}
		})

		AfterEach(func() {
			resource := &redisv1alpha1.RedisUser{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RedisUser")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// Deletion holds the finalizer until the ACL user has been removed
			// from a reachable pod, so the cleanup path needs one.
			ensureReadyRedisPod(clusterName+"-redis-0", "default", clusterName, "10.244.5.1")
			defer deleteRedisPods("default", clusterName)

			// Reconcile once more so the finalizer added above is removed.
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				err = k8sClient.Get(ctx, typeNamespacedName, resource)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		})

		// Redis honours a rule wherever it appears in the SETUSER list, so a
		// grant written under spec.aclRules.keys is a real grant that the
		// confinement floor never follows. The rejection has to reach the user,
		// naming the rule and the field it belongs in, or the spec looks
		// accepted while the account is never written.
		It("should reject a grant smuggled into spec.aclRules.keys", func() {
			By("writing a category grant into the key-pattern field")
			user := &redisv1alpha1.RedisUser{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, user)).To(Succeed())
			user.Spec.ACLRules.Keys = []string{"+@read", "~*"}
			Expect(k8sClient.Update(ctx, user)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("not retrying a spec that only an edit can fix")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			By("naming the offending rule and the field it belongs in")
			Expect(k8sClient.Get(ctx, typeNamespacedName, user)).To(Succeed())
			Expect(user.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", "Degraded"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "SpecRejected"),
				HaveField("Message", SatisfyAll(
					ContainSubstring("+@read"),
					ContainSubstring("keys"),
					ContainSubstring("categories"),
				)),
			)))
		})

		It("should report that there are no Redis pods to apply the ACL to", func() {
			By("Reconciling the created resource")
			// envtest runs no kubelet, so the StatefulSet produces no pods and
			// there is nothing to apply the ACL to.
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("keeping the retry interval instead of falling back to rate-limited backoff")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(userRetryInterval))

			By("recording the failure in the status")
			user := &redisv1alpha1.RedisUser{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, user)).To(Succeed())
			Expect(user.Status.Phase).To(Equal("Error"))
			Expect(user.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Message", ContainSubstring("No running Redis pods")),
			)))
		})
	})
})
