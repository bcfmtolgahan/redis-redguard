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

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	custmetrics "github.com/bcfmtolgahan/redis-redguard/pkg/metrics"
)

var _ = Describe("Metrics", func() {
	const ns = "default"

	Context("when a backup run fails", func() {
		const (
			backupName  = "metrics-failed-backup"
			clusterName = "metrics-backup-cluster"
		)

		key := types.NamespacedName{Name: backupName, Namespace: ns}

		AfterEach(func() {
			backup := &redisv1alpha1.RedisBackup{}
			if err := k8sClient.Get(ctx, key, backup); err == nil {
				backup.Finalizers = nil
				Expect(k8sClient.Update(ctx, backup)).To(Succeed())
				Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
			}
			custmetrics.RedisBackupStatus.Reset()
		})

		It("reports redis_backup_status 0 instead of leaving the last success standing", func() {
			By("creating a cluster with no Redis pods, so the run cannot find a source")
			ensureTestSentinel(clusterName, ns)
			ensureSecret("metrics-s3-creds", ns, map[string][]byte{
				"accessKeyId":     []byte("test-access-key"),
				"secretAccessKey": []byte("test-secret-key"),
			})

			By("creating a one-time backup that runs on the first reconcile")
			backup := &redisv1alpha1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: ns},
				Spec: redisv1alpha1.RedisBackupSpec{
					RedisClusterRef: clusterName,
					S3: redisv1alpha1.S3Config{
						Bucket:               "test-bucket",
						Region:               "us-east-1",
						CredentialsSecretRef: "metrics-s3-creds",
					},
				},
			}
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())

			custmetrics.RedisBackupStatus.Reset()

			reconciler := &RedisBackupReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: testRecorder,
			}
			// How the run reports the failure back to the queue is asserted by
			// the requeue specs; this one is only about what it exports.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

			By("exporting exactly one backup series, at zero")
			Expect(testutil.CollectAndCount(custmetrics.RedisBackupStatus)).To(Equal(1))
			Expect(testutil.ToFloat64(
				custmetrics.RedisBackupStatus.WithLabelValues(ns, backupName, clusterName),
			)).To(BeZero())
		})
	})

	Context("when a RedisUser cannot be applied", func() {
		const userName = "metrics-orphan-user"

		key := types.NamespacedName{Name: userName, Namespace: ns}

		AfterEach(func() {
			user := &redisv1alpha1.RedisUser{}
			if err := k8sClient.Get(ctx, key, user); err == nil {
				user.Finalizers = nil
				Expect(k8sClient.Update(ctx, user)).To(Succeed())
				Expect(k8sClient.Delete(ctx, user)).To(Succeed())
			}
			custmetrics.RedisUserACLStatus.Reset()
		})

		It("reports redis_user_acl_status 0", func() {
			ensureSecret("metrics-user-password", ns, map[string][]byte{
				"password": []byte("s3cret"),
			})

			user := &redisv1alpha1.RedisUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: redisv1alpha1.RedisUserSpec{
					RedisClusterRef:   "no-such-cluster",
					Username:          "appuser",
					PasswordSecretRef: "metrics-user-password",
					ACLRules: redisv1alpha1.ACLRule{
						Keys:     []string{"~app:*"},
						Commands: []string{"+get", "+set"},
					},
					Enabled: ptr.To(true),
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			custmetrics.RedisUserACLStatus.Reset()

			reconciler := &RedisUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			// How the failure is reported back to the queue is asserted by the
			// requeue specs; this one is only about what it exports.
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

			Expect(testutil.CollectAndCount(custmetrics.RedisUserACLStatus)).To(Equal(1))
			Expect(testutil.ToFloat64(
				custmetrics.RedisUserACLStatus.WithLabelValues(ns, userName, "appuser"),
			)).To(BeZero())
		})
	})

	Context("when a CR is deleted", func() {
		It("drops every series the RedisSentinel owned", func() {
			custmetrics.RedisClusterInfo.Reset()
			custmetrics.RedisUsedMemory.Reset()
			DeferCleanup(func() {
				custmetrics.RedisClusterInfo.Reset()
				custmetrics.RedisUsedMemory.Reset()
			})

			custmetrics.RedisClusterInfo.WithLabelValues(ns, "metrics-doomed").Set(1)
			custmetrics.RedisUsedMemory.WithLabelValues(ns, "metrics-doomed", "metrics-doomed-redis-0").Set(1024)

			now := metav1.Now()
			rs := &redisv1alpha1.RedisSentinel{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "metrics-doomed",
					Namespace:         ns,
					DeletionTimestamp: &now,
				},
			}

			reconciler := &RedisSentinelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.handleDeletion(ctx, rs)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.CollectAndCount(custmetrics.RedisClusterInfo)).To(BeZero())
			Expect(testutil.CollectAndCount(custmetrics.RedisUsedMemory)).To(BeZero())
		})

		It("drops the RedisBackup series", func() {
			custmetrics.RedisBackupStatus.Reset()
			DeferCleanup(custmetrics.RedisBackupStatus.Reset)

			custmetrics.RedisBackupStatus.WithLabelValues(ns, "metrics-doomed-backup", "c").Set(1)

			now := metav1.Now()
			backup := &redisv1alpha1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "metrics-doomed-backup",
					Namespace:         ns,
					DeletionTimestamp: &now,
				},
			}

			reconciler := &RedisBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.handleDeletion(ctx, backup)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.CollectAndCount(custmetrics.RedisBackupStatus)).To(BeZero())
		})

		It("drops the RedisUser series", func() {
			custmetrics.RedisUserACLStatus.Reset()
			DeferCleanup(custmetrics.RedisUserACLStatus.Reset)

			custmetrics.RedisUserACLStatus.WithLabelValues(ns, "metrics-doomed-user", "appuser").Set(1)

			now := metav1.Now()
			user := &redisv1alpha1.RedisUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "metrics-doomed-user",
					Namespace:         ns,
					DeletionTimestamp: &now,
				},
			}

			reconciler := &RedisUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.handleDeletion(ctx, user)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.CollectAndCount(custmetrics.RedisUserACLStatus)).To(BeZero())
		})
	})
})
