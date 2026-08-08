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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

var _ = Describe("RedisBackup Controller", func() {
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
		redisbackup := &redisv1alpha1.RedisBackup{}

		var controllerReconciler *RedisBackupReconciler

		BeforeEach(func() {
			By("creating the RedisSentinel cluster the backup refers to")
			ensureTestSentinel(clusterName, "default")

			By("creating the S3 credentials secret")
			ensureSecret("s3-creds", "default", map[string][]byte{
				"accessKeyId":     []byte("test-access-key"),
				"secretAccessKey": []byte("test-secret-key"),
			})

			By("creating the custom resource for the Kind RedisBackup")
			err := k8sClient.Get(ctx, typeNamespacedName, redisbackup)
			if err != nil && errors.IsNotFound(err) {
				resource := &redisv1alpha1.RedisBackup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: redisv1alpha1.RedisBackupSpec{
						RedisClusterRef: clusterName,
						// A daily schedule keeps the reconcile on the
						// "nothing to do yet, requeue" path, so no Redis pod
						// and no S3 bucket are needed.
						Schedule: "0 2 * * *",
						S3: redisv1alpha1.S3Config{
							Bucket:               "test-bucket",
							Region:               "us-east-1",
							CredentialsSecretRef: "s3-creds",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			controllerReconciler = &RedisBackupReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		AfterEach(func() {
			resource := &redisv1alpha1.RedisBackup{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RedisBackup")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// Reconcile once more so the finalizer added above is removed.
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				err = k8sClient.Get(ctx, typeNamespacedName, resource)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("requeueing until the next scheduled backup window")
			Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

			By("adding the backup finalizer")
			backup := &redisv1alpha1.RedisBackup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, backup)).To(Succeed())
			Expect(backup.Finalizers).To(ContainElement(redisBackupFinalizer))
		})
	})
})
