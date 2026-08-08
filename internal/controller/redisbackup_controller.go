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
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/robfig/cron/v3"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/tlsutil"
	custmetrics "github.com/redguard/redguard/pkg/metrics"
)

const redisBackupFinalizer = "redis.redguard.io/redisbackup-finalizer"

// backupRetryInterval paces a retry after a failed run. A backup forks the
// Redis process, so the interval has to stay far away from the rate limiter's
// millisecond backoff even when the cause is permanent.
const backupRetryInterval = 30 * time.Minute

// RedisBackupReconciler reconciles a RedisBackup object
type RedisBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RESTConfig is required by the pod-exec path that streams the RDB out of a
	// Redis pod; without it every backup fails after firing BGSAVE.
	RESTConfig *rest.Config
	Recorder   record.EventRecorder
	// RedisFactory builds Redis clients; nil means DefaultFactory.
	RedisFactory redisclient.Factory
	// AllowedBuckets restricts which S3 buckets a RedisBackup may target with
	// the operator's ambient IAM identity (spec.s3.useIAMRole). Empty disables
	// the IAM-role path entirely; tenant-supplied credentialsSecretRef backups
	// are unaffected.
	AllowedBuckets []string
	// AllowedEndpoints lists custom S3 endpoints permitted together with
	// useIAMRole. Empty permits only the SDK's default AWS endpoint.
	AllowedEndpoints []string
	// PodStream streams a command's stdout out of a Redis pod; nil means the
	// SPDY executor. It is a seam so tests can feed an RDB stream.
	PodStream podStreamFn
	// newS3Client replaces S3 client construction in tests.
	newS3Client func(ctx context.Context, backup *redisv1alpha1.RedisBackup) (s3API, error)
}

// podStreamFn writes a command's stdout to the given writer as it arrives, so
// the caller never holds the full output in memory.
type podStreamFn func(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdout io.Writer) (stderr string, err error)

// s3API is the subset of the AWS S3 client the backup controller uses.
// *s3.Client satisfies it; tests substitute an in-memory implementation.
type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, in *s3.UploadPartInput, optFns ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// s3ClientFor returns the S3 client for a backup, honouring the test override.
func (r *RedisBackupReconciler) s3ClientFor(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (s3API, error) {
	if r.newS3Client != nil {
		return r.newS3Client(ctx, redisBackup)
	}
	return r.createS3Client(ctx, redisBackup)
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *RedisBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the RedisBackup instance
	redisBackup := &redisv1alpha1.RedisBackup{}
	if err := r.Get(ctx, req.NamespacedName, redisBackup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisBackup")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !redisBackup.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, redisBackup)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(redisBackup, redisBackupFinalizer) {
		controllerutil.AddFinalizer(redisBackup, redisBackupFinalizer)
		if err := r.Update(ctx, redisBackup); err != nil {
			return ctrl.Result{}, err
		}
	}

	// A schedule the parser cannot read fires no backup ever. Without this gate
	// the CR sits in Pending forever while status advertises a next run.
	if _, err := parseSchedule(redisBackup.Spec.Schedule); err != nil {
		logger.Error(err, "Rejecting an unparsable backup schedule")
		recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "InvalidSchedule", err.Error())
		r.setDegraded(ctx, redisBackup, "InvalidSchedule", err.Error())
		return ctrl.Result{}, nil
	}

	// Check if suspended
	if redisBackup.Spec.Suspend {
		logger.Info("Backup is suspended")
		r.updateStatus(ctx, redisBackup, "Suspended", "", 0, "", nil)
		return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
	}

	// Refuse disallowed destinations before any Redis or S3 side effect. The
	// check runs at the start of every reconcile, so a violating CR never
	// starts a run, scheduled or one-time; an in-flight run is never aborted
	// because the policy is evaluated before the run begins.
	if err := r.validateBackupDestination(redisBackup); err != nil {
		logger.Error(err, "Backup destination not allowed")
		r.markDestinationRejected(ctx, redisBackup, err)
		return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
	}

	// Check if it's time to backup
	shouldBackup, nextBackupTime := r.shouldBackup(redisBackup)
	if !shouldBackup {
		logger.Info("Not time for backup yet", "nextBackupTime", nextBackupTime)
		return ctrl.Result{RequeueAfter: time.Until(nextBackupTime)}, nil
	}

	// Perform backup
	logger.Info("Starting backup", "cluster", redisBackup.Spec.RedisClusterRef)
	r.updateStatus(ctx, redisBackup, phaseRunning, "", 0, "", nil)

	startTime := time.Now()
	backupLocation, backupSize, keyCount, err := r.performBackup(ctx, redisBackup)
	duration := time.Since(startTime)

	if err != nil {
		// The error stays on the CR instead of being returned: controller-runtime
		// handles a non-nil error first and requeues through the rate limiter,
		// discarding RequeueAfter. A backup that fails on, say, rejected S3
		// credentials would then retry every few milliseconds, forking the Redis
		// process with BGSAVE each time.
		logger.Error(err, "Backup failed")
		r.updateStatus(ctx, redisBackup, phaseFailed, "", 0, "", err)
		recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "BackupFailed", err.Error())
		return ctrl.Result{RequeueAfter: backupRetryInterval}, nil
	}

	// The backup itself succeeded; a retention failure deletes nothing and is
	// surfaced on the CR rather than failing the run.
	if err := r.cleanupOldBackups(ctx, redisBackup); err != nil {
		logger.Error(err, "Failed to cleanup old backups")
		recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "RetentionFailed", err.Error())
	}

	// KeyCount is persisted only here, together with the BackupLocation it
	// describes: written earlier, a failed upload would pair the new count
	// with the previous object and every restore of it would then fail its
	// verification. A nil count clears a stale one for the same reason.
	redisBackup.Status.KeyCount = keyCount
	r.updateStatus(ctx, redisBackup, phaseCompleted, backupLocation, backupSize, duration.String(), nil)

	// Update metrics
	custmetrics.RedisBackupDuration.WithLabelValues(redisBackup.Namespace, redisBackup.Name).Observe(duration.Seconds())
	custmetrics.RedisBackupSize.WithLabelValues(redisBackup.Namespace, redisBackup.Name).Set(float64(backupSize))

	logger.Info("Backup completed successfully", "location", backupLocation, "size", backupSize, "duration", duration)

	// Schedule next backup
	if redisBackup.Spec.Schedule != "" {
		_, nextTime := r.shouldBackup(redisBackup)
		return ctrl.Result{RequeueAfter: time.Until(nextTime)}, nil
	}

	// One-time backup
	return ctrl.Result{}, nil
}

// validateBackupDestination applies the shared destination policy to the
// write path; see validateS3Destination for the confused-deputy rationale.
func (r *RedisBackupReconciler) validateBackupDestination(redisBackup *redisv1alpha1.RedisBackup) error {
	return validateS3Destination(&redisBackup.Spec.S3, r.AllowedBuckets, r.AllowedEndpoints)
}

// markDestinationRejected surfaces a destination-policy refusal in status and
// events under its own reason, distinguishable from a run that failed.
func (r *RedisBackupReconciler) markDestinationRejected(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, cause error) {
	redisBackup.Status.Phase = phaseFailed
	redisBackup.Status.ObservedGeneration = redisBackup.Generation
	r.reportBackupStatus(redisBackup, phaseFailed)
	meta.SetStatusCondition(&redisBackup.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: redisBackup.Generation,
		Reason:             "DestinationNotAllowed",
		Message:            cause.Error(),
	})
	recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "DestinationNotAllowed", cause.Error())
	if err := r.Status().Update(ctx, redisBackup); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisBackup status")
	}
}

// setDegraded reports a terminal, spec-caused failure: only a spec edit can
// clear it, and that edit triggers its own reconcile.
func (r *RedisBackupReconciler) setDegraded(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, reason, message string) {
	redisBackup.Status.Phase = phaseDegraded
	redisBackup.Status.ObservedGeneration = redisBackup.Generation
	redisBackup.Status.NextBackupTime = nil
	r.reportBackupStatus(redisBackup, phaseDegraded)
	for _, cond := range []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse},
		{Type: "Degraded", Status: metav1.ConditionTrue},
	} {
		cond.ObservedGeneration = redisBackup.Generation
		cond.Reason = reason
		cond.Message = message
		meta.SetStatusCondition(&redisBackup.Status.Conditions, cond)
	}
	if err := r.Status().Update(ctx, redisBackup); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisBackup status")
	}
}

// parseSchedule reads the spec's cron expression. An empty schedule is a
// one-time backup and yields no schedule. The parser is deliberately built
// without cron.Descriptor: @daily and friends are not part of the documented
// field, and accepting them here would make the CRD pattern a lie.
func parseSchedule(schedule string) (cron.Schedule, error) {
	if schedule == "" {
		return nil, nil
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, err := parser.Parse(schedule)
	if err != nil {
		return nil, fmt.Errorf("spec.schedule %q is not a valid cron expression: %w", schedule, err)
	}
	return parsed, nil
}

func (r *RedisBackupReconciler) handleDeletion(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (ctrl.Result, error) {
	// Nothing updates these once the CR is gone; a deleted backup that failed
	// its last run would keep alerting forever.
	custmetrics.DeleteBackupSeries(redisBackup.Namespace, redisBackup.Name)

	if controllerutil.ContainsFinalizer(redisBackup, redisBackupFinalizer) {
		// Optionally delete S3 backups here
		controllerutil.RemoveFinalizer(redisBackup, redisBackupFinalizer)
		if err := r.Update(ctx, redisBackup); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *RedisBackupReconciler) shouldBackup(redisBackup *redisv1alpha1.RedisBackup) (bool, time.Time) {
	// If no schedule, it's a one-time backup
	if redisBackup.Spec.Schedule == "" {
		if redisBackup.Status.LastBackupTime == nil {
			return true, time.Now()
		}
		// Already backed up once
		return false, time.Time{}
	}

	// Reconcile refuses an unparsable schedule before reaching this point; the
	// guard stays so a future caller cannot turn a parse error into a backup.
	schedule, err := parseSchedule(redisBackup.Spec.Schedule)
	if err != nil || schedule == nil {
		return false, time.Now().Add(1 * time.Hour)
	}

	now := time.Now()
	var lastBackup time.Time
	if redisBackup.Status.LastBackupTime != nil {
		lastBackup = redisBackup.Status.LastBackupTime.Time
	} else {
		lastBackup = redisBackup.CreationTimestamp.Time
	}

	nextBackup := schedule.Next(lastBackup)
	return now.After(nextBackup), schedule.Next(now)
}

// performBackup runs one backup and returns the stored location, uploaded
// size and the key count taken right after BGSAVE (nil when counting failed).
// The count is returned, not written to status: it describes an object that
// exists only if the upload succeeds, so the caller persists both together.
func (r *RedisBackupReconciler) performBackup(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (string, int64, *int64, error) {
	logger := log.FromContext(ctx)

	// Get RedisSentinel cluster
	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisBackup.Spec.RedisClusterRef,
		Namespace: redisBackup.Namespace,
	}, redisSentinel); err != nil {
		return "", 0, nil, fmt.Errorf("failed to get RedisSentinel: %w", err)
	}

	// Get backup pod - prefer replica over master to reduce load on master
	backupPod, isReplica, err := r.getBackupPod(ctx, redisSentinel)
	if err != nil {
		return "", 0, nil, fmt.Errorf("failed to get backup pod: %w", err)
	}

	if isReplica {
		logger.Info("Taking backup from replica pod", "pod", backupPod.Name)
	} else {
		logger.Info("Taking backup from master pod (no healthy replicas)", "pod", backupPod.Name)
	}

	// Trigger BGSAVE
	if err := r.triggerBGSave(ctx, redisSentinel, backupPod); err != nil {
		return "", 0, nil, fmt.Errorf("failed to trigger BGSAVE: %w", err)
	}

	// Wait for BGSAVE to complete
	if err := r.waitForBGSave(ctx, redisSentinel, backupPod); err != nil {
		return "", 0, nil, fmt.Errorf("BGSAVE failed: %w", err)
	}

	// Recorded so a restore of this object can verify the loaded dataset.
	// Counted after BGSAVE completes, so a cluster taking writes may drift
	// from the snapshot by the keys written since the fork; the restore
	// treats a mismatch as advisory for exactly that reason.
	var keyCount *int64
	if total, err := r.backupKeyCount(ctx, redisSentinel, backupPod); err != nil {
		logger.Error(err, "Failed to count keys; restores of this backup fall back to a non-empty check")
	} else {
		keyCount = &total
	}

	// The RDB is streamed out of the pod, optionally through gzip, straight
	// into the S3 upload: peak memory stays at one upload part regardless of
	// dataset size, where buffering held roughly twice the dataset and OOM
	// killed the operator on any dataset near the pod's memory limit.
	backupLocation, backupSize, err := r.streamBackupToS3(ctx, redisBackup, backupPod)
	if err != nil {
		return "", 0, nil, err
	}

	return backupLocation, backupSize, keyCount, nil
}

// getBackupPod returns a pod to take backup from, preferring replica over master
// Returns: (pod, isReplica, error)
func (r *RedisBackupReconciler) getBackupPod(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) (*corev1.Pod, bool, error) {
	logger := log.FromContext(ctx)

	// Resolved once, reused for the role probe of every pod below.
	adminPassword := r.getAdminPassword(ctx, redisSentinel)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		return nil, false, fmt.Errorf("resolve client TLS config: %w", err)
	}

	// List all Redis pods
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(redisSentinel.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/instance":  redisSentinel.Name,
			"app.kubernetes.io/component": "redis",
		},
	}

	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, false, err
	}

	var masterPod *corev1.Pod
	var healthyReplicas []*corev1.Pod

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Skip non-running pods
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		// Check if pod is ready
		isReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
		if !isReady {
			continue
		}

		// Check role of this pod
		addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
		redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
		isMaster, err := redisClient.IsMaster(ctx)
		redisClient.Close()

		if err != nil {
			logger.Error(err, "Failed to check role", "pod", pod.Name)
			continue
		}

		if isMaster {
			masterPod = pod
		} else {
			// Check if replica is healthy (master_link_status = up)
			redisClient = factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
			replInfo, err := redisClient.GetReplicationInfo(ctx)
			redisClient.Close()

			if err != nil {
				logger.Error(err, "Failed to get replication info", "pod", pod.Name)
				continue
			}

			if replInfo["master_link_status"] == "up" {
				healthyReplicas = append(healthyReplicas, pod)
			}
		}
	}

	// Prefer healthy replica for backup
	if len(healthyReplicas) > 0 {
		return healthyReplicas[0], true, nil
	}

	// Fallback to master
	if masterPod != nil {
		return masterPod, false, nil
	}

	return nil, false, fmt.Errorf("no suitable pod found for backup")
}

func (r *RedisBackupReconciler) getAdminPassword(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) string {
	if redisSentinel.Spec.RedisConfig.Auth == nil || redisSentinel.Spec.RedisConfig.Auth.SecretName == "" {
		return ""
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisSentinel.Spec.RedisConfig.Auth.SecretName,
		Namespace: redisSentinel.Namespace,
	}, secret); err != nil {
		return ""
	}

	return string(secret.Data["password"])
}

// backupKeyCount totals the backup pod's keys across all databases.
func (r *RedisBackupReconciler) backupKeyCount(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) (int64, error) {
	adminPassword := r.getAdminPassword(ctx, redisSentinel)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		return 0, fmt.Errorf("resolve client TLS config: %w", err)
	}

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	info, err := redisClient.GetKeyspaceInfo(ctx)
	if err != nil {
		return 0, err
	}
	return sumKeyspaceKeys(info)
}

func (r *RedisBackupReconciler) triggerBGSave(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	adminPassword := r.getAdminPassword(ctx, redisSentinel)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		return fmt.Errorf("resolve client TLS config: %w", err)
	}

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	// Check if BGSAVE is already in progress
	persistInfo, err := redisClient.GetPersistenceInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get persistence info: %w", err)
	}

	if persistInfo["rdb_bgsave_in_progress"] == "1" {
		logger.Info("BGSAVE already in progress, waiting for it to complete")
		return nil
	}

	// Trigger BGSAVE
	logger.Info("Triggering BGSAVE", "pod", pod.Name)
	if err := redisClient.BGSave(ctx); err != nil {
		// Check if error is because BGSAVE is already running
		if strings.Contains(err.Error(), "already in progress") {
			logger.Info("BGSAVE already in progress")
			return nil
		}
		return fmt.Errorf("failed to trigger BGSAVE: %w", err)
	}

	return nil
}

func (r *RedisBackupReconciler) waitForBGSave(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	adminPassword := r.getAdminPassword(ctx, redisSentinel)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		return fmt.Errorf("resolve client TLS config: %w", err)
	}

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	// Get the last save time before we started
	initialLastSave, err := redisClient.LastSave(ctx)
	if err != nil {
		return fmt.Errorf("failed to get initial last save time: %w", err)
	}

	// Wait for BGSAVE to complete (max 10 minutes)
	timeout := time.After(10 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("BGSAVE timed out after 10 minutes")
		case <-ticker.C:
			persistInfo, err := redisClient.GetPersistenceInfo(ctx)
			if err != nil {
				logger.Error(err, "Failed to get persistence info, retrying")
				continue
			}

			// Check if BGSAVE is still in progress
			if persistInfo["rdb_bgsave_in_progress"] == "1" {
				logger.Info("BGSAVE in progress...", "pod", pod.Name)
				continue
			}

			// Check if BGSAVE was successful
			if persistInfo["rdb_last_bgsave_status"] != "ok" {
				return fmt.Errorf("BGSAVE failed: %s", persistInfo["rdb_last_bgsave_status"])
			}

			// Check if a new save occurred
			lastSave, err := redisClient.LastSave(ctx)
			if err != nil {
				logger.Error(err, "Failed to get last save time, retrying")
				continue
			}

			if lastSave > initialLastSave {
				logger.Info("BGSAVE completed successfully", "pod", pod.Name, "lastSave", lastSave)
				return nil
			}

			// If BGSAVE is not in progress and last save time hasn't changed,
			// it might have already completed before we started checking
			changesSinceLastSave := persistInfo["rdb_changes_since_last_save"]
			if changesSinceLastSave == "0" {
				logger.Info("No changes since last save, backup is current", "pod", pod.Name)
				return nil
			}
		}
	}
}

// spdyPodStream is the production podStreamFn: it execs the command over SPDY
// and copies stdout into the writer as it arrives.
func spdyPodStream(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdout io.Writer) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("RESTConfig is not set")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
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
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: &stderr,
	})
	return stderr.String(), err
}

// backupUploadPartSize is the buffer one in-flight upload part occupies: the
// peak memory of a backup no longer scales with the dataset. 16MiB stays well
// inside the operator's memory limit and puts S3's 10000-part ceiling at
// ~156GiB per backup. A var only so tests can exercise the multipart path
// with small payloads.
var backupUploadPartSize = 16 << 20

// rdbHeaderWriter passes writes through after checking the stream starts with
// the RDB magic. The first five bytes are held back until the check passes,
// so a corrupt dump is refused before a single byte reaches the upload; the
// refusal error aborts the exec copy mid-stream.
type rdbHeaderWriter struct {
	dst      io.Writer
	header   [5]byte
	filled   int
	total    int64
	rejected bool
}

func (w *rdbHeaderWriter) Write(p []byte) (int, error) {
	written := len(p)
	w.total += int64(len(p))
	if w.filled < len(w.header) {
		n := copy(w.header[w.filled:], p)
		w.filled += n
		p = p[n:]
		if w.filled < len(w.header) {
			return written, nil
		}
		if string(w.header[:]) != "REDIS" {
			w.rejected = true
			return 0, fmt.Errorf("invalid RDB file format")
		}
		if _, err := w.dst.Write(w.header[:]); err != nil {
			return 0, err
		}
	}
	if len(p) > 0 {
		if _, err := w.dst.Write(p); err != nil {
			return 0, err
		}
	}
	return written, nil
}

// s3UploadWriter uploads whatever is written to it one part at a time: an
// object that fits one buffer becomes a single PutObject on finish, anything
// larger a multipart upload. The first S3 failure is kept in err so the
// caller can tell an upload failure from an exec one, and abort discards a
// started multipart upload, whose parts are otherwise invisible in listings
// and accrue storage charges forever.
type s3UploadWriter struct {
	ctx      context.Context
	client   s3API
	bucket   string
	key      string
	buf      []byte
	uploadID *string
	parts    []s3types.CompletedPart
	total    int64
	err      error
}

func (w *s3UploadWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		if w.buf == nil {
			w.buf = make([]byte, 0, backupUploadPartSize)
		}
		n := backupUploadPartSize - len(w.buf)
		if n > len(p) {
			n = len(p)
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == backupUploadPartSize {
			if err := w.flushPart(); err != nil {
				return 0, err
			}
		}
	}
	return written, nil
}

func (w *s3UploadWriter) flushPart() error {
	if w.uploadID == nil {
		create, err := w.client.CreateMultipartUpload(w.ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(w.bucket),
			Key:    aws.String(w.key),
		})
		if err != nil {
			w.err = err
			return err
		}
		w.uploadID = create.UploadId
	}
	partNumber := int32(len(w.parts) + 1)
	part, err := w.client.UploadPart(w.ctx, &s3.UploadPartInput{
		Bucket:        aws.String(w.bucket),
		Key:           aws.String(w.key),
		UploadId:      w.uploadID,
		PartNumber:    aws.Int32(partNumber),
		Body:          bytes.NewReader(w.buf),
		ContentLength: aws.Int64(int64(len(w.buf))),
	})
	if err != nil {
		w.err = err
		return err
	}
	w.parts = append(w.parts, s3types.CompletedPart{
		ETag:       part.ETag,
		PartNumber: aws.Int32(partNumber),
	})
	w.total += int64(len(w.buf))
	w.buf = w.buf[:0]
	return nil
}

// finish stores what was written and returns the object's size. The caller
// must abort on error.
func (w *s3UploadWriter) finish() (int64, error) {
	if w.uploadID == nil {
		if _, err := w.client.PutObject(w.ctx, &s3.PutObjectInput{
			Bucket: aws.String(w.bucket),
			Key:    aws.String(w.key),
			Body:   bytes.NewReader(w.buf),
		}); err != nil {
			w.err = err
			return 0, err
		}
		return int64(len(w.buf)), nil
	}
	if len(w.buf) > 0 {
		if err := w.flushPart(); err != nil {
			return 0, err
		}
	}
	if _, err := w.client.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(w.bucket),
		Key:             aws.String(w.key),
		UploadId:        w.uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: w.parts},
	}); err != nil {
		w.err = err
		return 0, err
	}
	return w.total, nil
}

func (w *s3UploadWriter) abort() {
	if w.uploadID == nil {
		return
	}
	_, _ = w.client.AbortMultipartUpload(w.ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(w.bucket),
		Key:      aws.String(w.key),
		UploadId: w.uploadID,
	})
	w.uploadID = nil
}

// streamBackupToS3 execs the RDB out of the pod directly into the upload:
// the exec pushes into header validation, optional gzip and the part-sized
// upload buffer, all inside this call, so no goroutine and no whole-file
// buffer is involved.
func (r *RedisBackupReconciler) streamBackupToS3(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, pod *corev1.Pod) (string, int64, error) {
	s3Client, err := r.s3ClientFor(ctx, redisBackup)
	if err != nil {
		return "", 0, err
	}
	prefix, err := backupObjectPrefix(redisBackup)
	if err != nil {
		return "", 0, err
	}
	key := prefix + fmt.Sprintf("backup-%s.rdb", time.Now().Format("20060102-150405"))
	if redisBackup.Spec.Compression {
		key += ".gz"
	}

	upload := &s3UploadWriter{ctx: ctx, client: s3Client, bucket: redisBackup.Spec.S3.Bucket, key: key}
	sink := io.Writer(upload)
	var gz *gzip.Writer
	if redisBackup.Spec.Compression {
		gz = gzip.NewWriter(upload)
		sink = gz
	}
	validated := &rdbHeaderWriter{dst: sink}

	streamFn := r.PodStream
	if streamFn == nil {
		streamFn = spdyPodStream
	}
	stderr, err := streamFn(ctx, r.RESTConfig, pod, "redis", []string{"cat", "/data/dump.rdb"}, validated)
	if err != nil {
		upload.abort()
		switch {
		case validated.rejected:
			return "", 0, fmt.Errorf("invalid RDB file format")
		case upload.err != nil:
			return "", 0, fmt.Errorf("S3 upload failed: %w", upload.err)
		case strings.TrimSpace(stderr) != "":
			return "", 0, fmt.Errorf("failed to read RDB file: %w (stderr: %s)", err, strings.TrimSpace(stderr))
		}
		return "", 0, fmt.Errorf("failed to read RDB file: %w", err)
	}
	if validated.total == 0 {
		upload.abort()
		return "", 0, fmt.Errorf("RDB file is empty or not found")
	}
	if validated.filled < 5 {
		upload.abort()
		return "", 0, fmt.Errorf("invalid RDB file format")
	}
	if gz != nil {
		// Close writes the gzip footer; without it the stored archive is one
		// no reader, including the restore path, can decompress.
		if err := gz.Close(); err != nil {
			upload.abort()
			if upload.err != nil {
				return "", 0, fmt.Errorf("S3 upload failed: %w", upload.err)
			}
			return "", 0, fmt.Errorf("compression failed: %w", err)
		}
	}
	size, err := upload.finish()
	if err != nil {
		upload.abort()
		return "", 0, fmt.Errorf("S3 upload failed: %w", err)
	}

	return fmt.Sprintf("s3://%s/%s", redisBackup.Spec.S3.Bucket, key), size, nil
}

func (r *RedisBackupReconciler) createS3Client(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (*s3.Client, error) {
	// Re-checked here so no future call site can reach AWS with the operator's
	// identity without passing the destination policy.
	if err := r.validateBackupDestination(redisBackup); err != nil {
		return nil, err
	}

	var cfg aws.Config
	var err error

	// Configure retry with exponential backoff
	// Max 5 attempts with adaptive retry mode for better handling of throttling
	retryer := retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = 5
		o.MaxBackoff = 30 * time.Second
	})

	if redisBackup.Spec.S3.UseIAMRole {
		// Use IAM role (IRSA)
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(redisBackup.Spec.S3.Region),
			config.WithRetryer(func() aws.Retryer { return retryer }),
		)
	} else {
		// Use credentials from secret
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      redisBackup.Spec.S3.CredentialsSecretRef,
			Namespace: redisBackup.Namespace,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get S3 credentials: %w", err)
		}

		accessKey := string(secret.Data["accessKeyId"])
		secretKey := string(secret.Data["secretAccessKey"])

		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(redisBackup.Spec.S3.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
			config.WithRetryer(func() aws.Retryer { return retryer }),
		)
	}

	if err != nil {
		return nil, err
	}

	// Custom endpoint for S3-compatible storage (MinIO, etc.)
	var s3Client *s3.Client
	if redisBackup.Spec.S3.Endpoint != "" {
		s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(redisBackup.Spec.S3.Endpoint)
			o.UsePathStyle = true
		})
	} else {
		s3Client = s3.NewFromConfig(cfg)
	}

	return s3Client, nil
}

// backupObjectPrefix is the exact key prefix this backup reads and writes:
// <spec.prefix>/<namespace>/<clusterName>/<crName>/. The CR name is included
// because two RedisBackups for one cluster (a daily and a weekly) must not
// share a prefix: the one with the shorter retention would prune the other's
// objects. The trailing slash stops "cluster-a" matching "cluster-a-canary".
// All segments are DNS names by API validation; anything else is refused
// rather than joined into an ambiguous prefix.
func backupObjectPrefix(redisBackup *redisv1alpha1.RedisBackup) (string, error) {
	for _, seg := range []string{redisBackup.Namespace, redisBackup.Spec.RedisClusterRef, redisBackup.Name} {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/*") {
			return "", fmt.Errorf("cannot compute an unambiguous S3 prefix from namespace %q, cluster %q and backup %q",
				redisBackup.Namespace, redisBackup.Spec.RedisClusterRef, redisBackup.Name)
		}
	}
	return path.Join(redisBackup.Spec.S3.Prefix, redisBackup.Namespace,
		redisBackup.Spec.RedisClusterRef, redisBackup.Name) + "/", nil
}

// isBackupObject reports whether key is an object this controller writes for
// this cluster: a direct child of prefix named backup-<timestamp>.rdb or
// .rdb.gz. Anything else under the prefix is left alone.
func isBackupObject(prefix, key string) bool {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok || name == "" || strings.Contains(name, "/") {
		return false
	}
	return strings.HasPrefix(name, "backup-") &&
		(strings.HasSuffix(name, ".rdb") || strings.HasSuffix(name, ".rdb.gz"))
}

// cleanupOldBackups prunes this cluster's backups down to the retention count.
// It walks the scoped prefix with a paginator, considers only objects this
// controller writes, and returns every failed delete instead of dropping it.
func (r *RedisBackupReconciler) cleanupOldBackups(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) error {
	if redisBackup.Spec.RetentionPolicy <= 0 {
		return nil
	}

	prefix, err := backupObjectPrefix(redisBackup)
	if err != nil {
		return fmt.Errorf("retention refused: %w", err)
	}

	s3Client, err := r.s3ClientFor(ctx, redisBackup)
	if err != nil {
		return err
	}

	var backups []s3types.Object
	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(redisBackup.Spec.S3.Bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list backups under %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if isBackupObject(prefix, aws.ToString(obj.Key)) {
				backups = append(backups, obj)
			}
		}
	}

	// Oldest first; key order breaks LastModified ties deterministically.
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].LastModified.Equal(*backups[j].LastModified) {
			return aws.ToString(backups[i].Key) < aws.ToString(backups[j].Key)
		}
		return backups[i].LastModified.Before(*backups[j].LastModified)
	})

	var deleteErrs []error
	for i := 0; i < len(backups)-int(redisBackup.Spec.RetentionPolicy); i++ {
		if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(redisBackup.Spec.S3.Bucket),
			Key:    backups[i].Key,
		}); err != nil {
			deleteErrs = append(deleteErrs, fmt.Errorf("delete %s: %w", aws.ToString(backups[i].Key), err))
		}
	}
	return errors.Join(deleteErrs...)
}

// reportBackupStatus mirrors the reported phase onto redis_backup_status, so
// that a failed run is distinguishable from a healthy one. Driven from the
// phase rather than written at each call site: a failure path that forgot the
// gauge would leave the last success standing.
func (r *RedisBackupReconciler) reportBackupStatus(redisBackup *redisv1alpha1.RedisBackup, phase string) {
	var value float64
	switch phase {
	case phaseCompleted:
		value = 1
	case phaseFailed:
		value = 0
	case phaseRunning:
		value = -1
	default:
		// Suspended and any future phase say nothing about the last run, so
		// the gauge keeps reporting it.
		return
	}
	custmetrics.RedisBackupStatus.
		WithLabelValues(redisBackup.Namespace, redisBackup.Name, redisBackup.Spec.RedisClusterRef).
		Set(value)
}

func (r *RedisBackupReconciler) updateStatus(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, phase, location string, size int64, duration string, err error) {
	redisBackup.Status.Phase = phase
	redisBackup.Status.ObservedGeneration = redisBackup.Generation
	r.reportBackupStatus(redisBackup, phase)

	now := metav1.Now()
	switch phase {
	case phaseCompleted:
		redisBackup.Status.LastBackupTime = &now
		redisBackup.Status.BackupLocation = location
		redisBackup.Status.BackupSize = size
		redisBackup.Status.LastBackupDuration = duration
		redisBackup.Status.BackupCount++

		// Calculate next backup time
		if redisBackup.Spec.Schedule != "" {
			_, nextTime := r.shouldBackup(redisBackup)
			nextTimeMeta := metav1.NewTime(nextTime)
			redisBackup.Status.NextBackupTime = &nextTimeMeta
		}

		meta.SetStatusCondition(&redisBackup.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: redisBackup.Generation,
			Reason:             "BackupCompleted",
			Message:            fmt.Sprintf("Backup completed successfully at %s", location),
		})
		meta.SetStatusCondition(&redisBackup.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: redisBackup.Generation,
			Reason:             "BackupCompleted",
			Message:            "The schedule was accepted and the last run succeeded",
		})
	case phaseFailed:
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		meta.SetStatusCondition(&redisBackup.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: redisBackup.Generation,
			Reason:             "BackupFailed",
			Message:            errMsg,
		})
	}

	if err := r.Status().Update(ctx, redisBackup); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisBackup status")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RedisFactory == nil {
		r.RedisFactory = redisclient.DefaultFactory{}
	}
	if r.RESTConfig == nil {
		r.RESTConfig = mgr.GetConfig()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("redisbackup-controller")
	}
	// The controller writes status (Running, Failed) on the object it watches;
	// unfiltered, each write re-enqueues the CR immediately and preempts the
	// RequeueAfter pacing, so a permanently failing backup re-fires BGSAVE in a
	// hot loop. Generation moves only on spec edits and deletion, which are
	// exactly the external triggers a backup must react to promptly; scheduled
	// and retry wakes arrive through RequeueAfter, which no predicate touches.
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisBackup{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("redisbackup").
		Complete(r)
}
