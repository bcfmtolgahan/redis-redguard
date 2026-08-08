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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// The backup controller watches the object whose status it writes. A failed
// run transitions the CR Running -> Failed; without an event filter each of
// those writes re-enqueues the CR immediately, preempting the RequeueAfter
// retry interval, so a permanently failing backup fires BGSAVE in a hot loop.
// This spec runs the controller under a real manager so the watch path is
// exercised, not simulated.
var _ = Describe("RedisBackup retry cadence", func() {
	const (
		ns         = "backup-cadence"
		backupName = "cadence-backup"
	)

	It("holds a failed backup for the retry interval and still reacts to spec edits", func() {
		By("creating an isolated namespace")
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}))).To(Succeed())

		By("running the backup controller under a namespace-scoped manager")
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		})
		Expect(err).NotTo(HaveOccurred())

		rec := record.NewFakeRecorder(200)
		Expect((&RedisBackupReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: rec,
		}).SetupWithManager(mgr)).To(Succeed())

		// Every failed run emits exactly one BackupFailed warning, so the
		// counter is the number of attempts. The drain goroutine also keeps
		// FakeRecorder.Event from blocking the manager's worker.
		var failedAttempts atomic.Int64
		go func() {
			for event := range rec.Events {
				if strings.Contains(event, "BackupFailed") {
					failedAttempts.Add(1)
				}
			}
		}()

		mctx, mcancel := context.WithCancel(ctx)
		DeferCleanup(mcancel)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())

		By("creating a one-time backup that can only fail: its cluster does not exist")
		backup := &redisv1alpha1.RedisBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: ns},
			Spec: redisv1alpha1.RedisBackupSpec{
				RedisClusterRef: "cadence-missing-cluster",
				S3: redisv1alpha1.S3Config{
					Bucket:               "cadence-bucket",
					Region:               "us-east-1",
					CredentialsSecretRef: "cadence-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())

		By("observing the first failed attempt")
		Eventually(failedAttempts.Load, 15*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">=", 1))

		By("verifying no further attempt preempts the retry interval")
		Consistently(failedAttempts.Load, 4*time.Second, 200*time.Millisecond).
			Should(Equal(int64(1)),
				"more than one attempt means the controller's own status writes re-enqueued it past RequeueAfter")

		By("verifying a spec edit still reconciles promptly")
		latest := &redisv1alpha1.RedisBackup{}
		key := types.NamespacedName{Name: backupName, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, latest)).To(Succeed())
		latest.Spec.S3.Prefix = "after-edit"
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		Eventually(failedAttempts.Load, 15*time.Second, 100*time.Millisecond).
			Should(Equal(int64(2)))

		By("verifying the edit ran once, not in a loop")
		Consistently(failedAttempts.Load, 3*time.Second, 200*time.Millisecond).
			Should(Equal(int64(2)))
	})
})
