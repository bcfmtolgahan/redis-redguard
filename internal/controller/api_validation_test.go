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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// asUnstructured re-encodes a typed CR so a spec field can be given a value the
// Go client's omitempty would drop. A zero on such a field is only reachable
// through the JSON a YAML user actually sends, and a zero is exactly what the
// numeric bounds below have to reject.
func asUnstructured(obj runtime.Object, kind string) *unstructured.Unstructured {
	GinkgoHelper()

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	Expect(err).NotTo(HaveOccurred())
	u := &unstructured.Unstructured{Object: raw}
	u.SetAPIVersion(redisv1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u
}

// newValidationBackup returns a RedisBackup that satisfies every CRD rule, so a
// spec can invalidate exactly one field at a time.
func newValidationBackup(name string) *redisv1alpha1.RedisBackup {
	return &redisv1alpha1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: redisv1alpha1.RedisBackupSpec{
			RedisClusterRef: "test-cluster",
			S3: redisv1alpha1.S3Config{
				Bucket:               "test-bucket",
				Region:               "us-east-1",
				CredentialsSecretRef: "s3-creds",
			},
		},
	}
}

// newValidationRestore returns a RedisRestore that satisfies every CRD rule.
func newValidationRestore(name string) *redisv1alpha1.RedisRestore {
	return &redisv1alpha1.RedisRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: redisv1alpha1.RedisRestoreSpec{
			RedisClusterRef: "test-cluster",
			BackupSource: redisv1alpha1.BackupSource{
				S3: redisv1alpha1.S3Config{
					Bucket:               "test-bucket",
					Region:               "us-east-1",
					CredentialsSecretRef: "s3-creds",
				},
				BackupPath: "backups/test-cluster/backup-1.rdb.gz",
			},
		},
	}
}

// A sentinel timing of zero or below is written verbatim into sentinel.conf,
// where down-after-milliseconds 0 means every instance is permanently down and
// parallel-syncs 0 means no replica is ever resynchronised after a failover.
var _ = Describe("RedisSentinel timing validation", func() {
	DescribeTable("rejects a non-positive sentinel timing",
		func(objName, field string, value int64) {
			u := asUnstructured(newTestSentinel(objName, "default"), "RedisSentinel")
			Expect(unstructured.SetNestedField(u.Object, value, "spec", "sentinelConfig", field)).To(Succeed())

			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(field))
			Expect(err.Error()).To(ContainSubstring("greater than or equal to 1"))
		},
		Entry("down-after zero", "timing-dam-zero", "downAfterMilliseconds", int64(0)),
		Entry("down-after negative", "timing-dam-neg", "downAfterMilliseconds", int64(-1)),
		Entry("failover-timeout zero", "timing-fot-zero", "failoverTimeout", int64(0)),
		Entry("failover-timeout negative", "timing-fot-neg", "failoverTimeout", int64(-5000)),
		Entry("parallel-syncs zero", "timing-ps-zero", "parallelSyncs", int64(0)),
		Entry("parallel-syncs negative", "timing-ps-neg", "parallelSyncs", int64(-1)),
	)
})

// A secret reference is resolved verbatim into a Get in the CR's own namespace,
// so a name that is not a legal DNS-1123 subdomain can never resolve.
var _ = Describe("Secret reference validation", func() {
	It("rejects a redisConfig.auth.secretName that is not a DNS name", func() {
		rs := newTestSentinel("badref-auth", "default")
		rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "Not_A_Secret"}

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("secretName"))
	})

	It("rejects a tls.certificateSecretRef that is not a DNS name", func() {
		rs := newTestSentinel("badref-cert", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "bad name"}

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("certificateSecretRef"))
	})

	It("rejects a tls.caSecretRef that is not a DNS name", func() {
		rs := newTestSentinel("badref-ca", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{
			Enabled:              true,
			CertificateSecretRef: "demo-tls",
			CASecretRef:          "BadCA",
		}

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("caSecretRef"))
	})

	It("rejects a RedisUser passwordSecretRef that is not a DNS name", func() {
		user := &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: "badref-user", Namespace: "default"},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   "test-cluster",
				Username:          "appuser",
				PasswordSecretRef: "Bad_Ref",
			},
		}

		err := k8sClient.Create(ctx, user)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("passwordSecretRef"))
	})

	It("rejects an s3.credentialsSecretRef that is not a DNS name", func() {
		backup := newValidationBackup("badref-s3")
		backup.Spec.S3.CredentialsSecretRef = "Bad_Creds"

		err := k8sClient.Create(ctx, backup)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("credentialsSecretRef"))
	})
})

// A cluster reference is resolved verbatim as a name in the CR's own namespace,
// and RedisBackup joins it into the S3 key prefix that retention prunes under.
var _ = Describe("Cluster reference validation", func() {
	DescribeTable("rejects a RedisBackup cluster reference that is not a DNS name",
		func(objName, ref string) {
			backup := newValidationBackup(objName)
			backup.Spec.RedisClusterRef = ref

			err := k8sClient.Create(ctx, backup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("redisClusterRef"))
		},
		Entry("empty", "clusterref-empty", ""),
		Entry("uppercase", "clusterref-upper", "TestCluster"),
		Entry("parent directory", "clusterref-dotdot", ".."),
		Entry("path separator", "clusterref-slash", "prod/cluster"),
		Entry("wildcard", "clusterref-glob", "*"),
	)

	It("rejects a RedisUser cluster reference that is not a DNS name", func() {
		user := &redisv1alpha1.RedisUser{
			ObjectMeta: metav1.ObjectMeta{Name: "clusterref-user", Namespace: "default"},
			Spec: redisv1alpha1.RedisUserSpec{
				RedisClusterRef:   "Not A Cluster",
				Username:          "appuser",
				PasswordSecretRef: "user-pass",
			},
		}

		err := k8sClient.Create(ctx, user)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("redisClusterRef"))
	})

	It("rejects a RedisRestore cluster reference that is not a DNS name", func() {
		restore := newValidationRestore("clusterref-restore")
		restore.Spec.RedisClusterRef = "Not A Cluster"

		err := k8sClient.Create(ctx, restore)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("redisClusterRef"))
	})
})

// Retention is a count of backups to keep. A negative count has no meaning and
// the pruner reads it as "keep everything", which is the opposite of what a
// user asking for fewer backups intends.
var _ = Describe("RedisBackup retention validation", func() {
	It("rejects a negative retentionPolicy", func() {
		u := asUnstructured(newValidationBackup("retention-negative"), "RedisBackup")
		Expect(unstructured.SetNestedField(u.Object, int64(-1), "spec", "retentionPolicy")).To(Succeed())

		err := k8sClient.Create(ctx, u)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retentionPolicy"))
		Expect(err.Error()).To(ContainSubstring("greater than or equal to 0"))
	})

	It("accepts retentionPolicy 0, which keeps every backup", func() {
		u := asUnstructured(newValidationBackup("retention-zero"), "RedisBackup")
		Expect(unstructured.SetNestedField(u.Object, int64(0), "spec", "retentionPolicy")).To(Succeed())

		Expect(k8sClient.Create(ctx, u)).To(Succeed())
		Expect(k8sClient.Delete(ctx, u)).To(Succeed())
	})
})

// An unparsable schedule silently disables backups forever, so it is refused at
// admission where the shape allows and at reconcile where it does not.
var _ = Describe("RedisBackup schedule validation", func() {
	DescribeTable("rejects a schedule that is not five cron fields",
		func(objName, schedule string) {
			backup := newValidationBackup(objName)
			backup.Spec.Schedule = schedule

			err := k8sClient.Create(ctx, backup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("schedule"))
		},
		Entry("prose", "sched-prose", "every night at two"),
		Entry("four fields", "sched-short", "0 2 * *"),
		Entry("six fields", "sched-long", "0 0 2 * * *"),
		Entry("shell injection", "sched-shell", "0 2 * * *; rm -rf /"),
	)

	DescribeTable("accepts a well-formed schedule",
		func(objName, schedule string) {
			backup := newValidationBackup(objName)
			backup.Spec.Schedule = schedule

			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
		},
		Entry("daily", "sched-daily", "0 2 * * *"),
		Entry("step and list", "sched-step", "*/15 0,12 1-15 * MON-FRI"),
		Entry("timezone prefix", "sched-tz", "CRON_TZ=Europe/Istanbul 0 2 * * *"),
		Entry("one-time backup", "sched-empty", ""),
		Entry("quartz blank field", "sched-qmark", "0 2 ? * MON-FRI"),
	)

	It("degrades on a schedule the cron parser rejects", func() {
		backup := newValidationBackup("sched-out-of-range")
		// Five fields of legal characters, so admission lets it through; the
		// minute field is out of range, which only the parser can see.
		backup.Spec.Schedule = "99 2 * * *"
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, backup))).To(Succeed())
		})

		key := types.NamespacedName{Name: backup.Name, Namespace: backup.Namespace}
		reconciler := &RedisBackupReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder,
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &redisv1alpha1.RedisBackup{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal("Degraded"))
		Expect(got.Status.NextBackupTime).To(BeNil(),
			"a schedule that never fires must not advertise a next run")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

		cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("InvalidSchedule"))
		Expect(cond.Message).To(ContainSubstring("99"))
	})
})

// Neither credential source means the AWS SDK falls back to whatever ambient
// identity the operator pod carries, which is the confused-deputy path the
// destination allowlist exists to gate; both means the Secret silently wins and
// useIAMRole is a lie about which identity signed the request.
var _ = Describe("S3 credential selection validation", func() {
	It("rejects a RedisBackup with neither credentials nor IAM role", func() {
		backup := newValidationBackup("s3-neither")
		backup.Spec.S3.CredentialsSecretRef = ""

		err := k8sClient.Create(ctx, backup)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of s3.credentialsSecretRef or s3.useIAMRole"))
	})

	It("rejects a RedisBackup with both credentials and IAM role", func() {
		backup := newValidationBackup("s3-both")
		backup.Spec.S3.UseIAMRole = true

		err := k8sClient.Create(ctx, backup)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of s3.credentialsSecretRef or s3.useIAMRole"))
	})

	It("accepts a RedisBackup using only the IAM role", func() {
		backup := newValidationBackup("s3-iam")
		backup.Spec.S3.CredentialsSecretRef = ""
		backup.Spec.S3.UseIAMRole = true

		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
	})

	It("accepts a RedisBackup using only tenant credentials", func() {
		backup := newValidationBackup("s3-creds-only")

		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
	})

	It("rejects a RedisRestore with neither credentials nor IAM role", func() {
		restore := newValidationRestore("s3-restore-neither")
		restore.Spec.BackupSource.S3.CredentialsSecretRef = ""

		err := k8sClient.Create(ctx, restore)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of s3.credentialsSecretRef or s3.useIAMRole"))
	})
})

// A one-replica cluster has no failover target: Sentinel can detect the master
// going down and can promote nothing. It stays legal because it is the cheapest
// useful development shape, but it must never be reported as merely Ready.
var _ = Describe("RedisSentinel availability reporting", func() {
	reconcileToStatus := func(name string, replicas int32) *redisv1alpha1.RedisSentinel {
		GinkgoHelper()

		rs := newTestSentinel(name, "default")
		rs.Spec.RedisConfig.Replicas = replicas
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rs))).To(Succeed())
		})

		key := types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}
		reconciler := &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: redisfake.NewFactory(),
		}

		got := &redisv1alpha1.RedisSentinel{}
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			g.Expect(meta.FindStatusCondition(got.Status.Conditions, "HighlyAvailable")).NotTo(BeNil())
		}).Should(Succeed())
		return got
	}

	It("reports a single-replica cluster as not highly available", func() {
		got := reconcileToStatus("ha-single", 1)

		cond := meta.FindStatusCondition(got.Status.Conditions, "HighlyAvailable")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoFailoverTarget"))
		Expect(cond.Message).To(ContainSubstring("no replica to promote"))
	})

	It("reports a multi-replica cluster as highly available", func() {
		got := reconcileToStatus("ha-multi", 3)

		cond := meta.FindStatusCondition(got.Status.Conditions, "HighlyAvailable")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})
})

// Rebuilding the condition slice with a fresh timestamp on every pass destroys
// the only thing a condition timestamp carries and, through the status watch,
// turns every reconcile into the next one.
var _ = Describe("RedisSentinel status churn", func() {
	It("keeps condition timestamps across an unchanged reconcile", func() {
		rs := newTestSentinel("status-churn", "default")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rs))).To(Succeed())
		})

		key := types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}
		reconciler := &RedisSentinelReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Recorder:     testRecorder,
			RedisFactory: redisfake.NewFactory(),
		}

		first := &redisv1alpha1.RedisSentinel{}
		Eventually(func(g Gomega) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Get(ctx, key, first)).To(Succeed())
			g.Expect(meta.FindStatusCondition(first.Status.Conditions, "Available")).NotTo(BeNil())
		}).Should(Succeed())

		Expect(first.Status.ObservedGeneration).To(Equal(first.Generation))

		// metav1.Time is serialized at second granularity, so two writes inside
		// the same second are indistinguishable; back-dating makes a rebuilt
		// slice visible.
		backdated := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
		meta.FindStatusCondition(first.Status.Conditions, "Available").LastTransitionTime = backdated
		Expect(k8sClient.Status().Update(ctx, first)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		second := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, second)).To(Succeed())
		after := meta.FindStatusCondition(second.Status.Conditions, "Available").LastTransitionTime
		Expect(after.Time).To(BeTemporally("==", backdated.Time),
			"an unchanged condition must keep the timestamp of the transition it describes")
	})
})
