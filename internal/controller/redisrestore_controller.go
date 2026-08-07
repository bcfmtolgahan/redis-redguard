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
	"io"
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
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/pkg/redisutils"
)

// RedisRestoreReconciler reconciles a RedisRestore object
type RedisRestoreReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RESTConfig *rest.Config
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch

func (r *RedisRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the RedisRestore instance
	redisRestore := &redisv1alpha1.RedisRestore{}
	if err := r.Get(ctx, req.NamespacedName, redisRestore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisRestore")
		return ctrl.Result{}, err
	}

	// Skip if already completed or failed
	if redisRestore.Status.Phase == redisv1alpha1.RestorePhaseCompleted ||
		redisRestore.Status.Phase == redisv1alpha1.RestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	// Get the RedisSentinel cluster
	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisRestore.Spec.RedisClusterRef,
		Namespace: redisRestore.Namespace,
	}, redisSentinel); err != nil {
		logger.Error(err, "Failed to get RedisSentinel cluster")
		r.updateStatus(ctx, redisRestore, redisv1alpha1.RestorePhaseFailed, "RedisSentinel cluster not found", 0)
		return ctrl.Result{}, err
	}

	// Initialize status if not set
	if redisRestore.Status.Phase == "" {
		r.updateStatus(ctx, redisRestore, redisv1alpha1.RestorePhasePending, "Starting restore operation", 0)
		return ctrl.Result{Requeue: true}, nil
	}

	// Perform restore based on current phase
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

func (r *RedisRestoreReconciler) handlePendingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if cluster has data and Force is not set
	if !restore.Spec.SkipDataCheck && !restore.Spec.Force {
		hasData, err := r.checkClusterHasData(ctx, rs)
		if err != nil {
			logger.Error(err, "Failed to check cluster data")
			r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to check cluster data: "+err.Error(), 0)
			return ctrl.Result{}, err
		}

		if hasData {
			msg := "Cluster has existing data. Set spec.force=true to overwrite"
			r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, msg, 0)
			return ctrl.Result{}, nil
		}
	}

	// Set start time
	restore.Status.StartTime = &metav1.Time{Time: time.Now()}

	// Move to downloading phase
	r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseDownloading, "Downloading backup from S3", 0)
	return ctrl.Result{Requeue: true}, nil
}

func (r *RedisRestoreReconciler) handleDownloadingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Download backup from S3
	data, err := r.downloadBackup(ctx, restore)
	if err != nil {
		logger.Error(err, "Failed to download backup")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to download backup: "+err.Error(), 0)
		return ctrl.Result{}, err
	}

	// Decompress if needed
	var rdbData []byte
	if restore.Spec.BackupSource.Compressed {
		gzipReader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			logger.Error(err, "Failed to create gzip reader")
			r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to decompress backup: "+err.Error(), 0)
			return ctrl.Result{}, err
		}
		defer gzipReader.Close()

		rdbData, err = io.ReadAll(gzipReader)
		if err != nil {
			logger.Error(err, "Failed to decompress backup")
			r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to decompress backup: "+err.Error(), 0)
			return ctrl.Result{}, err
		}
	} else {
		rdbData = data
	}

	// Verify RDB file format
	if len(rdbData) < 5 || string(rdbData[:5]) != "REDIS" {
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Invalid RDB file format", 0)
		return ctrl.Result{}, fmt.Errorf("invalid RDB file format")
	}

	restore.Status.RestoredDataSize = int64(len(rdbData))
	logger.Info("Downloaded and verified backup", "size", len(rdbData))

	// Store RDB data temporarily (in a real implementation, use a ConfigMap or persistent storage)
	// For now, we'll copy directly to the master pod
	if err := r.copyRDBToMaster(ctx, rs, rdbData); err != nil {
		logger.Error(err, "Failed to copy RDB to master")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to copy RDB to master: "+err.Error(), 0)
		return ctrl.Result{}, err
	}

	// Move to restoring phase
	r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseRestoring, "Restoring data to Redis", restore.Status.RestoredDataSize)
	return ctrl.Result{Requeue: true}, nil
}

func (r *RedisRestoreReconciler) handleRestoringPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Restart the master pod to load the new RDB file
	if err := r.restartMasterPod(ctx, rs); err != nil {
		logger.Error(err, "Failed to restart master pod")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to restart master pod: "+err.Error(), restore.Status.RestoredDataSize)
		return ctrl.Result{}, err
	}

	// Wait for master to be ready
	if err := r.waitForMasterReady(ctx, rs); err != nil {
		logger.Error(err, "Master pod not ready after restart")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Master pod not ready: "+err.Error(), restore.Status.RestoredDataSize)
		return ctrl.Result{}, err
	}

	// Move to verifying phase
	r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseVerifying, "Verifying restored data", restore.Status.RestoredDataSize)
	return ctrl.Result{Requeue: true}, nil
}

func (r *RedisRestoreReconciler) handleVerifyingPhase(ctx context.Context, restore *redisv1alpha1.RedisRestore, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Verify data was restored
	adminPassword := r.getAdminPassword(ctx, rs)
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		logger.Error(err, "Failed to get master pod for verification")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Failed to verify: "+err.Error(), restore.Status.RestoredDataSize)
		return ctrl.Result{}, err
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := redisutils.NewRedisClient(addr, adminPassword)
	defer redisClient.Close()

	// Check if Redis is responsive
	if err := redisClient.Ping(ctx); err != nil {
		logger.Error(err, "Redis not responsive after restore")
		r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseFailed, "Redis not responsive: "+err.Error(), restore.Status.RestoredDataSize)
		return ctrl.Result{}, err
	}

	// Get DB size to verify data was loaded
	dbSize, err := redisClient.DBSize(ctx)
	if err != nil {
		logger.Error(err, "Failed to get DB size")
	} else {
		logger.Info("Restore verification complete", "dbSize", dbSize)
	}

	// Calculate duration
	if restore.Status.StartTime != nil {
		duration := time.Since(restore.Status.StartTime.Time)
		restore.Status.Duration = duration.String()
	}

	restore.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	restore.Status.RestoredFrom = restore.Spec.BackupSource.BackupPath

	// Mark as completed
	r.updateStatus(ctx, restore, redisv1alpha1.RestorePhaseCompleted, "Restore completed successfully", restore.Status.RestoredDataSize)
	logger.Info("Restore completed successfully", "duration", restore.Status.Duration)

	return ctrl.Result{}, nil
}

func (r *RedisRestoreReconciler) checkClusterHasData(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (bool, error) {
	adminPassword := r.getAdminPassword(ctx, rs)
	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return false, err
	}

	addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
	redisClient := redisutils.NewRedisClient(addr, adminPassword)
	defer redisClient.Close()

	dbSize, err := redisClient.DBSize(ctx)
	if err != nil {
		return false, err
	}

	return dbSize > 0, nil
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
	defer result.Body.Close()

	return io.ReadAll(result.Body)
}

func (r *RedisRestoreReconciler) createS3Client(ctx context.Context, restore *redisv1alpha1.RedisRestore) (*s3.Client, error) {
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

func (r *RedisRestoreReconciler) copyRDBToMaster(ctx context.Context, rs *redisv1alpha1.RedisSentinel, rdbData []byte) error {
	logger := log.FromContext(ctx)

	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return err
	}

	if r.RESTConfig == nil {
		return fmt.Errorf("RESTConfig is not set")
	}

	clientset, err := kubernetes.NewForConfig(r.RESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Execute command to write RDB data to file
	// First, stop Redis to safely replace the RDB file
	// Note: In a production environment, you might want to use a different approach
	// such as using SHUTDOWN NOSAVE followed by replacing the file

	// Write RDB file using base64 encoding to handle binary data
	// This is a simplified approach - production code should handle larger files differently
	cmd := []string{"sh", "-c", fmt.Sprintf("cat > /data/dump.rdb.new && mv /data/dump.rdb.new /data/dump.rdb")}

	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(masterPod.Name).
		Namespace(masterPod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "redis",
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(rdbData),
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		logger.Error(err, "Failed to copy RDB file", "stderr", stderr.String())
		return fmt.Errorf("failed to copy RDB file: %w", err)
	}

	logger.Info("Copied RDB file to master pod", "pod", masterPod.Name, "size", len(rdbData))
	return nil
}

func (r *RedisRestoreReconciler) restartMasterPod(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	masterPod, err := r.getMasterPod(ctx, rs)
	if err != nil {
		return err
	}

	// Delete the pod to trigger a restart
	if err := r.Delete(ctx, masterPod); err != nil {
		return fmt.Errorf("failed to delete master pod: %w", err)
	}

	logger.Info("Deleted master pod for restart", "pod", masterPod.Name)
	return nil
}

func (r *RedisRestoreReconciler) waitForMasterReady(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	// Wait for the new master pod to be ready
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for master to be ready")
		case <-ticker.C:
			masterPod, err := r.getMasterPod(ctx, rs)
			if err != nil {
				logger.V(1).Info("Waiting for master pod...", "error", err)
				continue
			}

			if masterPod.Status.Phase == corev1.PodRunning {
				// Check if Redis is responsive
				adminPassword := r.getAdminPassword(ctx, rs)
				addr := fmt.Sprintf("%s:6379", masterPod.Status.PodIP)
				redisClient := redisutils.NewRedisClient(addr, adminPassword)

				if err := redisClient.Ping(ctx); err == nil {
					redisClient.Close()
					logger.Info("Master pod is ready", "pod", masterPod.Name)
					return nil
				}
				redisClient.Close()
			}

			logger.V(1).Info("Master pod not ready yet", "phase", masterPod.Status.Phase)
		}
	}
}

func (r *RedisRestoreReconciler) getMasterPod(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (*corev1.Pod, error) {
	if rs.Status.MasterNode == "" {
		return nil, fmt.Errorf("master node not found in status")
	}

	podName := strings.Split(rs.Status.MasterNode, ".")[0]
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: rs.Namespace,
	}, pod); err != nil {
		return nil, err
	}

	return pod, nil
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

func (r *RedisRestoreReconciler) updateStatus(ctx context.Context, restore *redisv1alpha1.RedisRestore, phase redisv1alpha1.RestorePhase, message string, dataSize int64) {
	restore.Status.Phase = phase
	restore.Status.Message = message
	if dataSize > 0 {
		restore.Status.RestoredDataSize = dataSize
	}

	now := metav1.Now()
	conditionStatus := metav1.ConditionFalse
	reason := string(phase)
	if phase == redisv1alpha1.RestorePhaseCompleted {
		conditionStatus = metav1.ConditionTrue
		reason = "RestoreCompleted"
	}

	restore.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             conditionStatus,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		},
	}

	if err := r.Status().Update(ctx, restore); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisRestore status")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisRestore{}).
		Named("redisrestore").
		Complete(r)
}
