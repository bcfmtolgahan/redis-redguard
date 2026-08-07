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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/tlsutil"
	custmetrics "github.com/redguard/redguard/pkg/metrics"
)

const redisUserFinalizer = "redis.redguard.io/redisuser-finalizer"

// RedisUserReconciler reconciles a RedisUser object
type RedisUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RedisFactory builds Redis clients; nil means DefaultFactory.
	RedisFactory redisclient.Factory
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *RedisUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the RedisUser instance
	redisUser := &redisv1alpha1.RedisUser{}
	if err := r.Get(ctx, req.NamespacedName, redisUser); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisUser")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !redisUser.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, redisUser)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(redisUser, redisUserFinalizer) {
		controllerutil.AddFinalizer(redisUser, redisUserFinalizer)
		if err := r.Update(ctx, redisUser); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get the RedisSentinel cluster
	redisSentinel := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisUser.Spec.RedisClusterRef,
		Namespace: redisUser.Namespace,
	}, redisSentinel); err != nil {
		logger.Error(err, "Failed to get RedisSentinel cluster")
		r.updateStatus(ctx, redisUser, "Error", nil, "RedisSentinel cluster not found")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Get password from secret
	password, err := r.getPasswordFromSecret(ctx, redisUser)
	if err != nil {
		logger.Error(err, "Failed to get password from secret")
		r.updateStatus(ctx, redisUser, "Error", nil, "Password secret not found")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Get all Redis pod addresses (ACL must be applied to all nodes, not just master)
	allAddresses, err := r.getAllRedisPodAddresses(ctx, redisSentinel)
	if err != nil {
		logger.Error(err, "Failed to get Redis pod addresses")
		r.updateStatus(ctx, redisUser, "Error", nil, "Redis pods not found")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	if len(allAddresses) == 0 {
		logger.Error(nil, "No running Redis pods found")
		r.updateStatus(ctx, redisUser, "Error", nil, "No running Redis pods")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, fmt.Errorf("no running Redis pods")
	}

	// Get Redis admin password for connection
	adminPassword := ""
	if redisSentinel.Spec.RedisConfig.Auth != nil && redisSentinel.Spec.RedisConfig.Auth.SecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      redisSentinel.Spec.RedisConfig.Auth.SecretName,
			Namespace: redisSentinel.Namespace,
		}, secret); err == nil {
			adminPassword = string(secret.Data["password"])
		}
	}

	// Resolved once, reused for every pod below.
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
	if err != nil {
		logger.Error(err, "Failed to resolve client TLS config")
		r.updateStatus(ctx, redisUser, "Error", nil, err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Apply ACL to all Redis pods (master and replicas)
	// This ensures ACL rules persist after failover
	appliedTo := []string{}
	var lastErr error
	for _, addr := range allAddresses {
		redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
		if err := r.applyACL(ctx, redisClient, redisUser, password); err != nil {
			logger.Error(err, "Failed to apply ACL to pod", "address", addr)
			lastErr = err
		} else {
			appliedTo = append(appliedTo, addr)
			logger.Info("Applied ACL to pod", "address", addr, "username", redisUser.Spec.Username)
		}
		redisClient.Close()
	}

	// If we failed to apply to any pod, report error but continue
	if len(appliedTo) == 0 {
		logger.Error(lastErr, "Failed to apply ACL to any pod")
		r.updateStatus(ctx, redisUser, "Error", nil, lastErr.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, lastErr
	}

	// Partial success - some pods may have failed
	if lastErr != nil {
		logger.Info("ACL applied to some pods, will retry failed ones", "applied", len(appliedTo), "total", len(allAddresses))
	}

	// Update status
	r.updateStatus(ctx, redisUser, "Ready", appliedTo, "")

	// Update metrics
	custmetrics.RedisUserACLStatus.WithLabelValues(redisUser.Namespace, redisUser.Name, redisUser.Spec.Username).Set(1)

	logger.Info("Successfully reconciled RedisUser", "username", redisUser.Spec.Username)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *RedisUserReconciler) handleDeletion(ctx context.Context, redisUser *redisv1alpha1.RedisUser) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(redisUser, redisUserFinalizer) {
		// Delete ACL user from all Redis pods
		cleanupSuccessful := true
		cleanupAttempted := false

		redisSentinel := &redisv1alpha1.RedisSentinel{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      redisUser.Spec.RedisClusterRef,
			Namespace: redisUser.Namespace,
		}, redisSentinel); err == nil {
			allAddresses, err := r.getAllRedisPodAddresses(ctx, redisSentinel)
			if err == nil && len(allAddresses) > 0 {
				cleanupAttempted = true
				adminPassword := ""
				if redisSentinel.Spec.RedisConfig.Auth != nil && redisSentinel.Spec.RedisConfig.Auth.SecretName != "" {
					secret := &corev1.Secret{}
					if err := r.Get(ctx, types.NamespacedName{
						Name:      redisSentinel.Spec.RedisConfig.Auth.SecretName,
						Namespace: redisSentinel.Namespace,
					}, secret); err == nil {
						adminPassword = string(secret.Data["password"])
					}
				}

				// Without the TLS settings no pod is reachable, so retry the
				// cleanup later rather than dial plaintext.
				tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, redisSentinel)
				if err != nil {
					logger.Error(err, "Cannot resolve client TLS config for ACL cleanup, will retry")
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}

				// Delete from all pods with proper cleanup
				failedPods := []string{}
				for _, addr := range allAddresses {
					func(address string) {
						redisClient := factoryOrDefault(r.RedisFactory).NewClient(address, adminPassword, tlsCfg)
						defer redisClient.Close()

						if err := redisClient.ACLDelUser(ctx, redisUser.Spec.Username); err != nil {
							// Ignore "user not found" errors - user might already be deleted
							if !strings.Contains(err.Error(), "ERR The user") {
								logger.Error(err, "Failed to delete ACL user from pod", "username", redisUser.Spec.Username, "address", address)
								failedPods = append(failedPods, address)
								cleanupSuccessful = false
							}
						} else {
							logger.Info("Deleted ACL user from pod", "username", redisUser.Spec.Username, "address", address)
						}
					}(addr)
				}

				// If cleanup failed on some pods, requeue to retry
				if !cleanupSuccessful && len(failedPods) > 0 {
					logger.Info("ACL cleanup incomplete, will retry", "failedPods", failedPods)
					// Requeue after 10 seconds to retry cleanup
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
			}
		} else if !apierrors.IsNotFound(err) {
			// If we couldn't get the RedisSentinel for a reason other than NotFound, retry
			logger.Error(err, "Failed to get RedisSentinel for cleanup")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		// Only remove finalizer if cleanup was successful or cluster doesn't exist
		// This ensures we don't leave orphaned ACL users
		if cleanupSuccessful || !cleanupAttempted {
			controllerutil.RemoveFinalizer(redisUser, redisUserFinalizer)
			if err := r.Update(ctx, redisUser); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("Finalizer removed, RedisUser cleanup complete", "username", redisUser.Spec.Username)
		}
	}

	return ctrl.Result{}, nil
}

func (r *RedisUserReconciler) applyACL(ctx context.Context, redisClient redisclient.Client, redisUser *redisv1alpha1.RedisUser, password string) error {
	rules := []string{"reset"} // Start fresh

	// Set password
	rules = append(rules, fmt.Sprintf(">%s", password))

	// Enable/disable user
	if redisUser.Spec.Enabled {
		rules = append(rules, "on")
	} else {
		rules = append(rules, "off")
	}

	// Add categories
	for _, cat := range redisUser.Spec.ACLRules.Categories {
		rules = append(rules, cat)
	}

	// Add commands
	for _, cmd := range redisUser.Spec.ACLRules.Commands {
		rules = append(rules, cmd)
	}

	// Add key patterns
	for _, key := range redisUser.Spec.ACLRules.Keys {
		rules = append(rules, key)
	}

	// Add channel patterns
	for _, ch := range redisUser.Spec.ACLRules.Channels {
		rules = append(rules, ch)
	}

	// If no rules specified, allow read-only access
	if len(redisUser.Spec.ACLRules.Categories) == 0 &&
		len(redisUser.Spec.ACLRules.Commands) == 0 {
		rules = append(rules, "+@read")
	}

	// If no keys specified, allow all keys
	if len(redisUser.Spec.ACLRules.Keys) == 0 {
		rules = append(rules, "~*")
	}

	return redisClient.ACLSetUser(ctx, redisUser.Spec.Username, rules...)
}

func (r *RedisUserReconciler) getPasswordFromSecret(ctx context.Context, redisUser *redisv1alpha1.RedisUser) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      redisUser.Spec.PasswordSecretRef,
		Namespace: redisUser.Namespace,
	}, secret); err != nil {
		return "", err
	}

	password, ok := secret.Data["password"]
	if !ok {
		return "", fmt.Errorf("password key not found in secret")
	}

	return string(password), nil
}

func (r *RedisUserReconciler) getMasterPodAddress(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) (string, error) {
	// Use the master node from status
	if redisSentinel.Status.MasterNode == "" {
		return "", fmt.Errorf("master node not found in RedisSentinel status")
	}

	// Extract pod name from master node (format: podname.headless-svc)
	podName := strings.Split(redisSentinel.Status.MasterNode, ".")[0]

	// Get the pod to find its IP
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: redisSentinel.Namespace,
	}, pod); err != nil {
		return "", err
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod IP not available")
	}

	// Use default Redis port
	port := 6379

	return fmt.Sprintf("%s:%d", pod.Status.PodIP, port), nil
}

// getAllRedisPodAddresses returns all running Redis pod addresses
// ACL rules must be applied to all nodes to ensure they persist after failover
func (r *RedisUserReconciler) getAllRedisPodAddresses(ctx context.Context, redisSentinel *redisv1alpha1.RedisSentinel) ([]string, error) {
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
		return nil, err
	}

	addresses := []string{}
	for _, pod := range podList.Items {
		// Only include running pods with an IP
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			// Check if the pod is ready
			isReady := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					isReady = true
					break
				}
			}

			if isReady {
				addresses = append(addresses, fmt.Sprintf("%s:6379", pod.Status.PodIP))
			}
		}
	}

	return addresses, nil
}

func (r *RedisUserReconciler) updateStatus(ctx context.Context, redisUser *redisv1alpha1.RedisUser, phase string, appliedTo []string, errorMsg string) {
	redisUser.Status.Phase = phase
	if appliedTo != nil {
		redisUser.Status.AppliedTo = appliedTo
	}

	now := metav1.Now()
	if phase == "Ready" {
		redisUser.Status.LastPasswordChange = &now
		redisUser.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "ACLApplied",
				Message:            "ACL successfully applied to Redis cluster",
			},
		}
	} else {
		redisUser.Status.Conditions = []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             "Error",
				Message:            errorMsg,
			},
		}
	}

	if err := r.Status().Update(ctx, redisUser); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisUser status")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RedisFactory == nil {
		r.RedisFactory = redisclient.DefaultFactory{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisUser{}).
		Named("redisuser").
		Complete(r)
}
