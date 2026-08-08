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
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

const (
	restoreNS         = "default"
	restoreCluster    = "restore-cluster"
	restoreName       = "test-restore"
	restoreMasterIP   = "10.244.0.10"
	restoreMasterAddr = restoreMasterIP + ":6379"
	// restoreSentinelIPBase + ordinal is the IP of each seeded sentinel pod.
	restoreSentinelIPBase = "10.244.0.2"
)

type restoreFixture struct {
	r       *RedisRestoreReconciler
	factory *redisfake.Factory
	c       client.Client
}

// newRestoreFixture builds a reconciler over a fake API server seeded with a
// RedisSentinel whose status names a running, ready master pod, plus the
// RedisRestore shaped by mutate. Extra objects are seeded verbatim.
func newRestoreFixture(t *testing.T, mutate func(*redisv1alpha1.RedisRestore), extra ...client.Object) *restoreFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	rs := newTestSentinel(restoreCluster, restoreNS)
	// Status carries the master address as the sentinel controller writes it:
	// the pod IP and port reported by SENTINEL get-master-addr-by-name.
	rs.Status.MasterNode = restoreMasterAddr

	masterPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restoreCluster + "-redis-0",
			Namespace: restoreNS,
			Labels: map[string]string{
				"app.kubernetes.io/instance":  restoreCluster,
				"app.kubernetes.io/component": "redis",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: restoreMasterIP,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	sentinelPods := make([]client.Object, 3)
	for i := range sentinelPods {
		sentinelPods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-sentinel-%d", restoreCluster, i),
				Namespace: restoreNS,
				Labels: map[string]string{
					"app.kubernetes.io/instance":  restoreCluster,
					"app.kubernetes.io/component": "sentinel",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: fmt.Sprintf("%s%d", restoreSentinelIPBase, i),
			},
		}
	}

	restore := &redisv1alpha1.RedisRestore{
		ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: restoreNS, Generation: 1},
		Spec: redisv1alpha1.RedisRestoreSpec{
			RedisClusterRef: restoreCluster,
			BackupSource: redisv1alpha1.BackupSource{
				S3: redisv1alpha1.S3Config{
					Bucket:               "test-bucket",
					Region:               "us-east-1",
					CredentialsSecretRef: "s3-creds",
				},
				BackupPath: "backups/restore-cluster/backup-1.rdb.gz",
			},
		},
	}
	if mutate != nil {
		mutate(restore)
	}

	objects := append([]client.Object{rs, masterPod, restore}, sentinelPods...)
	objects = append(objects, extra...)
	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&redisv1alpha1.RedisRestore{}, &redisv1alpha1.RedisBackup{}).
		Build()

	factory := redisfake.NewFactory()
	factory.SetMaster(restoreMasterAddr)

	return &restoreFixture{
		r:       &RedisRestoreReconciler{Client: c, Scheme: scheme, RedisFactory: factory},
		factory: factory,
		c:       c,
	}
}

func (fx *restoreFixture) reconcile(t *testing.T) (ctrl.Result, error) {
	t.Helper()
	return fx.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: restoreName, Namespace: restoreNS},
	})
}

func (fx *restoreFixture) get(t *testing.T) *redisv1alpha1.RedisRestore {
	t.Helper()
	restore := &redisv1alpha1.RedisRestore{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Name: restoreName, Namespace: restoreNS}, restore); err != nil {
		t.Fatal(err)
	}
	return restore
}

// assertNoDestructiveCalls fails when the factory saw any call that changes
// cluster state or availability.
func assertNoDestructiveCalls(t *testing.T, factory *redisfake.Factory) {
	t.Helper()
	for _, call := range factory.Calls() {
		for _, destructive := range []string{"ShutdownNoSave", "SetMasterOptionAll", "ConfigSet", "SlaveOf"} {
			if strings.Contains(call, destructive) {
				t.Errorf("destructive call issued: %s", call)
			}
		}
	}
}

// payloadCheckExec answers the staged-payload presence check with the given
// stdout and rejects any exec that streams data (staging must not run).
func payloadCheckExec(t *testing.T, answer string) podExecFn {
	return func(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdin io.Reader) (string, string, error) {
		if stdin != nil {
			t.Errorf("unexpected staging exec during this phase: %v", command)
		}
		return answer + "\n", "", nil
	}
}

func TestRestore_RefusesNonEmptyClusterWithoutForce(t *testing.T) {
	fx := newRestoreFixture(t, nil)
	// Keys on a non-zero DB index only: a DB-0-only check would miss them.
	fx.factory.SetKeyspace(restoreMasterAddr, map[int]int64{2: 5})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("initializing reconcile: %v", err)
	}
	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("pending reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Fatalf("phase = %q, want Failed: a populated cluster must not be overwritten without force", restore.Status.Phase)
	}
	if !strings.Contains(restore.Status.Message, "existing data") {
		t.Errorf("message %q does not explain the refusal", restore.Status.Message)
	}
	assertNoDestructiveCalls(t, fx.factory)
}

func TestRestore_PendingEmptyClusterReachesS3Preflight(t *testing.T) {
	// The cluster is empty, so the data gate passes; the S3 credentials secret
	// is missing, so the preflight fails transiently and the phase must stay
	// Pending for a retry rather than jumping ahead or failing terminally.
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhasePending
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})

	if _, err := fx.reconcile(t); err == nil {
		t.Fatal("expected a transient error from the S3 preflight, got nil")
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhasePending {
		t.Fatalf("phase = %q, want Pending: the cluster must not be touched before the backup object is confirmed", restore.Status.Phase)
	}
	assertNoDestructiveCalls(t, fx.factory)
}

func TestRestore_IsIdempotentAfterCompletion(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseCompleted
		r.Status.ObservedGeneration = 1
	})

	res, err := fx.reconcile(t)
	if err != nil {
		t.Fatalf("reconcile of a completed restore: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("completed restore requeued: %+v", res)
	}
	if calls := fx.factory.Calls(); len(calls) != 0 {
		t.Errorf("completed restore dialed Redis again: %v", calls)
	}
	if restore := fx.get(t); restore.Status.Phase != redisv1alpha1.RestorePhaseCompleted {
		t.Errorf("phase changed to %q", restore.Status.Phase)
	}
}

func TestRestore_FailedIsTerminalForSameGeneration(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseFailed
		r.Status.ObservedGeneration = 1
	})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("reconcile of a failed restore: %v", err)
	}
	if calls := fx.factory.Calls(); len(calls) != 0 {
		t.Errorf("failed restore dialed Redis again: %v", calls)
	}
}

func TestRestore_SpecChangeRerunsCompletedRestore(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Generation = 2
		r.Status.Phase = redisv1alpha1.RestorePhaseCompleted
		r.Status.ObservedGeneration = 1
	})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("reconcile after spec change: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhasePending {
		t.Fatalf("phase = %q, want Pending: a changed spec must start a fresh run", restore.Status.Phase)
	}
}

func TestRestore_SpecChangeClearsStaleQuiesceBreadcrumb(t *testing.T) {
	// The previous run failed without lifting the quiesce, so its breadcrumb
	// survived. Carrying it into the new run would mark the sentinels as
	// quiesced by a shutdown this run never issued: the payload-lost re-stage
	// check reads it as "already shut down", and the sentinel reconciler keeps
	// honouring a quiesce nobody holds.
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Generation = 2
		r.Status.Phase = redisv1alpha1.RestorePhaseFailed
		r.Status.ObservedGeneration = 1
		r.Status.QuiescedDownAfterMilliseconds = 5000
	})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("reconcile after spec change: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhasePending {
		t.Fatalf("phase = %q, want Pending", restore.Status.Phase)
	}
	if restore.Status.QuiescedDownAfterMilliseconds != 0 {
		t.Errorf("stale quiesce breadcrumb %d carried into the new run", restore.Status.QuiescedDownAfterMilliseconds)
	}
}

func TestRestore_RestoringQuiescesSentinelsBeforeShutdown(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})
	fx.r.PodExec = payloadCheckExec(t, "present")

	res, err := fx.reconcile(t)
	if err != nil {
		t.Fatalf("restoring reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("restart must be awaited via requeue, got %+v", res)
	}

	if got := fx.factory.MasterOptions()["down-after-milliseconds"]; got != "600000" {
		t.Errorf("sentinels not quiesced, down-after-milliseconds = %q", got)
	}
	restore := fx.get(t)
	if restore.Status.QuiescedDownAfterMilliseconds != 5000 {
		t.Errorf("original down-after value not recorded, got %d", restore.Status.QuiescedDownAfterMilliseconds)
	}

	calls := fx.factory.Calls()
	quiesceIdx, shutdownIdx := -1, -1
	for i, call := range calls {
		if strings.Contains(call, "SetMasterOptionAll") && quiesceIdx == -1 {
			quiesceIdx = i
		}
		if strings.Contains(call, "ShutdownNoSave") && shutdownIdx == -1 {
			shutdownIdx = i
		}
	}
	if shutdownIdx == -1 {
		t.Fatalf("master was never shut down, calls: %v", calls)
	}
	if quiesceIdx == -1 || quiesceIdx > shutdownIdx {
		t.Fatalf("shutdown before sentinel quiesce would let a stale replica be promoted; calls: %v", calls)
	}
}

func TestRestore_RestoringQuiescesEverySentinelPod(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})
	fx.r.PodExec = payloadCheckExec(t, "present")

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("restoring reconcile: %v", err)
	}

	// The quiesce must land on the same sentinels the sentinel reconciler
	// converges: the running pods. A write to any other address set leaves a
	// live sentinel on the low threshold.
	ops := fx.factory.Ops()
	for i := 0; i < 3; i++ {
		addr := fmt.Sprintf("%s%d:26379", restoreSentinelIPBase, i)
		found := false
		for _, op := range ops {
			if strings.Contains(op, addr) && strings.Contains(op, "down-after-milliseconds=600000") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sentinel pod %s never saw the quiesce; ops: %v", addr, ops)
		}
	}
}

func TestRestore_RestoringRefusesPartialSentinelQuiesce(t *testing.T) {
	// One sentinel pod is gone. Quiescing only the survivors leaves a sentinel
	// on the low down-after-milliseconds; if it comes back mid-restart it
	// still reads the controlled shutdown as a master failure. The restore
	// must hold off the shutdown until every sentinel can be quiesced.
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})
	fx.r.PodExec = payloadCheckExec(t, "present")

	gone := &corev1.Pod{}
	gone.Name, gone.Namespace = restoreCluster+"-sentinel-2", restoreNS
	if err := fx.c.Delete(context.Background(), gone); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.reconcile(t); err == nil {
		t.Fatal("expected an error while a sentinel cannot be quiesced, got nil")
	}
	for _, call := range fx.factory.Calls() {
		if strings.Contains(call, "ShutdownNoSave") {
			t.Errorf("master shut down with a sentinel left unquiesced: %v", fx.factory.Calls())
		}
		if strings.Contains(call, "SetMasterOptionAll") {
			t.Errorf("partial quiesce written: %v", fx.factory.Calls())
		}
	}
}

func TestRestore_RestoringPayloadConsumedMovesToVerifying(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
	})
	fx.r.PodExec = payloadCheckExec(t, "absent")

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("restoring reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseVerifying {
		t.Fatalf("phase = %q, want Verifying after the payload was consumed", restore.Status.Phase)
	}
	for _, call := range fx.factory.Calls() {
		if strings.Contains(call, "ShutdownNoSave") {
			t.Errorf("master shut down again after it already restarted: %v", fx.factory.Calls())
		}
	}
}

func TestRestore_RestoringPayloadLostWithoutShutdownRestages(t *testing.T) {
	// The payload is gone but this restore never issued a shutdown: the pod
	// restarted or failed over on its own and consumed or dropped the staged
	// file. The restore must stage again, not verify a dataset it never loaded.
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})
	fx.r.PodExec = payloadCheckExec(t, "absent")

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("restoring reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseDownloading {
		t.Fatalf("phase = %q, want Downloading to re-stage the lost payload", restore.Status.Phase)
	}
	assertNoDestructiveCalls(t, fx.factory)
}

func TestRestore_VerifyingChecksActualKeyCount(t *testing.T) {
	// The master is up and answers, but its keyspace is empty: the dump was
	// not loaded. Completing here would report success on a wiped cluster.
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseVerifying
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
		r.Status.RestoredDataSize = 100
	})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("verifying reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Fatalf("phase = %q, want Failed: an empty keyspace after a restore is a failed restore", restore.Status.Phase)
	}
	if !strings.Contains(restore.Status.Message, "empty") {
		t.Errorf("message %q does not name the empty dataset", restore.Status.Message)
	}
}

func TestRestore_VerifyingSucceedsRestoresSentinelAndAOF(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseVerifying
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
		r.Status.RestoredDataSize = 100
	})
	fx.factory.SetKeyspace(restoreMasterAddr, map[int]int64{0: 42})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("verifying reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseCompleted {
		t.Fatalf("phase = %q (message %q), want Completed", restore.Status.Phase, restore.Status.Message)
	}
	if restore.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", restore.Status.ObservedGeneration)
	}
	if restore.Status.CompletionTime == nil {
		t.Error("completionTime not set")
	}
	if restore.Status.QuiescedDownAfterMilliseconds != 0 {
		t.Error("quiesce breadcrumb not cleared after the sentinels were restored")
	}
	if got := fx.factory.MasterOptions()["down-after-milliseconds"]; got != "5000" {
		t.Errorf("sentinel down-after-milliseconds = %q, want the recorded 5000 restored", got)
	}
	if got := fx.factory.Configs(restoreMasterAddr)["appendonly"]; got != "yes" {
		t.Errorf("appendonly = %q, want yes: without it the restored dataset has no durability", got)
	}
}

func TestRestore_VerifyingComparesBackupKeyCountMismatch(t *testing.T) {
	recorded := int64(100)
	backup := &redisv1alpha1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "source-backup", Namespace: restoreNS},
		Spec: redisv1alpha1.RedisBackupSpec{
			RedisClusterRef: restoreCluster,
			S3:              redisv1alpha1.S3Config{Bucket: "test-bucket"},
		},
		Status: redisv1alpha1.RedisBackupStatus{
			BackupLocation: "s3://test-bucket/backups/restore-cluster/backup-1.rdb.gz",
			KeyCount:       &recorded,
		},
	}
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseVerifying
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
	}, backup)
	fx.factory.SetKeyspace(restoreMasterAddr, map[int]int64{0: 42})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("verifying reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Fatalf("phase = %q, want Failed on a key count mismatch", restore.Status.Phase)
	}
	if !strings.Contains(restore.Status.Message, "42") || !strings.Contains(restore.Status.Message, "100") {
		t.Errorf("message %q does not state both counts", restore.Status.Message)
	}
}

func TestRestore_VerifyingComparesBackupKeyCountMatch(t *testing.T) {
	recorded := int64(42)
	backup := &redisv1alpha1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "source-backup", Namespace: restoreNS},
		Spec: redisv1alpha1.RedisBackupSpec{
			RedisClusterRef: restoreCluster,
			S3:              redisv1alpha1.S3Config{Bucket: "test-bucket"},
		},
		Status: redisv1alpha1.RedisBackupStatus{
			BackupLocation: "s3://test-bucket/backups/restore-cluster/backup-1.rdb.gz",
			KeyCount:       &recorded,
		},
	}
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseVerifying
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
	}, backup)
	fx.factory.SetKeyspace(restoreMasterAddr, map[int]int64{0: 42})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("verifying reconcile: %v", err)
	}

	if restore := fx.get(t); restore.Status.Phase != redisv1alpha1.RestorePhaseCompleted {
		t.Fatalf("phase = %q (message %q), want Completed when the counts match", restore.Status.Phase, restore.Status.Message)
	}
}

func TestRestore_VerifyingFailsWhenMasterRoleLost(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseVerifying
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
		r.Status.QuiescedDownAfterMilliseconds = 5000
	})
	// Another node took over: the restored pod is now a replica, so a failover
	// happened mid-restore and full-resynced the restored dataset away.
	fx.factory.SetMaster("10.244.0.99:6379")
	fx.factory.SetKeyspace(restoreMasterAddr, map[int]int64{0: 42})

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("verifying reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Fatalf("phase = %q, want Failed when the restored pod lost the master role", restore.Status.Phase)
	}
	if !strings.Contains(restore.Status.Message, "master") {
		t.Errorf("message %q does not name the role loss", restore.Status.Message)
	}
}
