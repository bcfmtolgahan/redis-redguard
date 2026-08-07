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
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
	// newS3Client replaces S3 client construction in tests.
	newS3Client func(ctx context.Context, backup *redisv1alpha1.RedisBackup) (s3API, error)
}

// s3API is the subset of the AWS S3 client the backup controller uses.
// *s3.Client satisfies it; tests substitute an in-memory implementation.
type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
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
	if !redisBackup.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, redisBackup)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(redisBackup, redisBackupFinalizer) {
		controllerutil.AddFinalizer(redisBackup, redisBackupFinalizer)
		if err := r.Update(ctx, redisBackup); err != nil {
			return ctrl.Result{}, err
		}
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
	r.updateStatus(ctx, redisBackup, "Running", "", 0, "", nil)

	startTime := time.Now()
	backupLocation, backupSize, err := r.performBackup(ctx, redisBackup)
	duration := time.Since(startTime)

	if err != nil {
		logger.Error(err, "Backup failed")
		r.updateStatus(ctx, redisBackup, "Failed", "", 0, "", err)
		return ctrl.Result{RequeueAfter: 30 * time.Minute}, err
	}

	// The backup itself succeeded; a retention failure deletes nothing and is
	// surfaced on the CR rather than failing the run.
	if err := r.cleanupOldBackups(ctx, redisBackup); err != nil {
		logger.Error(err, "Failed to cleanup old backups")
		recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "RetentionFailed", err.Error())
	}

	// Update status
	r.updateStatus(ctx, redisBackup, "Completed", backupLocation, backupSize, duration.String(), nil)

	// Update metrics
	custmetrics.RedisBackupStatus.WithLabelValues(redisBackup.Namespace, redisBackup.Name, redisBackup.Spec.RedisClusterRef).Set(1)
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

// validateBackupDestination enforces the operator-level destination policy on
// the confused-deputy path: with useIAMRole the request runs under the
// operator's own AWS identity, so any namespace user could otherwise aim that
// identity at an arbitrary bucket or endpoint. Tenant-supplied credentials
// (credentialsSecretRef) carry only privileges the tenant already holds and
// are not restricted.
func (r *RedisBackupReconciler) validateBackupDestination(redisBackup *redisv1alpha1.RedisBackup) error {
	if !redisBackup.Spec.S3.UseIAMRole {
		return nil
	}
	if len(r.AllowedBuckets) == 0 {
		return fmt.Errorf("spec.s3.useIAMRole is set but the operator runs without --allowed-backup-buckets; the IAM-role path is disabled, use spec.s3.credentialsSecretRef instead")
	}
	if !slices.Contains(r.AllowedBuckets, redisBackup.Spec.S3.Bucket) {
		return fmt.Errorf("bucket %q is not in the operator's --allowed-backup-buckets", redisBackup.Spec.S3.Bucket)
	}
	if ep := redisBackup.Spec.S3.Endpoint; ep != "" && !slices.Contains(r.AllowedEndpoints, ep) {
		return fmt.Errorf("endpoint %q is not in the operator's --allowed-backup-endpoints; a custom endpoint would redirect requests signed with the operator's identity", ep)
	}
	return nil
}

// markDestinationRejected surfaces a destination-policy refusal in status and
// events under its own reason, distinguishable from a run that failed.
func (r *RedisBackupReconciler) markDestinationRejected(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, cause error) {
	redisBackup.Status.Phase = "Failed"
	redisBackup.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "DestinationNotAllowed",
			Message:            cause.Error(),
		},
	}
	recordEvent(r.Recorder, redisBackup, corev1.EventTypeWarning, "DestinationNotAllowed", cause.Error())
	if err := r.Status().Update(ctx, redisBackup); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisBackup status")
	}
}

func (r *RedisBackupReconciler) handleDeletion(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (ctrl.Result, error) {
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

	// Parse cron schedule
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(redisBackup.Spec.Schedule)
	if err != nil {
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

func (r *RedisBackupReconciler) performBackup(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) (string, int64, error) {
	logger := log.FromContext(ctx)

	// Get RedisSentinel cluster
	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisBackup.Spec.RedisClusterRef,
		Namespace: redisBackup.Namespace,
	}, redisSentinel); err != nil {
		return "", 0, fmt.Errorf("failed to get RedisSentinel: %w", err)
	}

	// Get backup pod - prefer replica over master to reduce load on master
	backupPod, isReplica, err := r.getBackupPod(ctx, redisSentinel)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get backup pod: %w", err)
	}

	if isReplica {
		logger.Info("Taking backup from replica pod", "pod", backupPod.Name)
	} else {
		logger.Info("Taking backup from master pod (no healthy replicas)", "pod", backupPod.Name)
	}

	// Trigger BGSAVE
	if err := r.triggerBGSave(ctx, redisSentinel, backupPod); err != nil {
		return "", 0, fmt.Errorf("failed to trigger BGSAVE: %w", err)
	}

	// Wait for BGSAVE to complete
	if err := r.waitForBGSave(ctx, redisSentinel, backupPod); err != nil {
		return "", 0, fmt.Errorf("BGSAVE failed: %w", err)
	}

	// Recorded so a restore of this object can verify the loaded dataset.
	// Counted after BGSAVE completes, so a cluster taking writes may drift
	// from the snapshot by the keys written since the fork.
	if total, err := r.backupKeyCount(ctx, redisSentinel, backupPod); err != nil {
		logger.Error(err, "Failed to count keys; restores of this backup fall back to a non-empty check")
	} else {
		redisBackup.Status.KeyCount = &total
	}

	// Get RDB file content using pod exec
	rdbData, err := r.getRDBData(ctx, redisSentinel, backupPod)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get RDB data: %w", err)
	}

	// Compress if enabled
	var dataToUpload []byte
	if redisBackup.Spec.Compression {
		var buf bytes.Buffer
		gzipWriter := gzip.NewWriter(&buf)
		if _, err := gzipWriter.Write(rdbData); err != nil {
			return "", 0, fmt.Errorf("compression failed: %w", err)
		}
		gzipWriter.Close()
		dataToUpload = buf.Bytes()
		logger.Info("Compressed backup", "original", len(rdbData), "compressed", len(dataToUpload))
	} else {
		dataToUpload = rdbData
	}

	// Upload to S3
	backupLocation, err := r.uploadToS3(ctx, redisBackup, dataToUpload)
	if err != nil {
		return "", 0, fmt.Errorf("S3 upload failed: %w", err)
	}

	return backupLocation, int64(len(dataToUpload)), nil
}

func (r *RedisBackupReconciler) getMasterPod(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) (*corev1.Pod, error) {
	if redisSentinel.Status.MasterNode == "" {
		return nil, fmt.Errorf("master node not found")
	}

	podName := strings.Split(redisSentinel.Status.MasterNode, ".")[0]
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: redisSentinel.Namespace,
	}, pod); err != nil {
		return nil, err
	}

	return pod, nil
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

func (r *RedisBackupReconciler) getRDBData(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) ([]byte, error) {
	logger := log.FromContext(ctx)

	if r.RESTConfig == nil {
		return nil, fmt.Errorf("RESTConfig is not set")
	}

	clientset, err := kubernetes.NewForConfig(r.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	// Execute cat /data/dump.rdb in the pod
	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "redis",
			Command:   []string{"cat", "/data/dump.rdb"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RESTConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		// Check if stderr has useful info
		if stderr.Len() > 0 {
			logger.Error(err, "Pod exec failed", "stderr", stderr.String())
		}
		return nil, fmt.Errorf("failed to read RDB file: %w", err)
	}

	if stdout.Len() == 0 {
		return nil, fmt.Errorf("RDB file is empty or not found")
	}

	// Verify it's a valid RDB file (starts with "REDIS")
	data := stdout.Bytes()
	if len(data) < 5 || string(data[:5]) != "REDIS" {
		return nil, fmt.Errorf("invalid RDB file format")
	}

	logger.Info("Retrieved RDB data", "size", len(data), "pod", pod.Name)
	return data, nil
}

// getBackupRDBPath returns the path to the RDB file based on persistence info
func (r *RedisBackupReconciler) getBackupRDBPath(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) (string, error) {
	adminPassword := r.getAdminPassword(ctx, redisSentinel)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		return "", fmt.Errorf("resolve client TLS config: %w", err)
	}
	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)

	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	// Get dir and dbfilename from config
	dirConfig, err := redisClient.ConfigGet(ctx, "dir")
	if err != nil {
		return "/data/dump.rdb", nil // default path
	}

	dbfileConfig, err := redisClient.ConfigGet(ctx, "dbfilename")
	if err != nil {
		return "/data/dump.rdb", nil // default path
	}

	dir := dirConfig["dir"]
	dbfile := dbfileConfig["dbfilename"]

	if dir == "" {
		dir = "/data"
	}
	if dbfile == "" {
		dbfile = "dump.rdb"
	}

	return filepath.Join(dir, dbfile), nil
}

func (r *RedisBackupReconciler) uploadToS3(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, data []byte) (string, error) {
	// Create S3 client
	s3Client, err := r.s3ClientFor(ctx, redisBackup)
	if err != nil {
		return "", err
	}

	prefix, err := backupObjectPrefix(redisBackup)
	if err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	key := prefix + fmt.Sprintf("backup-%s.rdb", timestamp)
	if redisBackup.Spec.Compression {
		key += ".gz"
	}

	// Upload to S3
	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(redisBackup.Spec.S3.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})

	if err != nil {
		return "", err
	}

	location := fmt.Sprintf("s3://%s/%s", redisBackup.Spec.S3.Bucket, key)
	return location, nil
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
// <spec.prefix>/<namespace>/<clusterName>/. Namespace and cluster name are
// always included so two clusters can never share a prefix, and the trailing
// slash stops "cluster-a" matching "cluster-a-canary". Both segments are DNS
// labels by API validation; anything else is refused rather than joined into
// an ambiguous prefix.
func backupObjectPrefix(redisBackup *redisv1alpha1.RedisBackup) (string, error) {
	for _, seg := range []string{redisBackup.Namespace, redisBackup.Spec.RedisClusterRef} {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/*") {
			return "", fmt.Errorf("cannot compute an unambiguous S3 prefix from namespace %q and cluster %q",
				redisBackup.Namespace, redisBackup.Spec.RedisClusterRef)
		}
	}
	return path.Join(redisBackup.Spec.S3.Prefix, redisBackup.Namespace, redisBackup.Spec.RedisClusterRef) + "/", nil
}

// isBackupObject reports whether key is an object this controller writes for
// this cluster: a direct child of prefix named backup-<timestamp>.rdb or
// .rdb.gz. Anything else under the prefix is left alone.
func isBackupObject(prefix, key string) bool {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return false
	}
	return strings.HasPrefix(rest, "backup-") &&
		(strings.HasSuffix(rest, ".rdb") || strings.HasSuffix(rest, ".rdb.gz"))
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

func (r *RedisBackupReconciler) updateStatus(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup, phase, location string, size int64, duration string, err error) {
	redisBackup.Status.Phase = phase

	now := metav1.Now()
	if phase == "Completed" {
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

		redisBackup.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "BackupCompleted",
				Message:            fmt.Sprintf("Backup completed successfully at %s", location),
			},
		}
	} else if phase == "Failed" {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		redisBackup.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             "BackupFailed",
				Message:            errMsg,
			},
		}
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisBackup{}).
		Named("redisbackup").
		Complete(r)
}
