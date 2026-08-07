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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

var _ = Describe("RedisSentinel Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		redissentinel := &redisv1alpha1.RedisSentinel{}

		var controllerReconciler *RedisSentinelReconciler

		BeforeEach(func() {
			By("creating the custom resource for the Kind RedisSentinel")
			err := k8sClient.Get(ctx, typeNamespacedName, redissentinel)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, newTestSentinel(resourceName, "default"))).To(Succeed())
			}

			controllerReconciler = &RedisSentinelReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: testRecorder,
			}
		})

		AfterEach(func() {
			resource := &redisv1alpha1.RedisSentinel{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RedisSentinel")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// The reconciler adds a finalizer, so the object only disappears
			// once a reconcile of the deletion has removed it again. The
			// Manager-owned controller does that too; driving it here keeps the
			// cleanup deterministic.
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				err = k8sClient.Get(ctx, typeNamespacedName, resource)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			// The same reconciler also runs inside the Manager, so a manual
			// call can lose an optimistic-concurrency race (409 Conflict /
			// AlreadyExists). Retry until the reconcile settles.
			var result reconcile.Result
			Eventually(func() error {
				var err error
				result, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				return err
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())

			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})
	})
})
