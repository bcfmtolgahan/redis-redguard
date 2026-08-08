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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/tlsutil"
)

// The restore replaces the master's dataset in place. Redis 7 with appendonly
// yes never loads dump.rdb, and the configured save points overwrite it on a
// normal shutdown, so the payload is staged under a name the running server
// does not touch and the init script performs the swap on the restart that
// SHUTDOWN NOSAVE triggers: it parks the old AOF, moves the payload to
// dump.rdb and boots with AOF off. The controller re-enables AOF only after
// the loaded dataset is verified. Sentinel failure detection is raised before
// the shutdown so no stale replica is promoted while the master reloads.
const (
	// restorePayloadPath is the staging name the init script consumes.
	restorePayloadPath = "/data/redguard-restore.rdb"
	// preRestoreAOFPath is where the init script parks the previous AOF.
	preRestoreAOFPath = "/data/appendonlydir.pre-restore"
	// quiesceDownAfterMilliseconds keeps the sentinels from reading the
	// controlled restart as a master failure.
	quiesceDownAfterMilliseconds = "600000"
	// restoreTimeout bounds a restore run end to end.
	restoreTimeout = 30 * time.Minute
	// restartPollInterval paces the wait for the master container restart.
	restartPollInterval = 5 * time.Second
)

// podExecFn runs a command in a pod container, streaming stdin when non-nil.
// It is a seam so unit tests can drive the exec-dependent restore steps.
type podExecFn func(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdin io.Reader) (stdout, stderr string, err error)

// RedisRestoreReconciler reconciles a RedisRestore object
type RedisRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RESTConfig is required by the pod-exec path that writes the RDB into a
	// Redis pod; without it every restore fails.
	RESTConfig *rest.Config
	Recorder   record.EventRecorder
	// RedisFactory builds Redis clients; nil means DefaultFactory.
	RedisFactory redisclient.Factory
	// PodExec runs commands inside Redis pods; nil means the SPDY executor.
	PodExec podExecFn
	// AllowedBuckets and AllowedEndpoints are the same allowlists the backup
	// controller enforces (--allowed-backup-buckets, --allowed-backup-endpoints).
	// With spec.backupSource.s3.useIAMRole the GetObject is signed with the
	// operator's own identity, so an unrestricted restore reads any object that
	// identity can reach; empty AllowedBuckets disables the IAM-role path.
	AllowedBuckets   []string
	AllowedEndpoints []string
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisbackups,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch

func (r *RedisRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	redisRestore := &redisv1alpha1.RedisRestore{}
	if err := r.Get(ctx, req.NamespacedName, redisRestore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisRestore")
		return ctrl.Result{}, err
	}

	// Terminal phases are sticky per spec generation: a requeue or operator
	// restart must not run a finished restore again; only a spec change may.
	if redisRestore.Status.Phase == redisv1alpha1.RestorePhaseCompleted ||
		redisRestore.Status.Phase == redisv1alpha1.RestorePhaseFailed {
		if redisRestore.Status.ObservedGeneration == redisRestore.Generation {
			return ctrl.Result{}, nil
		}
		redisRestore.Status.StartTime = &metav1.Time{Time: time.Now()}
		redisRestore.Status.CompletionTime = nil
		redisRestore.Status.Duration = ""
		// A breadcrumb surviving a failed unquiesce belongs to the previous
		// run: carried over, it reads as "this run already shut the master
		// down" and holds the sentinel reconciler off a quiesce nobody owns.
		// The verification fields likewise describe the previous run.
		redisRestore.Status.QuiescedDownAfterMilliseconds = 0
		redisRestore.Status.DatasetReplaced = false
		redisRestore.Status.ExpectedKeyCount = 0
		redisRestore.Status.RestoredKeyCount = 0
		if err := r.updateStatus(ctx, redisRestore, redisv1alpha1.RestorePhasePending, "Spec changed; starting a new restore run", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Refuse disallowed destinations before any Redis or S3 side effect. The
	// check runs before every phase, so a violating restore never starts and an
	// operator restarted with a tighter allowlist stops an in-flight one.
	if err := r.validateRestoreDestination(redisRestore); err != nil {
		logger.Error(err, "Restore destination not allowed")
		return r.markDestinationRejected(ctx, redisRestore, err)
	}

	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisRestore.Spec.RedisClusterRef,
		Namespace: redisRestore.Namespace,
	}, redisSentinel); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failRestore(ctx, redisRestore, nil, "RedisSentinel cluster not found: "+redisRestore.Spec.RedisClusterRef)
		}
		logger.Error(err, "Failed to get RedisSentinel cluster")
		return ctrl.Result{}, err
	}

	if redisRestore.Status.Phase == "" {
		redisRestore.Status.StartTime = &metav1.Time{Time: time.Now()}
		if err := r.updateStatus(ctx, redisRestore, redisv1alpha1.RestorePhasePending, "Starting restore operation", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch redisRestore.Status.Phase {
	case redisv1alpha1.RestorePhasePending:
		return r.handlePendingPhase(ctx, redisRestore, redisSentinel)
	case redisv1alpha1.RestorePhaseDownloading:
		return r.handleDownloadingPhase(ctx, redisRestore, redisSentinel)
	case redisv1alpha1.RestorePhaseRestoring:
		return r.handleRestoringPhase(ctx, redisRestore, redisSentinel)
	case redisv1alpha1.RestorePhaseVerifying:
		return r.handleVerifyingPhase(ctx, redisRestore, redisSentinel)
	}

	return ctrl.Result{}, nil
}

// handlePendingPhase gates the restore: it refuses a populated cluster unless
// forced and confirms the backup object exists before anything is touched.
func (r *RedisRestoreReconciler) handlePendingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !restore.Spec.SkipDataCheck && !restore.Spec.Force {
		// INFO keyspace covers every database; DBSIZE would only see DB 0 and
		// wave through a cluster populated on another index.
		total, err := r.clusterKeyCount(ctx, rs)
		if err != nil {
			logger.Error(err, "Failed to check cluster data")
			return ctrl.Result{}, err
		}
		if total > 0 {
			return r.failRestore(ctx, restore, rs, fmt.Sprintf(
				"Cluster has existing data (%d keys across all databases); set spec.force=true to overwrite", total))
		}
	}

	object := "s3://" + restore.Spec.BackupSource.S3.Bucket + "/" + restore.Spec.BackupSource.BackupPath
	size, err := r.preflightBackupObject(ctx, restore)
	if err != nil {
		var notFound *s3types.NotFound
		if errors.As(err, &notFound) {
			return r.failRestore(ctx, restore, rs, "Backup object not found: "+object)
		}
		logger.Error(err, "Backup object preflight failed")
		return ctrl.Result{}, err
	}
	if size == 0 {
		return r.failRestore(ctx, restore, rs, "Backup object is empty: "+object)
	}

	if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseDownloading, "Downloading backup from S3", 0); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handleDownloadingPhase downloads and validates the payload, then stages it
// on the master's volume. The running server is not disturbed: the staging
// name is one it never reads or writes.
func (r *RedisRestoreReconciler) handleDownloadingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	data, err := r.downloadBackup(ctx, restore)
	if err != nil {
		logger.Error(err, "Failed to download backup")
		return ctrl.Result{}, err
	}

	var rdbData []byte
	if restore.Spec.BackupSource.Compressed {
		gzipReader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return r.failRestore(ctx, restore, rs, "Backup is not valid gzip data: "+err.Error())
		}
		rdbData, err = io.ReadAll(gzipReader)
		if cerr := gzipReader.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return r.failRestore(ctx, restore, rs, "Failed to decompress backup: "+err.Error())
		}
	} else {
		rdbData = data
	}

	if len(rdbData) < 5 || string(rdbData[:5]) != "REDIS" {
		return r.failRestore(ctx, restore, rs, "Downloaded object is not an RDB file")
	}

	if err := r.stageRestorePayload(ctx, rs, rdbData); err != nil {
		logger.Error(err, "Failed to stage restore payload")
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseRestoring,
		"Restore payload staged; restarting the master to load it", int64(len(rdbData))); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handleRestoringPhase quiesces the sentinels and shuts the master down so the
// init script swaps the staged payload in, then waits for the payload to be
// consumed. Every step keys on observable pod state, so a requeue or operator
// restart re-enters safely.
func (r *RedisRestoreReconciler) handleRestoringPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if restoreExpired(restore) {
		return r.failRestore(ctx, restore, rs, "Restore did not complete within "+restoreTimeout.String())
	}

	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		logger.V(1).Info("Master pod not available yet", "error", err.Error())
		return ctrl.Result{RequeueAfter: restartPollInterval}, nil
	}

	present, err := r.restorePayloadPresent(ctx, masterPod)
	if err != nil {
		// The container is down between the shutdown and the kubelet restart;
		// exec failures here are polling, not errors.
		logger.V(1).Info("Cannot check restore payload; container may be restarting", "error", err.Error())
		return ctrl.Result{RequeueAfter: restartPollInterval}, nil
	}

	if !present {
		if restore.Status.QuiescedDownAfterMilliseconds == 0 {
			// The payload vanished but this restore never shut the master
			// down: the pod restarted or failed over on its own. Stage again
			// rather than verifying a dataset that was never loaded.
			if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseDownloading,
				"Restore payload lost before the restart; staging it again", 0); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		// The payload is gone after this restore's shutdown: the master now
		// serves the restored data. Recorded before verification so every
		// terminal path from here knows the old dataset no longer exists.
		restore.Status.DatasetReplaced = true
		if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseVerifying,
			"Master restarted; verifying the restored dataset", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve client TLS config: %w", err)
	}

	// The value to restore is persisted before SENTINEL SET: if the process
	// dies in between, the breadcrumb still gets the setting restored later;
	// the reverse order could leave the sentinels quiesced forever. The
	// breadcrumb doubles as the quiesce marker the sentinel reconciler
	// honours: while it is set and this restore is not terminal, monitor
	// drift repair leaves down-after-milliseconds alone instead of lowering
	// it mid-restart and letting a failover wipe the restored dataset.
	if restore.Status.QuiescedDownAfterMilliseconds == 0 {
		value := rs.Spec.SentinelConfig.DownAfterMilliseconds
		if value == 0 {
			value = 5000
		}
		restore.Status.QuiescedDownAfterMilliseconds = value
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.setSentinelDownAfter(ctx, rs, adminPassword, tlsCfg, quiesceDownAfterMilliseconds, true); err != nil {
		return ctrl.Result{}, fmt.Errorf("quiesce sentinels: %w", err)
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()
	// NOSAVE, or the exiting server would overwrite dump.rdb with the old
	// dataset right before the init script swaps the payload in.
	if err := redisClient.ShutdownNoSave(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("shut down master: %w", err)
	}

	logger.Info("Master shut down to load the restore payload", "pod", masterPod.Name)
	return ctrl.Result{RequeueAfter: restartPollInterval}, nil
}

// handleVerifyingPhase confirms the restarted master actually serves the
// restored dataset before durability and failover detection are switched back
// on and the restore is declared complete.
func (r *RedisRestoreReconciler) handleVerifyingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if restoreExpired(restore) {
		return r.failRestore(ctx, restore, rs, "Restore did not complete within "+restoreTimeout.String())
	}

	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve client TLS config: %w", err)
	}
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return ctrl.Result{}, err
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	if err := redisClient.Ping(ctx); err != nil {
		// Still restarting or loading the dump; poll until the deadline.
		logger.V(1).Info("Master not answering yet", "error", err.Error())
		return ctrl.Result{RequeueAfter: restartPollInterval}, nil
	}

	isMaster, err := redisClient.IsMaster(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isMaster {
		return r.failRestore(ctx, restore, rs, fmt.Sprintf(
			"Pod %s lost the master role during the restore; a failover replaced the restored dataset", masterPod.Name))
	}

	info, err := redisClient.GetKeyspaceInfo(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	total, err := sumKeyspaceKeys(info)
	if err != nil {
		return ctrl.Result{}, err
	}

	restore.Status.RestoredKeyCount = total
	if total == 0 {
		return r.failRestore(ctx, restore, rs, "Restore produced an empty dataset; the dump was not loaded")
	}

	// The backup records its count after BGSAVE completes, so a cluster taking
	// writes drifts from the snapshot by design; a strict comparison would
	// fail every such restore after the dataset was already replaced
	// cluster-wide. Both counts go on record and a mismatch only warns.
	driftNote := ""
	if expected := r.lookupBackupKeyCount(ctx, restore); expected != nil {
		restore.Status.ExpectedKeyCount = *expected
		if total != *expected {
			driftNote = fmt.Sprintf("; the backup recorded %d keys (counted after BGSAVE, so writes during the backup drift it)", *expected)
			recordEvent(r.Recorder, restore, corev1.EventTypeWarning, "RestoreKeyCountDrift",
				fmt.Sprintf("Restored %d keys, the backup recorded %d", total, *expected))
		}
	}

	// Durability returns only after the restored dataset is in memory:
	// enabling AOF rewrites it from the current dataset.
	if err := redisClient.ConfigSet(ctx, "appendonly", "yes"); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-enable appendonly: %w", err)
	}

	r.cleanupPreRestoreAOF(ctx, masterPod)

	// Completing while the sentinels cannot detect failures would hide a
	// degraded cluster; retry until the deadline instead.
	if err := r.restoreSentinelDownAfter(ctx, restore, rs, adminPassword, tlsCfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("restore sentinel settings: %w", err)
	}

	if restore.Status.StartTime != nil {
		restore.Status.Duration = time.Since(restore.Status.StartTime.Time).String()
	}
	restore.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	restore.Status.RestoredFrom = restore.Spec.BackupSource.BackupPath

	message := fmt.Sprintf("Restore completed: %d keys loaded", total) + driftNote
	if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseCompleted, message, 0); err != nil {
		return ctrl.Result{}, err
	}
	recordEvent(r.Recorder, restore, corev1.EventTypeNormal, "RestoreCompleted", message)
	logger.Info("Restore completed", "keys", total, "duration", restore.Status.Duration)

	return ctrl.Result{}, nil
}

// validateRestoreDestination applies the shared destination policy to the
// read path; see validateS3Destination for the confused-deputy rationale.
func (r *RedisRestoreReconciler) validateRestoreDestination(restore *redisv1alpha1.RedisRestore) error {
	return validateS3Destination(&restore.Spec.BackupSource.S3, r.AllowedBuckets, r.AllowedEndpoints)
}

// markDestinationRejected records a destination-policy refusal as a terminal
// failure under its own reason, distinguishable from a run that failed. The
// Failed phase is sticky per generation, so the sentinel reconciler resumes
// converging any quiesce a stopped in-flight run left behind.
func (r *RedisRestoreReconciler) markDestinationRejected(ctx context.Context, restore *redisv1alpha1.RedisRestore, cause error) (ctrl.Result, error) {
	restore.Status.Phase = redisv1alpha1.RestorePhaseFailed
	restore.Status.Message = cause.Error()
	restore.Status.ObservedGeneration = restore.Generation
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: restore.Generation,
		Reason:             "DestinationNotAllowed",
		Message:            cause.Error(),
	})
	recordEvent(r.Recorder, restore, corev1.EventTypeWarning, "DestinationNotAllowed", cause.Error())
	if err := r.Status().Update(ctx, restore); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisRestore status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reenableAppendonly turns AOF back on, on the current master. The restarted
// master boots with AOF off so the staged payload survives the load; leaving
// it off gives the dataset a durability window of up to the next rewrite.
func (r *RedisRestoreReconciler) reenableAppendonly(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return fmt.Errorf("resolve client TLS config: %w", err)
	}
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()
	return redisClient.ConfigSet(ctx, "appendonly", "yes")
}

// failRestore lifts the sentinel quiesce when possible and records a terminal
// failure. No data-destroying step may follow a failRestore. When the failure
// comes after the payload was consumed, the cluster serves the restored data
// whatever status says, so durability is re-enabled and the replacement named:
// the worst outcome is a master serving replaced data with AOF off while the
// status reads as if the restore never happened.
func (r *RedisRestoreReconciler) failRestore(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if rs != nil && restore.Status.DatasetReplaced {
		message += "; the master already loaded the restore payload, so the previous dataset is replaced"
		if err := r.reenableAppendonly(ctx, rs); err != nil {
			logger.Error(err, "Failed to re-enable appendonly after the dataset was replaced")
			message += " and appendonly could not be re-enabled: the restored data has no durability until it is"
		}
	}

	if rs != nil && restore.Status.QuiescedDownAfterMilliseconds != 0 {
		adminPassword := r.getAdminPassword(ctx, rs)
		tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
		if err == nil {
			err = r.restoreSentinelDownAfter(ctx, restore, rs, adminPassword, tlsCfg)
		}
		if err != nil {
			logger.Error(err, "Failed to restore sentinel down-after-milliseconds")
			message += "; sentinel down-after-milliseconds is still raised and the sentinel reconciler will converge it back to the spec"
		}
	}

	if err := r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, message, 0); err != nil {
		return ctrl.Result{}, err
	}
	recordEvent(r.Recorder, restore, corev1.EventTypeWarning, "RestoreFailed", message)
	return ctrl.Result{}, nil
}

// restoreExpired bounds the poll loops of the restart and verify phases.
func restoreExpired(restore *redisv1alpha1.RedisRestore) bool {
	return restore.Status.StartTime != nil && time.Since(restore.Status.StartTime.Time) > restoreTimeout
}

// clusterKeyCount returns the master's key total across all databases.
func (r *RedisRestoreReconciler) clusterKeyCount(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (int64, error) {
	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return 0, fmt.Errorf("resolve client TLS config: %w", err)
	}
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return 0, err
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	info, err := redisClient.GetKeyspaceInfo(ctx)
	if err != nil {
		return 0, err
	}
	return sumKeyspaceKeys(info)
}

// sumKeyspaceKeys totals the keys=N fields of an INFO keyspace section, which
// lists every database holding keys.
func sumKeyspaceKeys(info map[string]string) (int64, error) {
	var total int64
	for db, fields := range info {
		if !strings.HasPrefix(db, "db") {
			continue
		}
		for _, field := range strings.Split(fields, ",") {
			value, ok := strings.CutPrefix(field, "keys=")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse keyspace entry %s=%q: %w", db, fields, err)
			}
			total += n
		}
	}
	return total, nil
}

// lookupBackupKeyCount returns the key count recorded by the RedisBackup that
// produced the object being restored, or nil when no backup in the namespace
// matches, for example when the backup was taken elsewhere.
func (r *RedisRestoreReconciler) lookupBackupKeyCount(ctx context.Context, restore *redisv1alpha1.RedisRestore) *int64 {
	backups := &redisv1alpha1.RedisBackupList{}
	if err := r.List(ctx, backups, client.InNamespace(restore.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list RedisBackups for verification")
		return nil
	}
	want := fmt.Sprintf("s3://%s/%s", restore.Spec.BackupSource.S3.Bucket, restore.Spec.BackupSource.BackupPath)
	for i := range backups.Items {
		status := &backups.Items[i].Status
		if status.BackupLocation == want && status.KeyCount != nil {
			return status.KeyCount
		}
	}
	return nil
}

// runningSentinelAddresses lists the dial addresses of the cluster's running
// sentinel pods: the same surface the sentinel reconciler converges, so the
// quiesce and the drift repair talk about the same instances.
func (r *RedisRestoreReconciler) runningSentinelAddresses(ctx context.Context, rs *redisv1alpha1.RedisSentinel) ([]string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": "sentinel",
		"app.kubernetes.io/instance":  rs.Name,
	}); err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		addrs = append(addrs, pod.Status.PodIP+":26379")
	}
	slices.Sort(addrs)
	return addrs, nil
}

// setSentinelDownAfter applies down-after-milliseconds on every running
// sentinel. requireAll refuses to act unless every sentinel in the spec is
// visible and running: one missed by the quiesce keeps the low threshold and
// can still declare the controlled restart a master failure. The lenient form
// serves the unquiesce, where the sentinel reconciler repairs any member
// missed here once the restore is terminal.
func (r *RedisRestoreReconciler) setSentinelDownAfter(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config, value string, requireAll bool) error {
	addrs, err := r.runningSentinelAddresses(ctx, rs)
	if err != nil {
		return err
	}
	if requireAll && int32(len(addrs)) < rs.Spec.SentinelConfig.Replicas {
		return fmt.Errorf("only %d of %d sentinels are running; refusing a partial quiesce",
			len(addrs), rs.Spec.SentinelConfig.Replicas)
	}
	if len(addrs) == 0 {
		return nil
	}
	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(addrs, password, tlsCfg)
	return pool.SetMasterOptionAll(ctx, rs.Name+"-master", "down-after-milliseconds", value)
}

// restoreSentinelDownAfter puts down-after-milliseconds back to the recorded
// pre-restore value (falling back to the spec) and clears the breadcrumb.
func (r *RedisRestoreReconciler) restoreSentinelDownAfter(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config) error {
	value := restore.Status.QuiescedDownAfterMilliseconds
	if value == 0 {
		value = rs.Spec.SentinelConfig.DownAfterMilliseconds
	}
	if value == 0 {
		return nil
	}
	if err := r.setSentinelDownAfter(ctx, rs, password, tlsCfg, strconv.Itoa(int(value)), false); err != nil {
		return err
	}
	restore.Status.QuiescedDownAfterMilliseconds = 0
	return nil
}

func (r *RedisRestoreReconciler) downloadBackup(ctx context.Context, restore *redisv1alpha1.RedisRestore) ([]byte, error) {
	s3Client, err := r.createS3Client(ctx, restore)
	if err != nil {
		return nil, err
	}

	result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(restore.Spec.BackupSource.S3.Bucket),
		Key:    aws.String(restore.Spec.BackupSource.BackupPath),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Body.Close() }()

	return io.ReadAll(result.Body)
}

// preflightBackupObject confirms the backup object exists and returns its
// size, before anything on the cluster is touched.
func (r *RedisRestoreReconciler) preflightBackupObject(ctx context.Context, restore *redisv1alpha1.RedisRestore) (int64, error) {
	s3Client, err := r.createS3Client(ctx, restore)
	if err != nil {
		return 0, err
	}

	head, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(restore.Spec.BackupSource.S3.Bucket),
		Key:    aws.String(restore.Spec.BackupSource.BackupPath),
	})
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(head.ContentLength), nil
}

func (r *RedisRestoreReconciler) createS3Client(ctx context.Context, restore *redisv1alpha1.RedisRestore) (*s3.Client, error) {
	// Re-checked here so no future call site can reach AWS with the operator's
	// identity without passing the destination policy.
	if err := r.validateRestoreDestination(restore); err != nil {
		return nil, err
	}

	var cfg aws.Config
	var err error

	s3Config := restore.Spec.BackupSource.S3

	if s3Config.UseIAMRole {
		cfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(s3Config.Region))
	} else {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      s3Config.CredentialsSecretRef,
			Namespace: restore.Namespace,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get S3 credentials: %w", err)
		}

		accessKey := string(secret.Data["accessKeyId"])
		secretKey := string(secret.Data["secretAccessKey"])

		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(s3Config.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		)
	}

	if err != nil {
		return nil, err
	}

	var s3Client *s3.Client
	if s3Config.Endpoint != "" {
		s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(s3Config.Endpoint)
			o.UsePathStyle = true
		})
	} else {
		s3Client = s3.NewFromConfig(cfg)
	}

	return s3Client, nil
}

// stageRestorePayload writes the RDB next to the master's data under the
// staging name and renames it into place atomically, so the init script only
// ever sees a complete file.
func (r *RedisRestoreReconciler) stageRestorePayload(ctx context.Context, rs *redisv1alpha1.RedisSentinel, rdbData []byte) error {
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return err
	}

	cmd := []string{"sh", "-c",
		"cat > " + restorePayloadPath + ".tmp && mv " + restorePayloadPath + ".tmp " + restorePayloadPath}
	_, stderr, err := r.execInPod(ctx, masterPod, cmd, bytes.NewReader(rdbData))
	if err != nil {
		return fmt.Errorf("stage restore payload on %s: %w (stderr: %s)", masterPod.Name, err, stderr)
	}

	log.FromContext(ctx).Info("Staged restore payload", "pod", masterPod.Name, "size", len(rdbData))
	return nil
}

// restorePayloadPresent reports whether the staged payload still exists in the
// master pod. Its absence is the positive signal that the init script ran and
// the restarted server loaded the dump.
func (r *RedisRestoreReconciler) restorePayloadPresent(ctx context.Context, pod *corev1.Pod) (bool, error) {
	cmd := []string{"sh", "-c",
		"if [ -f " + restorePayloadPath + " ]; then echo present; else echo absent; fi"}
	stdout, stderr, err := r.execInPod(ctx, pod, cmd, nil)
	if err != nil {
		return false, fmt.Errorf("check restore payload on %s: %w (stderr: %s)", pod.Name, err, stderr)
	}
	switch strings.TrimSpace(stdout) {
	case "present":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("unexpected payload check output %q", stdout)
}

// cleanupPreRestoreAOF removes the AOF the init script set aside. Best effort:
// the restore is complete either way and the next restore replaces the dir.
func (r *RedisRestoreReconciler) cleanupPreRestoreAOF(ctx context.Context, pod *corev1.Pod) {
	cmd := []string{"sh", "-c", "rm -rf " + preRestoreAOFPath}
	if _, stderr, err := r.execInPod(ctx, pod, cmd, nil); err != nil {
		log.FromContext(ctx).V(1).Info("Could not remove pre-restore AOF dir", "error", err.Error(), "stderr", stderr)
	}
}

// execInPod runs command in the redis container of pod through the seam.
func (r *RedisRestoreReconciler) execInPod(ctx context.Context, pod *corev1.Pod, command []string, stdin io.Reader) (string, string, error) {
	execFn := r.PodExec
	if execFn == nil {
		execFn = spdyPodExec
	}
	return execFn(ctx, r.RESTConfig, pod, "redis", command, stdin)
}

// spdyPodExec is the production podExecFn.
func spdyPodExec(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdin io.Reader) (string, string, error) {
	if cfg == nil {
		return "", "", fmt.Errorf("RESTConfig is not set")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", "", fmt.Errorf("failed to create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

// getMasterPod resolves status.masterNode, a host:port address reported by
// Sentinel, to the Redis pod whose IP is the host part. A pod restart or
// failover moves the address, so the lookup runs against the live pod list on
// every call rather than caching a name.
func (r *RedisRestoreReconciler) getMasterPod(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (*corev1.Pod, error) {
	if rs.Status.MasterNode == "" {
		return nil, fmt.Errorf("master node not found in status")
	}

	host := rs.Status.MasterNode
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(rs.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/instance":  rs.Name,
			"app.kubernetes.io/component": "redis",
		}); err != nil {
		return nil, err
	}
	for i := range podList.Items {
		if podList.Items[i].Status.PodIP == host {
			return &podList.Items[i], nil
		}
	}

	return nil, fmt.Errorf("no redis pod matches master address %s", rs.Status.MasterNode)
}

func (r *RedisRestoreReconciler) getAdminPassword(ctx context.Context, rs *redisv1alpha1.RedisSentinel) string {
	if rs.Spec.RedisConfig.Auth == nil || rs.Spec.RedisConfig.Auth.SecretName == "" {
		return ""
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rs.Spec.RedisConfig.Auth.SecretName,
		Namespace: rs.Namespace,
	}, secret); err != nil {
		return ""
	}

	return string(secret.Data["password"])
}

// updateStatus writes the phase transition. The error must reach the caller;
// swallowing it lets a finished phase re-run its side effects on the next pass.
func (r *RedisRestoreReconciler) updateStatus(ctx context.Context, restore *redisv1alpha1.RedisRestore, phase redisv1alpha1.RestorePhase, message string, dataSize int64) error {
	restore.Status.Phase = phase
	restore.Status.Message = message
	restore.Status.ObservedGeneration = restore.Generation
	if dataSize > 0 {
		restore.Status.RestoredDataSize = dataSize
	}

	conditionStatus := metav1.ConditionFalse
	reason := string(phase)
	if phase == redisv1alpha1.RestorePhaseCompleted {
		conditionStatus = metav1.ConditionTrue
		reason = "RestoreCompleted"
	}

	// A phase is re-asserted on every requeue while a download or a
	// resynchronisation runs; only a real phase change is a new transition.
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		ObservedGeneration: restore.Generation,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, restore); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisRestore status")
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RedisFactory == nil {
		r.RedisFactory = redisclient.DefaultFactory{}
	}
	if r.RESTConfig == nil {
		r.RESTConfig = mgr.GetConfig()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("redisrestore-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisRestore{}).
		Named("redisrestore").
		Complete(r)
}
