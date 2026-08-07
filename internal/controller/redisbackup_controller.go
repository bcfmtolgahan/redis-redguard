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
	"fmt"
	"path/filepath"
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
	"github.com/robfig/cron/v3"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/redisclient"
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

	// Cleanup old backups based on retention policy
	if err := r.cleanupOldBackups(ctx, redisBackup); err != nil {
		logger.Error(err, "Failed to cleanup old backups")
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

	// Get admin password
	adminPassword := r.getAdminPassword(ctx, redisSentinel)

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
		redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, nil)
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
			redisClient = factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, nil)
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

func (r *RedisBackupReconciler) triggerBGSave(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel, pod *corev1.Pod) error {
	logger := log.FromContext(ctx)
	adminPassword := r.getAdminPassword(ctx, redisSentinel)

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, nil)
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

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, nil)
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
	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)

	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, nil)
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
	s3Client, err := r.createS3Client(ctx, redisBackup)
	if err != nil {
		return "", err
	}

	// Generate backup key
	timestamp := time.Now().Format("20060102-150405")
	key := filepath.Join(
		redisBackup.Spec.S3.Prefix,
		redisBackup.Spec.RedisClusterRef,
		fmt.Sprintf("backup-%s.rdb", timestamp),
	)

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

func (r *RedisBackupReconciler) cleanupOldBackups(ctx context.Context, redisBackup *redisv1alpha1.RedisBackup) error {
	if redisBackup.Spec.RetentionPolicy <= 0 {
		return nil
	}

	s3Client, err := r.createS3Client(ctx, redisBackup)
	if err != nil {
		return err
	}

	prefix := filepath.Join(redisBackup.Spec.S3.Prefix, redisBackup.Spec.RedisClusterRef) + "/"

	// List all backups
	result, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(redisBackup.Spec.S3.Bucket),
		Prefix: aws.String(prefix),
	})

	if err != nil {
		return err
	}

	// Sort by last modified (oldest first)
	objects := result.Contents
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastModified.Before(*objects[j].LastModified)
	})

	// Delete old backups
	toDelete := len(objects) - int(redisBackup.Spec.RetentionPolicy)
	if toDelete > 0 {
		for i := 0; i < toDelete; i++ {
			_, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(redisBackup.Spec.S3.Bucket),
				Key:    objects[i].Key,
			})
			if err != nil {
				log.FromContext(ctx).Error(err, "Failed to delete old backup", "key", *objects[i].Key)
			}
		}
	}

	return nil
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
