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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	"github.com/redguard/redguard/internal/sentinel"
	custmetrics "github.com/redguard/redguard/pkg/metrics"
	"github.com/redguard/redguard/pkg/redisutils"
)

// RedisSentinelReconciler reconciles a RedisSentinel object
type RedisSentinelReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

const (
	finalizerName = "redis.redguard.io/finalizer"
)

// Reconcile reconciles a RedisSentinel object
func (r *RedisSentinelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling RedisSentinel", "name", req.Name, "namespace", req.Namespace)

	// Fetch the RedisSentinel instance
	rs := &redisv1alpha1.RedisSentinel{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("RedisSentinel resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RedisSentinel")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !rs.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, rs)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(rs, finalizerName) {
		controllerutil.AddFinalizer(rs, finalizerName)
		if err := r.Update(ctx, rs); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile resources
	if err := r.reconcileConfigMaps(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile ConfigMaps")
		return ctrl.Result{}, err
	}

	if err := r.reconcileServices(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile Services")
		r.Recorder.Event(rs, corev1.EventTypeWarning, "ServiceReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcileNetworkPolicies(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile NetworkPolicies")
		r.Recorder.Event(rs, corev1.EventTypeWarning, "NetworkPolicyReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatefulSets(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile StatefulSets")
		r.Recorder.Event(rs, corev1.EventTypeWarning, "StatefulSetReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, rs); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Update Prometheus metrics
	r.updateMetrics(ctx, rs)

	// Requeue after 30 seconds to check for failovers
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *RedisSentinelReconciler) handleDeletion(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(rs, finalizerName) {
		logger.Info("Performing cleanup before deletion")

		// Cleanup logic here if needed (e.g., external resources)

		controllerutil.RemoveFinalizer(rs, finalizerName)
		if err := r.Update(ctx, rs); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *RedisSentinelReconciler) reconcileConfigMaps(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	// Redis ConfigMap
	redisConfigMap := builder.BuildRedisConfigMap(rs)
	if err := controllerutil.SetControllerReference(rs, redisConfigMap, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, redisConfigMap); err != nil {
		return fmt.Errorf("failed to reconcile Redis ConfigMap: %w", err)
	}
	logger.Info("Reconciled Redis ConfigMap", "name", redisConfigMap.Name)

	// Sentinel ConfigMap
	sentinelConfigMap := builder.BuildSentinelConfigMap(rs)
	if err := controllerutil.SetControllerReference(rs, sentinelConfigMap, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, sentinelConfigMap); err != nil {
		return fmt.Errorf("failed to reconcile Sentinel ConfigMap: %w", err)
	}
	logger.Info("Reconciled Sentinel ConfigMap", "name", sentinelConfigMap.Name)

	return nil
}

func (r *RedisSentinelReconciler) reconcileServices(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	services := []*corev1.Service{
		builder.BuildRedisHeadlessService(rs),
		builder.BuildRedisService(rs),
		builder.BuildSentinelHeadlessService(rs),
		builder.BuildSentinelService(rs),
	}

	for _, svc := range services {
		if err := controllerutil.SetControllerReference(rs, svc, r.Scheme); err != nil {
			return err
		}
		if err := r.createOrUpdate(ctx, svc); err != nil {
			return fmt.Errorf("failed to reconcile Service %s: %w", svc.Name, err)
		}
		logger.Info("Reconciled Service", "name", svc.Name)
	}

	return nil
}

func (r *RedisSentinelReconciler) reconcileStatefulSets(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	// Redis StatefulSet
	redisStatefulSet := builder.BuildRedisStatefulSet(rs)
	if err := controllerutil.SetControllerReference(rs, redisStatefulSet, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, redisStatefulSet); err != nil {
		return fmt.Errorf("failed to reconcile Redis StatefulSet: %w", err)
	}
	logger.Info("Reconciled Redis StatefulSet", "name", redisStatefulSet.Name)

	// Sentinel StatefulSet
	sentinelStatefulSet := builder.BuildSentinelStatefulSet(rs)
	if err := controllerutil.SetControllerReference(rs, sentinelStatefulSet, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, sentinelStatefulSet); err != nil {
		return fmt.Errorf("failed to reconcile Sentinel StatefulSet: %w", err)
	}
	logger.Info("Reconciled Sentinel StatefulSet", "name", sentinelStatefulSet.Name)

	return nil
}

func (r *RedisSentinelReconciler) reconcileNetworkPolicies(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	// Redis NetworkPolicy
	redisNetPol := builder.BuildRedisNetworkPolicy(rs)
	if err := controllerutil.SetControllerReference(rs, redisNetPol, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, redisNetPol); err != nil {
		return fmt.Errorf("failed to reconcile Redis NetworkPolicy: %w", err)
	}
	logger.Info("Reconciled Redis NetworkPolicy", "name", redisNetPol.Name)

	// Sentinel NetworkPolicy
	sentinelNetPol := builder.BuildSentinelNetworkPolicy(rs)
	if err := controllerutil.SetControllerReference(rs, sentinelNetPol, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdate(ctx, sentinelNetPol); err != nil {
		return fmt.Errorf("failed to reconcile Sentinel NetworkPolicy: %w", err)
	}
	logger.Info("Reconciled Sentinel NetworkPolicy", "name", sentinelNetPol.Name)

	r.Recorder.Event(rs, corev1.EventTypeNormal, "NetworkPoliciesReconciled", "Network policies successfully created/updated")

	return nil
}

func (r *RedisSentinelReconciler) updateStatus(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	// Get Redis StatefulSet
	redisStatefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rs.Name + "-redis",
		Namespace: rs.Namespace,
	}, redisStatefulSet); err != nil {
		if errors.IsNotFound(err) {
			return nil // Not created yet
		}
		return err
	}

	// Get Sentinel StatefulSet
	sentinelStatefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rs.Name + "-sentinel",
		Namespace: rs.Namespace,
	}, sentinelStatefulSet); err != nil {
		if errors.IsNotFound(err) {
			return nil // Not created yet
		}
		return err
	}

	// Update status
	rs.Status.ReadyReplicas = redisStatefulSet.Status.ReadyReplicas
	rs.Status.ReadySentinels = sentinelStatefulSet.Status.ReadyReplicas

	// Determine phase
	if redisStatefulSet.Status.ReadyReplicas == 0 {
		rs.Status.Phase = "Creating"
	} else if redisStatefulSet.Status.ReadyReplicas < rs.Spec.RedisConfig.Replicas {
		rs.Status.Phase = "Scaling"
	} else if sentinelStatefulSet.Status.ReadyReplicas < rs.Spec.SentinelConfig.Replicas {
		rs.Status.Phase = "ConfiguringSentinel"
	} else {
		rs.Status.Phase = "Running"
	}

	// Try to get current master from Sentinel
	if sentinelStatefulSet.Status.ReadyReplicas > 0 {
		masterNode, err := r.getCurrentMaster(ctx, rs)
		if err != nil {
			logger.Info("Could not determine master node", "error", err)
		} else {
			if rs.Status.MasterNode != "" && rs.Status.MasterNode != masterNode {
				// Failover detected
				logger.Info("Failover detected", "old", rs.Status.MasterNode, "new", masterNode)
				r.Recorder.Event(rs, corev1.EventTypeWarning, "FailoverDetected",
					fmt.Sprintf("Master changed from %s to %s", rs.Status.MasterNode, masterNode))
				now := metav1.Now()
				rs.Status.LastFailoverTime = &now
				custmetrics.RedisFailoverTotal.WithLabelValues(rs.Namespace, rs.Name).Inc()

				// Handle failover - reconfigure old master and verify replicas
				go r.handleFailover(ctx, rs, rs.Status.MasterNode, masterNode)
			}
			rs.Status.MasterNode = masterNode
		}
	}

	// Build detailed conditions
	now := metav1.Now()
	conditions := []metav1.Condition{}

	// Available condition
	availableCondition := metav1.Condition{
		Type:               "Available",
		ObservedGeneration: rs.Generation,
		LastTransitionTime: now,
	}
	if rs.Status.Phase == "Running" {
		availableCondition.Status = metav1.ConditionTrue
		availableCondition.Reason = "ReconcileSuccess"
		availableCondition.Message = "RedisSentinel is available"
	} else {
		availableCondition.Status = metav1.ConditionFalse
		availableCondition.Reason = "NotReady"
		availableCondition.Message = fmt.Sprintf("RedisSentinel is in %s phase", rs.Status.Phase)
	}
	conditions = append(conditions, availableCondition)

	// Replication condition
	replicationCondition := metav1.Condition{
		Type:               "ReplicationHealthy",
		ObservedGeneration: rs.Generation,
		LastTransitionTime: now,
	}
	if redisStatefulSet.Status.ReadyReplicas >= rs.Spec.RedisConfig.Replicas {
		replicationCondition.Status = metav1.ConditionTrue
		replicationCondition.Reason = "AllReplicasReady"
		replicationCondition.Message = fmt.Sprintf("All %d replicas are ready", redisStatefulSet.Status.ReadyReplicas)
	} else {
		replicationCondition.Status = metav1.ConditionFalse
		replicationCondition.Reason = "ReplicasNotReady"
		replicationCondition.Message = fmt.Sprintf("Only %d/%d replicas are ready", redisStatefulSet.Status.ReadyReplicas, rs.Spec.RedisConfig.Replicas)
	}
	conditions = append(conditions, replicationCondition)

	// Sentinel condition - use real quorum check if sentinels are running
	sentinelCondition := metav1.Condition{
		Type:               "SentinelHealthy",
		ObservedGeneration: rs.Generation,
		LastTransitionTime: now,
	}

	minQuorum := rs.Spec.SentinelConfig.Replicas/2 + 1
	if sentinelStatefulSet.Status.ReadyReplicas >= minQuorum {
		// Perform actual quorum health check
		quorumHealthy, quorumMsg, err := r.checkSentinelQuorum(ctx, rs)
		if err != nil {
			// Fallback to pod count if quorum check fails
			logger.V(1).Info("Quorum check failed, using pod count", "error", err)
			sentinelCondition.Status = metav1.ConditionTrue
			sentinelCondition.Reason = "QuorumReached"
			sentinelCondition.Message = fmt.Sprintf("Sentinel pods ready (%d/%d), quorum check unavailable",
				sentinelStatefulSet.Status.ReadyReplicas, rs.Spec.SentinelConfig.Replicas)
		} else if quorumHealthy {
			sentinelCondition.Status = metav1.ConditionTrue
			sentinelCondition.Reason = "QuorumHealthy"
			sentinelCondition.Message = quorumMsg
		} else {
			sentinelCondition.Status = metav1.ConditionFalse
			sentinelCondition.Reason = "QuorumUnhealthy"
			sentinelCondition.Message = quorumMsg
		}
	} else {
		sentinelCondition.Status = metav1.ConditionFalse
		sentinelCondition.Reason = "QuorumNotReached"
		sentinelCondition.Message = fmt.Sprintf("Insufficient sentinel pods (%d/%d, need %d)",
			sentinelStatefulSet.Status.ReadyReplicas, rs.Spec.SentinelConfig.Replicas, minQuorum)
	}
	conditions = append(conditions, sentinelCondition)

	rs.Status.Conditions = conditions

	if err := r.Status().Update(ctx, rs); err != nil {
		return err
	}

	logger.Info("Updated status", "phase", rs.Status.Phase, "master", rs.Status.MasterNode)
	return nil
}

func (r *RedisSentinelReconciler) getCurrentMaster(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (string, error) {
	// Build list of all sentinel addresses for fallback
	sentinelAddresses := make([]string, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		sentinelAddresses[i] = fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local:26379",
			rs.Name, i, rs.Name, rs.Namespace)
	}

	password := r.getAdminPassword(ctx, rs)
	masterName := rs.Name + "-master"

	// Use sentinel pool to try multiple sentinels
	pool := sentinel.NewSentinelClientPool(sentinelAddresses, password)
	masterAddr, err := pool.GetMasterAddrFromPool(ctx, masterName)
	if err != nil {
		return "", err
	}

	return masterAddr, nil
}

// getAdminPassword retrieves the Redis admin password from the secret
func (r *RedisSentinelReconciler) getAdminPassword(ctx context.Context, rs *redisv1alpha1.RedisSentinel) string {
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

// handleFailover handles post-failover actions to ensure cluster consistency
func (r *RedisSentinelReconciler) handleFailover(ctx context.Context, rs *redisv1alpha1.RedisSentinel, oldMaster, newMaster string) {
	logger := log.FromContext(ctx)
	logger.Info("Handling failover", "oldMaster", oldMaster, "newMaster", newMaster)

	// CRITICAL: Verify quorum health before making any changes
	// This prevents split-brain scenarios where we might reconfigure nodes incorrectly
	quorumHealthy, quorumMsg, err := r.checkSentinelQuorum(ctx, rs)
	if err != nil {
		logger.Error(err, "Failed to check quorum health, aborting failover handling")
		r.Recorder.Event(rs, corev1.EventTypeWarning, "FailoverAborted",
			"Cannot verify sentinel quorum, failover handling aborted for safety")
		return
	}

	if !quorumHealthy {
		logger.Error(nil, "Sentinel quorum not healthy, aborting failover handling",
			"quorumStatus", quorumMsg)
		r.Recorder.Event(rs, corev1.EventTypeWarning, "FailoverAborted",
			fmt.Sprintf("Sentinel quorum not healthy: %s", quorumMsg))
		return
	}

	logger.Info("Quorum verified healthy, proceeding with failover handling", "quorumStatus", quorumMsg)

	adminPassword := r.getAdminPassword(ctx, rs)

	// Parse new master address (format: IP:port)
	newMasterIP, newMasterPort := parseHostPort(newMaster)
	if newMasterIP == "" {
		logger.Error(nil, "Failed to parse new master address", "address", newMaster)
		return
	}

	// Try to reconfigure old master as replica of new master
	if oldMaster != "" {
		oldMasterIP, _ := parseHostPort(oldMaster)
		if oldMasterIP != "" && oldMasterIP != newMasterIP {
			oldMasterAddr := oldMaster
			if !strings.Contains(oldMaster, ":") {
				oldMasterAddr = oldMaster + ":6379"
			}

			// Use a function to ensure proper cleanup with defer
			func() {
				oldClient := redisutils.NewRedisClient(oldMasterAddr, adminPassword)
				defer oldClient.Close()

				err := oldClient.SlaveOf(ctx, newMasterIP, newMasterPort)
				if err != nil {
					logger.Error(err, "Failed to reconfigure old master as replica", "oldMaster", oldMaster)
				} else {
					logger.Info("Reconfigured old master as replica of new master", "oldMaster", oldMaster, "newMaster", newMaster)
					r.Recorder.Event(rs, corev1.EventTypeNormal, "FailoverHandled",
						fmt.Sprintf("Reconfigured %s as replica of %s", oldMaster, newMaster))
				}
			}()
		}
	}

	// Ensure all other replicas are pointing to the new master
	if err := r.ensureReplicasFollowMaster(ctx, rs, newMasterIP, newMasterPort, adminPassword); err != nil {
		logger.Error(err, "Failed to ensure all replicas follow new master")
	}
}

// ensureReplicasFollowMaster verifies and corrects replication configuration for all replicas
func (r *RedisSentinelReconciler) ensureReplicasFollowMaster(ctx context.Context, rs *redisv1alpha1.RedisSentinel, masterIP, masterPort, adminPassword string) error {
	logger := log.FromContext(ctx)

	// List all Redis pods
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": "redis",
		"app.kubernetes.io/instance":  rs.Name,
	}); err != nil {
		return err
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		// Skip the master itself
		if pod.Status.PodIP == masterIP {
			continue
		}

		// Use a function to ensure proper cleanup with defer
		func(podIP, podName string) {
			addr := fmt.Sprintf("%s:6379", podIP)
			redisClient := redisutils.NewRedisClient(addr, adminPassword)
			defer redisClient.Close()

			info, err := redisClient.GetReplicationInfo(ctx)
			if err != nil {
				logger.Error(err, "Failed to get replication info", "pod", podName)
				return
			}

			// Check if this is a replica pointing to the correct master
			role := info["role"]
			if role == "master" {
				// This pod thinks it's a master but shouldn't be - reconfigure it
				logger.Info("Found unexpected master, reconfiguring as replica", "pod", podName)
				if err := redisClient.SlaveOf(ctx, masterIP, masterPort); err != nil {
					logger.Error(err, "Failed to reconfigure pod as replica", "pod", podName)
				}
			} else if role == "slave" {
				// Check if it's following the correct master
				currentMasterHost := info["master_host"]
				currentMasterPort := info["master_port"]
				if currentMasterHost != masterIP || currentMasterPort != masterPort {
					logger.Info("Replica following wrong master, reconfiguring",
						"pod", podName,
						"currentMaster", fmt.Sprintf("%s:%s", currentMasterHost, currentMasterPort),
						"expectedMaster", fmt.Sprintf("%s:%s", masterIP, masterPort))
					if err := redisClient.SlaveOf(ctx, masterIP, masterPort); err != nil {
						logger.Error(err, "Failed to reconfigure replica", "pod", podName)
					}
				}
			}
		}(pod.Status.PodIP, pod.Name)
	}

	return nil
}

// checkSentinelQuorum verifies the actual Sentinel quorum health
func (r *RedisSentinelReconciler) checkSentinelQuorum(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (bool, string, error) {
	// Build sentinel addresses
	sentinelAddresses := make([]string, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		sentinelAddresses[i] = fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local:26379",
			rs.Name, i, rs.Name, rs.Namespace)
	}

	password := r.getAdminPassword(ctx, rs)
	masterName := rs.Name + "-master"

	// Use pool to check quorum from any available sentinel
	pool := sentinel.NewSentinelClientPool(sentinelAddresses, password)
	healthy, count, err := pool.CheckQuorumFromPool(ctx, masterName)
	if err != nil {
		return false, "Unable to verify quorum", err
	}

	if healthy {
		msg := fmt.Sprintf("Quorum healthy: %d sentinels agree", count)
		return true, msg, nil
	}

	return false, "Quorum not reached", nil
}

// parseHostPort parses a host:port string into separate components
func parseHostPort(addr string) (string, string) {
	if addr == "" {
		return "", ""
	}

	// Handle case where port is missing
	if !strings.Contains(addr, ":") {
		return addr, "6379"
	}

	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		return addr, "6379"
	}

	return parts[0], parts[1]
}

func (r *RedisSentinelReconciler) createOrUpdate(ctx context.Context, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	existing := obj.DeepCopyObject().(client.Object)

	err := r.Get(ctx, key, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create
			return r.Create(ctx, obj)
		}
		return err
	}

	// Update (preserve some fields like ClusterIP for Services)
	switch obj := obj.(type) {
	case *corev1.Service:
		existingSvc := existing.(*corev1.Service)
		obj.Spec.ClusterIP = existingSvc.Spec.ClusterIP
		obj.ResourceVersion = existingSvc.ResourceVersion
	case *appsv1.StatefulSet:
		existingSts := existing.(*appsv1.StatefulSet)
		obj.ResourceVersion = existingSts.ResourceVersion
	case *corev1.ConfigMap:
		existingCm := existing.(*corev1.ConfigMap)
		obj.ResourceVersion = existingCm.ResourceVersion
	case *networkingv1.NetworkPolicy:
		existingNetPol := existing.(*networkingv1.NetworkPolicy)
		obj.ResourceVersion = existingNetPol.ResourceVersion
	}

	return r.Update(ctx, obj)
}

func (r *RedisSentinelReconciler) updateMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel) {
	logger := log.FromContext(ctx)

	namespace := rs.Namespace
	name := rs.Name

	// Update cluster info metric
	clusterUp := 0.0
	if rs.Status.Phase == "Running" {
		clusterUp = 1.0
	}
	custmetrics.RedisClusterInfo.WithLabelValues(namespace, name, rs.Status.MasterNode).Set(clusterUp)

	// Update connected replicas
	custmetrics.RedisConnectedReplicas.WithLabelValues(namespace, name).Set(float64(rs.Status.ReadyReplicas))

	// Update sentinel status using real quorum check
	sentinelHealthy := 0.0
	quorumOK, message, err := r.checkSentinelQuorum(ctx, rs)
	if err == nil && quorumOK {
		sentinelHealthy = 1.0
	}
	custmetrics.SentinelStatus.WithLabelValues(namespace, name).Set(sentinelHealthy)
	custmetrics.SentinelQuorumHealth.WithLabelValues(namespace, name).Set(sentinelHealthy)
	custmetrics.SentinelMonitoredMasters.WithLabelValues(namespace, name).Set(1.0)

	// Get sentinel master info for additional metrics
	r.updateSentinelMetrics(ctx, rs)

	// Track failovers (initialized to 0)
	if rs.Status.LastFailoverTime != nil {
		custmetrics.RedisFailoverTotal.WithLabelValues(namespace, name).Add(0)
	}

	// List Redis pods with correct labels
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":  name,
		"app.kubernetes.io/component": "redis",
	}); err != nil {
		logger.Error(err, "Failed to list Redis pods for metrics")
		return
	}

	adminPassword := r.getAdminPassword(ctx, rs)

	// Collect metrics from each Redis pod
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		r.collectPodMetrics(ctx, rs, &pod, adminPassword)
	}

	logger.V(1).Info("Metrics updated", "quorumHealthy", quorumOK, "quorumMessage", message)
}

// updateSentinelMetrics collects metrics from Sentinel
func (r *RedisSentinelReconciler) updateSentinelMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel) {
	logger := log.FromContext(ctx)
	adminPassword := r.getAdminPassword(ctx, rs)
	masterName := rs.Name + "-master"

	// Try to get sentinel info from pool
	sentinelAddresses := make([]string, 0, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		addr := fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local:26379",
			rs.Name, i, rs.Name, rs.Namespace)
		sentinelAddresses = append(sentinelAddresses, addr)
	}

	pool := sentinel.NewSentinelClientPool(sentinelAddresses, adminPassword)
	masterInfo, err := pool.GetMasterFromPool(ctx, masterName)
	if err != nil {
		logger.V(1).Info("Failed to get master info from sentinel", "error", err)
		return
	}

	// Parse sentinel counts
	if masterInfo.NumOtherSentinels != "" {
		var count float64
		fmt.Sscanf(masterInfo.NumOtherSentinels, "%f", &count)
		custmetrics.SentinelKnownSentinels.WithLabelValues(rs.Namespace, rs.Name).Set(count)
	}

	if masterInfo.NumSlaves != "" {
		var count float64
		fmt.Sscanf(masterInfo.NumSlaves, "%f", &count)
		custmetrics.SentinelKnownReplicas.WithLabelValues(rs.Namespace, rs.Name).Set(count)
	}
}

// collectPodMetrics collects all metrics from a single Redis pod
func (r *RedisSentinelReconciler) collectPodMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel, pod *corev1.Pod, adminPassword string) {
	logger := log.FromContext(ctx)
	namespace := rs.Namespace
	name := rs.Name
	podName := pod.Name

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := redisutils.NewRedisClient(addr, adminPassword)
	defer redisClient.Close()

	// --- Memory Metrics ---
	memInfo, err := redisClient.GetMemoryInfo(ctx)
	if err == nil {
		if usedMem, ok := memInfo["used_memory"]; ok {
			var val float64
			fmt.Sscanf(usedMem, "%f", &val)
			custmetrics.RedisUsedMemory.WithLabelValues(namespace, name, podName).Set(val)
		}
		if maxMem, ok := memInfo["maxmemory"]; ok {
			var val float64
			fmt.Sscanf(maxMem, "%f", &val)
			custmetrics.RedisMaxMemory.WithLabelValues(namespace, name, podName).Set(val)
		}
		if fragRatio, ok := memInfo["mem_fragmentation_ratio"]; ok {
			var val float64
			fmt.Sscanf(fragRatio, "%f", &val)
			custmetrics.RedisMemoryFragmentationRatio.WithLabelValues(namespace, name, podName).Set(val)
		}
	}

	// --- Client Metrics ---
	clientInfo, err := redisClient.GetClientInfo(ctx)
	if err == nil {
		if connClients, ok := clientInfo["connected_clients"]; ok {
			var val float64
			fmt.Sscanf(connClients, "%f", &val)
			custmetrics.RedisConnectedClients.WithLabelValues(namespace, name, podName).Set(val)
		}
		if blockedClients, ok := clientInfo["blocked_clients"]; ok {
			var val float64
			fmt.Sscanf(blockedClients, "%f", &val)
			custmetrics.RedisBlockedClients.WithLabelValues(namespace, name, podName).Set(val)
		}
	}

	// --- Persistence Metrics ---
	persistInfo, err := redisClient.GetPersistenceInfo(ctx)
	if err == nil {
		if lastSave, ok := persistInfo["rdb_last_save_time"]; ok {
			var val float64
			fmt.Sscanf(lastSave, "%f", &val)
			custmetrics.RedisRDBLastSaveTime.WithLabelValues(namespace, name, podName).Set(val)
		}
		if changes, ok := persistInfo["rdb_changes_since_last_save"]; ok {
			var val float64
			fmt.Sscanf(changes, "%f", &val)
			custmetrics.RedisRDBChangesSinceLastSave.WithLabelValues(namespace, name, podName).Set(val)
		}
		if aofEnabled, ok := persistInfo["aof_enabled"]; ok {
			val := 0.0
			if aofEnabled == "1" {
				val = 1.0
			}
			custmetrics.RedisAOFEnabled.WithLabelValues(namespace, name, podName).Set(val)
		}
		if aofSize, ok := persistInfo["aof_current_size"]; ok {
			var val float64
			fmt.Sscanf(aofSize, "%f", &val)
			custmetrics.RedisAOFCurrentSize.WithLabelValues(namespace, name, podName).Set(val)
		}
	}

	// --- Replication Metrics ---
	replInfo, err := redisClient.GetReplicationInfo(ctx)
	if err == nil {
		role := replInfo["role"]

		// Replication offset
		if role == "master" {
			if offset, ok := replInfo["master_repl_offset"]; ok {
				var val float64
				fmt.Sscanf(offset, "%f", &val)
				custmetrics.RedisReplicationOffset.WithLabelValues(namespace, name, podName, "master").Set(val)
			}
		} else {
			// Slave/replica
			if offset, ok := replInfo["slave_repl_offset"]; ok {
				var val float64
				fmt.Sscanf(offset, "%f", &val)
				custmetrics.RedisReplicationOffset.WithLabelValues(namespace, name, podName, "replica").Set(val)
			}

			// Master link status
			linkStatus := 0.0
			if replInfo["master_link_status"] == "up" {
				linkStatus = 1.0
			}
			custmetrics.RedisMasterLinkStatus.WithLabelValues(namespace, name, podName).Set(linkStatus)

			// Replication lag (bytes and seconds)
			lagBytes, err := redisClient.GetReplicationLagByOffset(ctx)
			if err == nil {
				custmetrics.RedisReplicationLagBytes.WithLabelValues(namespace, name, podName).Set(float64(lagBytes))
			}

			lag, err := redisClient.GetReplicationLag(ctx)
			if err == nil {
				custmetrics.RedisReplicationLag.WithLabelValues(namespace, name, podName).Set(float64(lag))
			} else {
				custmetrics.RedisReplicationLag.WithLabelValues(namespace, name, podName).Set(-1)
			}
		}
	}

	// --- Stats Metrics ---
	statsInfo, err := redisClient.GetStatsInfo(ctx)
	if err == nil {
		if totalCmds, ok := statsInfo["total_commands_processed"]; ok {
			var val float64
			fmt.Sscanf(totalCmds, "%f", &val)
			// Note: For counters, we should track the value and compute the delta
			// For now, we set the absolute value (Prometheus will compute rate)
			custmetrics.RedisTotalCommandsProcessed.WithLabelValues(namespace, name, podName).Add(0)
		}
		if hits, ok := statsInfo["keyspace_hits"]; ok {
			var val float64
			fmt.Sscanf(hits, "%f", &val)
			custmetrics.RedisKeyspaceHits.WithLabelValues(namespace, name, podName).Add(0)
		}
		if misses, ok := statsInfo["keyspace_misses"]; ok {
			var val float64
			fmt.Sscanf(misses, "%f", &val)
			custmetrics.RedisKeyspaceMisses.WithLabelValues(namespace, name, podName).Add(0)
		}
		if rejectedConns, ok := statsInfo["rejected_connections"]; ok {
			var val float64
			fmt.Sscanf(rejectedConns, "%f", &val)
			custmetrics.RedisRejectedConnections.WithLabelValues(namespace, name, podName).Add(0)
		}
	}

	// --- Keyspace Metrics ---
	keyspaceInfo, err := redisClient.GetKeyspaceInfo(ctx)
	if err == nil {
		for db, info := range keyspaceInfo {
			if strings.HasPrefix(db, "db") {
				// Parse "keys=123,expires=45,avg_ttl=6789"
				parts := strings.Split(info, ",")
				for _, part := range parts {
					if strings.HasPrefix(part, "keys=") {
						var keys float64
						fmt.Sscanf(part, "keys=%f", &keys)
						custmetrics.RedisDBKeys.WithLabelValues(namespace, name, podName, db).Set(keys)
					}
				}
			}
		}
	}

	logger.V(2).Info("Collected metrics from pod", "pod", podName)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisSentinelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisSentinel{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("redissentinel").
		Complete(r)
}
