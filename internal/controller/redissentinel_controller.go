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
	"crypto/tls"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/tlsutil"
	custmetrics "github.com/redguard/redguard/pkg/metrics"
)

// RedisSentinelReconciler reconciles a RedisSentinel object
type RedisSentinelReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// RedisFactory builds Redis and Sentinel clients; nil means DefaultFactory.
	RedisFactory redisclient.Factory
}

// factoryOrDefault lets a zero-value reconciler dial real Redis without wiring.
// Shared by all reconcilers in this package.
func factoryOrDefault(f redisclient.Factory) redisclient.Factory {
	if f != nil {
		return f
	}
	return redisclient.DefaultFactory{}
}

// recordEvent drops the event when no recorder is wired. A reconciler built
// outside SetupWithManager has a nil Recorder, and losing an event must not
// take down the manager. Shared by all reconcilers in this package.
func recordEvent(rec record.EventRecorder, obj runtime.Object, eventType, reason, message string) {
	if rec == nil {
		return
	}
	rec.Event(obj, eventType, reason, message)
}

// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redissentinels/finalizers,verbs=update
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisrestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=redis.redguard.io,resources=redisusers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

const (
	finalizerName = "redis.redguard.io/finalizer"

	// steadyRequeue paces the periodic health pass. failoverRequeue is used
	// while Sentinel and the write Service disagree on the master, so the
	// role label follows a promotion within seconds instead of waiting for
	// the periodic pass.
	steadyRequeue   = 30 * time.Second
	failoverRequeue = 5 * time.Second

	// failoverHandleTimeout bounds the inline post-failover reconfiguration
	// so one unreachable node cannot stall the reconcile worker; unfinished
	// work is retried on the fast requeue.
	failoverHandleTimeout = 15 * time.Second

	// rotationTimeout bounds one online credential push across the whole
	// cluster. The push is idempotent, so an aborted attempt is simply
	// resumed on the next pass rather than left half-done forever.
	rotationTimeout = 30 * time.Second
)

// Keys inside the applied-credentials Secret: the password every node
// currently accepts, and the auth Secret resourceVersion it was read from,
// which is the value stamped on the pod templates.
const (
	appliedPasswordKey = "password"
	appliedVersionKey  = "authSecretResourceVersion"
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

	// Add finalizer if not present. A patch touching only metadata.finalizers,
	// never a full-object update: a PUT re-serializes the spec, and a stored
	// quantity respelled canonically (1024Mi as 1Gi) would trip the storage
	// immutability rules on a CR the user never edited.
	if !controllerutil.ContainsFinalizer(rs, finalizerName) {
		patch := client.MergeFrom(rs.DeepCopy())
		controllerutil.AddFinalizer(rs, finalizerName)
		if err := r.Patch(ctx, rs, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// customConfig is validated here rather than at admission: the reserved-
	// directive logic is not reasonably expressible in CEL. The failure is
	// terminal until the spec is edited, so no requeue follows.
	if err := r.validateSpec(rs); err != nil {
		logger.Error(err, "Rejecting invalid RedisSentinel spec")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "InvalidCustomConfig", err.Error())
		r.setDegraded(ctx, rs, "InvalidCustomConfig", err.Error())
		return ctrl.Result{}, nil
	}

	// Reconcile resources
	if err := r.reconcileConfigMaps(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile ConfigMaps")
		return ctrl.Result{}, err
	}

	if err := r.reconcileServices(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile Services")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "ServiceReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcileNetworkPolicies(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile NetworkPolicies")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "NetworkPolicyReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcilePodDisruptionBudgets(ctx, rs); err != nil {
		logger.Error(err, "Failed to reconcile PodDisruptionBudgets")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "PodDisruptionBudgetReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	scaleDownDeferred, rotationDegraded, err := r.reconcileStatefulSets(ctx, rs)
	if err != nil {
		logger.Error(err, "Failed to reconcile StatefulSets")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "StatefulSetReconcileFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Update status
	writeReady, err := r.updateStatus(ctx, rs, rotationDegraded)
	if err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Update Prometheus metrics
	r.updateMetrics(ctx, rs)

	if !writeReady || scaleDownDeferred {
		return ctrl.Result{RequeueAfter: failoverRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: steadyRequeue}, nil
}

func (r *RedisSentinelReconciler) handleDeletion(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Nothing rewrites these once the CR is gone, so a cluster that is being
	// deleted would otherwise keep reporting its last state forever.
	custmetrics.DeleteClusterSeries(rs.Namespace, rs.Name)

	if controllerutil.ContainsFinalizer(rs, finalizerName) {
		logger.Info("Performing cleanup before deletion")

		// Cleanup logic here if needed (e.g., external resources)

		// Same metadata-only patch as the add: a full-object update would
		// re-serialize the spec and could be rejected by the immutability
		// rules, leaving the CR stuck in Terminating.
		patch := client.MergeFrom(rs.DeepCopy())
		controllerutil.RemoveFinalizer(rs, finalizerName)
		if err := r.Patch(ctx, rs, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// validateSpec covers the spec constraints the CRD schema cannot express.
func (r *RedisSentinelReconciler) validateSpec(rs *redisv1alpha1.RedisSentinel) error {
	if err := builder.ValidateCustomConfig(rs.Spec.RedisConfig.CustomConfig); err != nil {
		return fmt.Errorf("spec.redisConfig.customConfig: %w", err)
	}
	if err := builder.ValidateCustomConfig(rs.Spec.SentinelConfig.CustomConfig); err != nil {
		return fmt.Errorf("spec.sentinelConfig.customConfig: %w", err)
	}
	return nil
}

// setDegraded reports a terminal, spec-caused failure. The next pass that
// reaches updateStatus flips the condition back to False.
func (r *RedisSentinelReconciler) setDegraded(ctx context.Context, rs *redisv1alpha1.RedisSentinel, reason, message string) {
	rs.Status.Phase = phaseDegraded
	rs.Status.ObservedGeneration = rs.Generation
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: rs.Generation,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, rs); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update RedisSentinel status")
	}
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
		builder.BuildRedisReplicasService(rs),
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

// reconcileStatefulSets converges both StatefulSets. The bool reports that a
// Redis scale-down was held back because the master would have been deleted;
// the caller retries on the fast requeue until the promotion has happened.
// rotationDegraded carries a stuck credential push forward as a message rather
// than an error, so the pass still reaches the status update and the master
// label; see reconcileAuthCredentials.
func (r *RedisSentinelReconciler) reconcileStatefulSets(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (scaleDownDeferred bool, rotationDegraded string, err error) {
	logger := log.FromContext(ctx)

	authVersion, rotationDegraded, err := r.reconcileAuthCredentials(ctx, rs)
	if err != nil {
		return false, "", err
	}
	tlsVersion, err := r.tlsSecretVersion(ctx, rs)
	if err != nil {
		return false, "", err
	}

	// Redis StatefulSet
	redisStatefulSet := builder.BuildRedisStatefulSet(rs)
	stampAuthSecretVersion(redisStatefulSet, authVersion)
	stampTLSSecretVersion(redisStatefulSet, tlsVersion)
	if err := controllerutil.SetControllerReference(rs, redisStatefulSet, r.Scheme); err != nil {
		return false, "", err
	}
	scaleDownDeferred = r.holdScaleDownUntilMasterMoves(ctx, rs, redisStatefulSet)
	r.warnOnRollout(ctx, rs, redisStatefulSet,
		"Redis pods restart in descending ordinal order to apply the new configuration; "+
			"Sentinel promotes a replica when the master restarts")
	if err := r.createOrUpdate(ctx, redisStatefulSet); err != nil {
		return false, "", fmt.Errorf("failed to reconcile Redis StatefulSet: %w", err)
	}
	logger.Info("Reconciled Redis StatefulSet", "name", redisStatefulSet.Name)

	// Sentinel StatefulSet
	sentinelStatefulSet := builder.BuildSentinelStatefulSet(rs)
	stampAuthSecretVersion(sentinelStatefulSet, authVersion)
	stampTLSSecretVersion(sentinelStatefulSet, tlsVersion)
	if err := controllerutil.SetControllerReference(rs, sentinelStatefulSet, r.Scheme); err != nil {
		return false, "", err
	}
	r.warnOnSentinelScaleDown(ctx, rs, sentinelStatefulSet)
	r.warnOnRollout(ctx, rs, sentinelStatefulSet,
		"Sentinel pods restart one at a time to apply the new configuration; "+
			"learned state on the volume is kept, and the monitor parameters are "+
			"converged to the spec at runtime rather than by this restart")
	if err := r.createOrUpdate(ctx, sentinelStatefulSet); err != nil {
		return false, "", fmt.Errorf("failed to reconcile Sentinel StatefulSet: %w", err)
	}
	logger.Info("Reconciled Sentinel StatefulSet", "name", sentinelStatefulSet.Name)

	return scaleDownDeferred, rotationDegraded, nil
}

func (r *RedisSentinelReconciler) reconcilePodDisruptionBudgets(ctx context.Context, rs *redisv1alpha1.RedisSentinel) error {
	logger := log.FromContext(ctx)

	budgets := []*policyv1.PodDisruptionBudget{
		builder.BuildRedisPodDisruptionBudget(rs),
		builder.BuildSentinelPodDisruptionBudget(rs),
	}

	for _, pdb := range budgets {
		if err := controllerutil.SetControllerReference(rs, pdb, r.Scheme); err != nil {
			return err
		}
		if err := r.createOrUpdate(ctx, pdb); err != nil {
			return fmt.Errorf("failed to reconcile PodDisruptionBudget %s: %w", pdb.Name, err)
		}
		logger.Info("Reconciled PodDisruptionBudget", "name", pdb.Name)
	}

	return nil
}

// holdScaleDownUntilMasterMoves keeps the Redis StatefulSet at its current size
// while the master occupies an ordinal the scale-down would delete, and asks
// Sentinel to promote a replica that survives. Reports whether the scale-down
// was held.
//
// Deleting the master pod outright works -- Sentinel notices and promotes --
// but only after down-after-milliseconds plus an election, during which the
// write Service has no endpoints and the acknowledged writes that had not yet
// reached a replica are gone with the pod. Forcing the promotion first turns
// that into an ordinary, immediate failover of a healthy master.
//
// Sentinel chooses which replica to promote, so the new master may itself sit
// on a doomed ordinal; the guard simply runs again. It cannot loop forever
// against a healthy cluster, because a promotion updates status.lastFailoverTime
// and no further failover is forced within one failover timeout of it.
func (r *RedisSentinelReconciler) holdScaleDownUntilMasterMoves(ctx context.Context, rs *redisv1alpha1.RedisSentinel, desired *appsv1.StatefulSet) bool {
	logger := log.FromContext(ctx)

	existing := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		// Nothing is running yet, or the read failed and createOrUpdate is
		// about to report it. Either way no pod is about to be deleted.
		return false
	}
	if existing.Spec.Replicas == nil || *desired.Spec.Replicas >= *existing.Spec.Replicas {
		return false
	}

	live := *existing.Spec.Replicas
	target := *desired.Spec.Replicas

	// Pin the live size for this pass. Everything else in the template still
	// converges, so a config edit made in the same pass is not held up.
	hold := func(reason string) bool {
		desired.Spec.Replicas = &live
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "ScaleDownDeferred",
			fmt.Sprintf("Holding %s at %d replicas instead of %d: %s", desired.Name, live, target, reason))
		return true
	}

	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return hold(fmt.Sprintf("cannot resolve the client TLS config to ask Sentinel which pod is master: %v", err))
	}

	masterAddr, err := r.getCurrentMaster(ctx, rs, adminPassword, tlsCfg)
	if err != nil {
		return hold("no Sentinel could name the current master, so the ordinals being removed cannot be shown to exclude it")
	}

	masterOrdinal, ok := r.masterOrdinal(ctx, rs, masterAddr)
	if !ok {
		return hold(fmt.Sprintf("the master Sentinel reports (%s) matches no running Redis pod", masterAddr))
	}
	if masterOrdinal < target {
		return false
	}

	if since := timeSinceLastFailover(rs); since >= 0 && since < failoverTimeoutOf(rs) {
		return hold(fmt.Sprintf("ordinal %d is master and a failover is still settling", masterOrdinal))
	}

	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(sentinelAddresses(rs), adminPassword, tlsCfg)
	if err := pool.FailoverFromPool(ctx, rs.Name+"-master"); err != nil {
		logger.Error(err, "Failed to request a failover before scaling down", "masterOrdinal", masterOrdinal)
		return hold(fmt.Sprintf("ordinal %d is master and Sentinel refused the failover that would move it: %v", masterOrdinal, err))
	}

	logger.Info("Requested a failover off an ordinal that is about to be removed",
		"masterOrdinal", masterOrdinal, "target", target)
	return hold(fmt.Sprintf("ordinal %d is master; Sentinel was asked to promote a replica that survives the scale-down", masterOrdinal))
}

// masterOrdinal resolves the address Sentinel reports to the ordinal of the
// Redis pod serving it.
func (r *RedisSentinelReconciler) masterOrdinal(ctx context.Context, rs *redisv1alpha1.RedisSentinel, masterAddr string) (int32, bool) {
	masterIP, _ := parseHostPort(masterAddr)
	if masterIP == "" {
		return 0, false
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": "redis",
		"app.kubernetes.io/instance":  rs.Name,
	}); err != nil {
		return 0, false
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.PodIP != masterIP || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		return podOrdinal(pod.Name)
	}
	return 0, false
}

// podOrdinal reads the StatefulSet ordinal off a pod name. The ordinal decides
// which pods a scale-down deletes: the highest ones, always.
func podOrdinal(podName string) (int32, bool) {
	i := strings.LastIndex(podName, "-")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(podName[i+1:], 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// timeSinceLastFailover returns how long ago the master last changed, or -1
// when no failover has been observed for this CR.
func timeSinceLastFailover(rs *redisv1alpha1.RedisSentinel) time.Duration {
	if rs.Status.LastFailoverTime == nil {
		return -1
	}
	return time.Since(rs.Status.LastFailoverTime.Time)
}

// failoverTimeoutOf is the window Sentinel itself uses before it will retry a
// failover, and therefore the shortest interval at which forcing another one
// could achieve anything.
func failoverTimeoutOf(rs *redisv1alpha1.RedisSentinel) time.Duration {
	if rs.Spec.SentinelConfig.FailoverTimeout <= 0 {
		return 10 * time.Second
	}
	return time.Duration(rs.Spec.SentinelConfig.FailoverTimeout) * time.Millisecond
}

// pruneRemovedSentinels makes the running sentinels forget peers that no longer
// exist. A removed sentinel stays in every survivor's set, flagged s_down, for
// as long as the survivor runs. Sentinel needs a majority of the set it knows
// to elect the sentinel that performs a failover, so a set shrunk past that
// majority reaches quorum, marks the master o_down, starts a failover and never
// finishes it -- verified against real sentinels: five monitors scaled to two
// left the master o_down for two minutes with no promotion, and a SENTINEL
// RESET on the survivors completed the same failover within five seconds.
//
// The reset also discards the learned replica list, which is rediscovered from
// the master within seconds, so it runs only when the peer count is provably
// stale rather than on every pass.
func (r *RedisSentinelReconciler) pruneRemovedSentinels(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config) {
	logger := log.FromContext(ctx)
	masterName := rs.Name + "-master"

	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(sentinelAddresses(rs), password, tlsCfg)
	info, err := pool.GetMasterFromPool(ctx, masterName)
	if err != nil {
		logger.V(1).Info("Cannot read the sentinel peer count", "error", err)
		return
	}

	others, err := strconv.Atoi(info.NumOtherSentinels)
	if err != nil {
		return
	}
	known := int32(others) + 1
	if known <= rs.Spec.SentinelConfig.Replicas {
		return
	}

	if err := pool.ResetMasterAll(ctx, masterName); err != nil {
		logger.Error(err, "Failed to reset the sentinel view of the master", "known", known)
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "SentinelPeersResetFailed", fmt.Sprintf(
			"Sentinels still count %d peers but only %d exist, and the reset failed: %v",
			known, rs.Spec.SentinelConfig.Replicas, err))
		return
	}

	logger.Info("Reset the sentinel view of the master", "known", known, "expected", rs.Spec.SentinelConfig.Replicas)
	recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "SentinelPeersReset", fmt.Sprintf(
		"Sentinels counted %d peers but only %d exist; reset their view so a failover can still win an election",
		known, rs.Spec.SentinelConfig.Replicas))
}

// warnOnSentinelScaleDown announces the loss of failure detectors. Removing a
// sentinel does not remove it from the survivors' view: they keep it in
// SENTINEL sentinels until a SENTINEL RESET, so the majority they require to
// elect the sentinel that runs a failover is still counted from the old size.
func (r *RedisSentinelReconciler) warnOnSentinelScaleDown(ctx context.Context, rs *redisv1alpha1.RedisSentinel, desired *appsv1.StatefulSet) {
	existing := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		return
	}
	if existing.Spec.Replicas == nil || *desired.Spec.Replicas >= *existing.Spec.Replicas {
		return
	}

	target := *desired.Spec.Replicas
	recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "SentinelScaleDown", fmt.Sprintf(
		"Removing sentinels %d..%d leaves %d monitoring with a quorum of %d. "+
			"The surviving sentinels still count the removed ones as known peers until a SENTINEL RESET, "+
			"so the majority they need to elect a failover leader is still computed from %d",
		target, *existing.Spec.Replicas-1, target, rs.Spec.SentinelConfig.Quorum, *existing.Spec.Replicas))
}

// sentinelAddresses is the stable per-pod DNS of every Sentinel in the set.
func sentinelAddresses(rs *redisv1alpha1.RedisSentinel) []string {
	addrs := make([]string, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		addrs[i] = fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local:26379",
			rs.Name, i, rs.Name, rs.Namespace)
	}
	return addrs
}

// appliedAuthSecretName is the operator-owned Secret recording the password
// the cluster currently accepts.
func appliedAuthSecretName(rs *redisv1alpha1.RedisSentinel) string {
	return rs.Name + "-auth-state"
}

// reconcileAuthCredentials returns the value to stamp as the auth Secret
// version on both pod templates, or "" when the CR configures no
// authentication or the referenced Secret is absent (the pods cannot start
// without it either; the version appears and rolls them once the Secret does).
//
// A changed password is pushed to every running node online before the new
// version is returned: rolling first restarts each pod into the new credential
// while its un-rolled peers still require the old one, which breaks
// replication and Sentinel authentication for the whole roll and stalls it on
// the readiness probe. After a successful push the roll only makes the change
// durable. The password last pushed is kept in an operator-owned Secret,
// because the user Secret holds only the new value and the operator must still
// authenticate against nodes that accept the old one.
//
// A failed push degrades the pass instead of failing it: it is reported as the
// non-empty degraded message and the previously applied stamp is carried
// forward, so the pods do not roll onto a password their peers never accepted.
// The push often fails during a partition, which is exactly when the rest of
// the pass -- the status update and the master role label -- must still run.
func (r *RedisSentinelReconciler) reconcileAuthCredentials(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (version, degraded string, err error) {
	auth := rs.Spec.RedisConfig.Auth
	if auth == nil || auth.SecretName == "" {
		return "", "", nil
	}

	userSecret := &corev1.Secret{}
	key := types.NamespacedName{Name: auth.SecretName, Namespace: rs.Namespace}
	if err := r.Get(ctx, key, userSecret); err != nil {
		if errors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read auth secret %s: %w", auth.SecretName, err)
	}
	desired := string(userSecret.Data["password"])

	// Rejected before anything is pushed or stamped. Both failure modes are
	// worse than refusing: an empty value renders the default ACL user nopass
	// on every node, and a value the config parser cannot carry becomes, once
	// the rotation narrowed the old password away, the only accepted
	// credential no pod can boot with. The previously applied stamp is
	// carried forward so the pods do not roll onto the rejected value.
	if verr := builder.ValidateAuthPassword(desired); verr != nil {
		msg := fmt.Sprintf("auth secret %s: %v", auth.SecretName, verr)
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "InvalidAuthPassword", msg)
		r.setDegraded(ctx, rs, "InvalidAuthPassword", msg)
		applied := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: appliedAuthSecretName(rs), Namespace: rs.Namespace}, applied); err == nil {
			return string(applied.Data[appliedVersionKey]), msg, nil
		}
		return "", msg, nil
	}

	applied := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: appliedAuthSecretName(rs), Namespace: rs.Namespace}, applied)
	if errors.IsNotFound(err) {
		// First contact: converge the runtime state (idempotent when it
		// already matches, and it also carries a cluster that predates auth
		// into requiring it) and record what the cluster accepts from now on.
		// No stamp has ever been applied, so there is none to carry forward.
		if pushErr := r.pushCredentials(ctx, rs, desired, ""); pushErr != nil {
			pushErr = fmt.Errorf("online credential push: %w", pushErr)
			r.reportRotationFailure(ctx, rs, pushErr)
			return "", pushErr.Error(), nil
		}
		if err := r.writeAppliedAuth(ctx, rs, desired, userSecret.ResourceVersion); err != nil {
			return "", "", err
		}
		return userSecret.ResourceVersion, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read applied credentials secret: %w", err)
	}

	previous := string(applied.Data[appliedPasswordKey])
	if previous == desired {
		// Track the Secret's resourceVersion even when only metadata moved:
		// the stamp must equal the live version or the next comparison
		// misreads an unchanged password as a pending rotation.
		if string(applied.Data[appliedVersionKey]) != userSecret.ResourceVersion {
			if err := r.writeAppliedAuth(ctx, rs, desired, userSecret.ResourceVersion); err != nil {
				return "", "", err
			}
		}
		return userSecret.ResourceVersion, "", nil
	}

	if pushErr := r.pushCredentials(ctx, rs, desired, previous); pushErr != nil {
		pushErr = fmt.Errorf("online credential rotation: %w", pushErr)
		r.reportRotationFailure(ctx, rs, pushErr)
		return string(applied.Data[appliedVersionKey]), pushErr.Error(), nil
	}
	if err := r.writeAppliedAuth(ctx, rs, desired, userSecret.ResourceVersion); err != nil {
		return "", "", err
	}
	recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "CredentialsRotated",
		"Pushed the rotated password to every running Redis node and Sentinel; "+
			"the pods now restart only to make the change durable")
	return userSecret.ResourceVersion, "", nil
}

// reportRotationFailure surfaces an incomplete push. The cluster still agrees
// on the previously applied password, so nothing is rolled and the push is
// retried; silent partial application is the one state that must not exist.
func (r *RedisSentinelReconciler) reportRotationFailure(ctx context.Context, rs *redisv1alpha1.RedisSentinel, pushErr error) {
	recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "CredentialRotationFailed", pushErr.Error())
	r.setDegraded(ctx, rs, "CredentialRotationFailed", pushErr.Error())
}

// writeAppliedAuth records password and the auth Secret version it was read
// from, creating the applied-credentials Secret on first use. Owned by the CR
// so it is garbage collected with it.
func (r *RedisSentinelReconciler) writeAppliedAuth(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password, version string) error {
	key := types.NamespacedName{Name: appliedAuthSecretName(rs), Namespace: rs.Namespace}
	data := map[string][]byte{
		appliedPasswordKey: []byte(password),
		appliedVersionKey:  []byte(version),
	}

	return retry.OnError(retry.DefaultRetry, isRaceError, func() error {
		existing := &corev1.Secret{}
		err := r.Get(ctx, key, existing)
		if errors.IsNotFound(err) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      key.Name,
					Namespace: key.Namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "redguard-operator",
						"app.kubernetes.io/instance":   rs.Name,
					},
				},
				Data: data,
			}
			if err := controllerutil.SetControllerReference(rs, secret, r.Scheme); err != nil {
				return err
			}
			return r.Create(ctx, secret)
		}
		if err != nil {
			return err
		}
		existing.Data = data
		return r.Update(ctx, existing)
	})
}

// pushCredentials moves every running node from previous to desired online, in
// three cluster-wide phases: widen (every node accepts both passwords), switch
// outbound (every dialer presents the new one), narrow (the old one stops
// being accepted). At every instant each presented credential is accepted by
// its peer, so replication links stay up and Sentinel never loses the master;
// there is no ordering window to get wrong. Each phase is gated on the
// previous one completing on every node, and everything is idempotent, so an
// aborted push is resumed by simply running it again. Verified against real
// redis:7-alpine containers: sentinel mode has no CONFIG command, so widening
// and narrowing there go through the default user's ACL, which sentinel
// persists into its state file by itself.
func (r *RedisSentinelReconciler) pushCredentials(ctx context.Context, rs *redisv1alpha1.RedisSentinel, desired, previous string) error {
	ctx, cancel := context.WithTimeout(ctx, rotationTimeout)
	defer cancel()

	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return fmt.Errorf("resolve client TLS config: %w", err)
	}
	factory := factoryOrDefault(r.RedisFactory)

	redisAddrs, err := r.runningPodAddresses(ctx, rs, "redis", 6379)
	if err != nil {
		return err
	}
	sentinelAddrs, err := r.runningPodAddresses(ctx, rs, "sentinel", 26379)
	if err != nil {
		return err
	}

	// Every redis node must be reachable before anything is written anywhere:
	// a push that cannot finish must not start.
	clients := make(map[string]redisclient.Client, len(redisAddrs))
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for _, addr := range redisAddrs {
		c, err := dialWithEither(ctx, factory, addr, desired, previous, tlsCfg)
		if err != nil {
			return fmt.Errorf("redis node %s: %w", addr, err)
		}
		clients[addr] = c
	}

	// forEachSentinel runs op against every sentinel individually, trying the
	// new credential first (already valid on a resumed push) and falling back
	// to the previous one.
	forEachSentinel := func(what string, op func(redisclient.Sentinel) error) error {
		for _, addr := range sentinelAddrs {
			err := op(factory.NewSentinelPool([]string{addr}, desired, tlsCfg))
			if err != nil {
				if prevErr := op(factory.NewSentinelPool([]string{addr}, previous, tlsCfg)); prevErr != nil {
					return fmt.Errorf("%s on sentinel %s: with the new password: %v; with the previous one: %w", what, addr, err, prevErr)
				}
			}
		}
		return nil
	}

	// Phase 1: widen.
	for _, addr := range redisAddrs {
		if err := clients[addr].ACLSetUser(ctx, "default", ">"+desired); err != nil {
			return fmt.Errorf("widen accepted passwords on redis node %s: %w", addr, err)
		}
	}
	if err := forEachSentinel("widen accepted passwords", func(p redisclient.Sentinel) error {
		return p.AddPasswordAll(ctx, desired)
	}); err != nil {
		return err
	}

	// Phase 2: switch outbound.
	for _, addr := range redisAddrs {
		if err := clients[addr].ConfigSet(ctx, "masterauth", desired); err != nil {
			return fmt.Errorf("switch masterauth on redis node %s: %w", addr, err)
		}
	}
	masterName := rs.Name + "-master"
	if err := forEachSentinel("switch outbound credentials", func(p redisclient.Sentinel) error {
		return p.SetOutboundPasswordAll(ctx, masterName, desired)
	}); err != nil {
		return err
	}

	// Phase 3: narrow.
	for _, addr := range redisAddrs {
		if err := clients[addr].ConfigSet(ctx, "requirepass", desired); err != nil {
			return fmt.Errorf("narrow accepted passwords on redis node %s: %w", addr, err)
		}
	}
	return forEachSentinel("narrow accepted passwords", func(p redisclient.Sentinel) error {
		return p.ResetPasswordAll(ctx, desired)
	})
}

// dialWithEither connects to one redis node with whichever of the two
// credentials it currently accepts, new one first.
func dialWithEither(ctx context.Context, factory redisclient.Factory, addr, desired, previous string, tlsCfg *tls.Config) (redisclient.Client, error) {
	c := factory.NewClient(addr, desired, tlsCfg)
	newErr := c.Ping(ctx)
	if newErr == nil {
		return c, nil
	}
	_ = c.Close()

	c = factory.NewClient(addr, previous, tlsCfg)
	prevErr := c.Ping(ctx)
	if prevErr == nil {
		return c, nil
	}
	_ = c.Close()
	return nil, fmt.Errorf("not reachable with the new password (%v) nor the previous one (%v)", newErr, prevErr)
}

// runningPodAddresses lists the dial addresses of the running pods of one
// component, sorted so multi-node operations run in a stable order.
func (r *RedisSentinelReconciler) runningPodAddresses(ctx context.Context, rs *redisv1alpha1.RedisSentinel, component string, port int) ([]string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": component,
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
		addrs = append(addrs, fmt.Sprintf("%s:%d", pod.Status.PodIP, port))
	}
	slices.Sort(addrs)
	return addrs, nil
}

// tlsSecretVersion returns the value to stamp as the TLS secret version on
// both pod templates, or "" when TLS is disabled or the certificate Secret is
// absent. Both servers load their certificates once at startup, so a renewal
// reaches a running pod only through the roll this stamp triggers.
func (r *RedisSentinelReconciler) tlsSecretVersion(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (string, error) {
	spec := rs.Spec.TLS
	if spec == nil || !spec.Enabled || spec.CertificateSecretRef == "" {
		return "", nil
	}

	cert := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.CertificateSecretRef, Namespace: rs.Namespace}, cert); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read TLS certificate secret %s: %w", spec.CertificateSecretRef, err)
	}
	version := cert.ResourceVersion

	if spec.CASecretRef != "" && spec.CASecretRef != spec.CertificateSecretRef {
		ca := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: spec.CASecretRef, Namespace: rs.Namespace}, ca); err != nil {
			if errors.IsNotFound(err) {
				return version, nil
			}
			return "", fmt.Errorf("read TLS CA secret %s: %w", spec.CASecretRef, err)
		}
		version += "/" + ca.ResourceVersion
	}
	return version, nil
}

// stampAuthSecretVersion records on the pod template which version of the auth
// Secret the pods read at startup. Nothing else in the template changes when a
// password is rotated, so without it the pods keep presenting the old
// credential until an unrelated event happens to restart them.
func stampAuthSecretVersion(sts *appsv1.StatefulSet, version string) {
	if version == "" {
		return
	}
	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = map[string]string{}
	}
	sts.Spec.Template.Annotations[builder.AuthSecretVersionAnnotation] = version
}

// stampTLSSecretVersion records on the pod template which version of the TLS
// secrets the pods loaded their certificates from, so a renewal rolls them.
func stampTLSSecretVersion(sts *appsv1.StatefulSet, version string) {
	if version == "" {
		return
	}
	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = map[string]string{}
	}
	sts.Spec.Template.Annotations[builder.TLSSecretVersionAnnotation] = version
}

// warnOnRollout announces a pod restart before it happens. Rolling a
// StatefulSet is how a config or password change reaches a running server, but
// it is not free, and an operator who edited one directive should not have to
// deduce from a failover event that the two are connected.
func (r *RedisSentinelReconciler) warnOnRollout(ctx context.Context, rs *redisv1alpha1.RedisSentinel, desired *appsv1.StatefulSet, message string) {
	existing := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		// Nothing is running yet, or the read failed and createOrUpdate is
		// about to report it. Either way there is no restart to announce.
		return
	}
	if podConfigIdentity(existing) == podConfigIdentity(desired) {
		return
	}

	// The rotated credential was already pushed to every running node before
	// the stamp changed, so this roll only makes it durable.
	if authSecretVersionOf(existing) != authSecretVersionOf(desired) {
		message += "; the auth Secret changed and the new password was already " +
			"pushed online, this roll makes it durable"
	}
	if tlsSecretVersionOf(existing) != tlsSecretVersionOf(desired) {
		message += "; the TLS secrets changed, so the pods restart to load the renewed certificates"
	}

	recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "ConfigRollout",
		fmt.Sprintf("%s: %s", desired.Name, message))
}

// podConfigIdentity is the set of annotations that decide whether the
// StatefulSet controller replaces the pods.
func podConfigIdentity(sts *appsv1.StatefulSet) string {
	return sts.Spec.Template.Annotations[builder.ConfigHashAnnotation] + "/" +
		authSecretVersionOf(sts) + "/" + tlsSecretVersionOf(sts)
}

func authSecretVersionOf(sts *appsv1.StatefulSet) string {
	return sts.Spec.Template.Annotations[builder.AuthSecretVersionAnnotation]
}

func tlsSecretVersionOf(sts *appsv1.StatefulSet) string {
	return sts.Spec.Template.Annotations[builder.TLSSecretVersionAnnotation]
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

	recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "NetworkPoliciesReconciled", "Network policies successfully created/updated")

	return nil
}

// updateStatus refreshes the status subresource and maintains the master role
// label. The returned bool reports whether a running pod currently backs the
// write Service; false asks the caller for a fast requeue so the label chases
// an in-flight failover instead of waiting for the periodic pass.
// rotationDegraded, when non-empty, is a credential push that could not finish
// this pass; it keeps the Degraded condition true instead of letting a
// completed pass clear it while the rotation is still pending.
func (r *RedisSentinelReconciler) updateStatus(ctx context.Context, rs *redisv1alpha1.RedisSentinel, rotationDegraded string) (bool, error) {
	logger := log.FromContext(ctx)

	// Get Redis StatefulSet
	redisStatefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rs.Name + "-redis",
		Namespace: rs.Namespace,
	}, redisStatefulSet); err != nil {
		if errors.IsNotFound(err) {
			return true, nil // Not created yet
		}
		return false, err
	}

	// Get Sentinel StatefulSet
	sentinelStatefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      rs.Name + "-sentinel",
		Namespace: rs.Namespace,
	}, sentinelStatefulSet); err != nil {
		if errors.IsNotFound(err) {
			return true, nil // Not created yet
		}
		return false, err
	}

	// Update status
	rs.Status.ReadyReplicas = redisStatefulSet.Status.ReadyReplicas
	rs.Status.ReadySentinels = sentinelStatefulSet.Status.ReadyReplicas

	// Determine phase
	if redisStatefulSet.Status.ReadyReplicas == 0 {
		rs.Status.Phase = phaseCreating
	} else if redisStatefulSet.Status.ReadyReplicas < rs.Spec.RedisConfig.Replicas {
		rs.Status.Phase = phaseScaling
	} else if sentinelStatefulSet.Status.ReadyReplicas < rs.Spec.SentinelConfig.Replicas {
		rs.Status.Phase = phaseConfiguringSentinel
	} else {
		rs.Status.Phase = phaseRunning
	}

	// Resolved once here; every Redis and Sentinel connection made below and in
	// the spawned failover handler reuses these. A TLS resolution failure aborts
	// the status update: falling back to plaintext against TLS-only pods would
	// misreport the cluster as masterless.
	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		return false, fmt.Errorf("resolve client TLS config: %w", err)
	}

	// Try to get current master from Sentinel. Before any sentinel is ready
	// there is no master to label; the StatefulSet watch fires when that
	// changes, so no fast requeue is requested for that state.
	writeReady := true
	if sentinelStatefulSet.Status.ReadyReplicas > 0 {
		masterNode, err := r.getCurrentMaster(ctx, rs, adminPassword, tlsCfg)
		if err != nil {
			logger.Info("Could not determine master node", "error", err)
			writeReady = false
		} else {
			failoverSettled := true
			masterMoved := rs.Status.MasterNode != "" && rs.Status.MasterNode != masterNode
			if masterMoved {
				// Failover detected
				logger.Info("Failover detected", "old", rs.Status.MasterNode, "new", masterNode)
				recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "FailoverDetected",
					fmt.Sprintf("Master changed from %s to %s", rs.Status.MasterNode, masterNode))
				now := metav1.Now()
				rs.Status.LastFailoverTime = &now
				custmetrics.RedisFailoverTotal.WithLabelValues(rs.Namespace, rs.Name).Inc()

				// Reconfiguration runs inline, never in a goroutine: a
				// detached handler acts on a snapshot that another failover
				// can invalidate, dies with the operator mid-write, and a
				// panic inside it would take down the whole process.
				handleCtx, cancel := context.WithTimeout(ctx, failoverHandleTimeout)
				failoverSettled = r.handleFailover(handleCtx, rs, rs.Status.MasterNode, masterNode, adminPassword, tlsCfg)
				cancel()
			}
			rs.Status.MasterNode = masterNode

			// A pod that boots while Sentinel still reports the previous
			// master follows a dead address, and the failover branch above
			// cannot repair it: the master change it reacts to is already in
			// the past by then. Only on a settled pass, and never while the
			// master is moving -- mid-failover the reported address may be a
			// promotion Sentinel has not agreed on, and acting on it is how
			// the freshly promoted master gets demoted.
			if !masterMoved && failoverSettled {
				r.repointStrayReplicas(ctx, rs, masterNode, adminPassword, tlsCfg)
				// Same settledness gate: pruning compares an enumeration of
				// every node against the declared RedisUser set, and a pass
				// where the master is still moving is not a coherent view.
				r.pruneOrphanedACLUsers(ctx, rs, adminPassword, tlsCfg)
			}

			labeled, err := r.reconcileMasterLabel(ctx, rs, masterNode)
			if err != nil {
				// Label errors are retried on the fast requeue rather than
				// failing the pass: the status update below must still land.
				logger.Error(err, "Failed to reconcile the master role label")
				writeReady = false
			} else {
				writeReady = labeled
			}
			if !failoverSettled {
				writeReady = false
			}
		}
	}

	// Conditions are merged, never rebuilt: meta.SetStatusCondition keeps
	// LastTransitionTime when the state is unchanged, so an unchanged pass writes
	// nothing and the status watch does not schedule the next pass.
	conditions := []metav1.Condition{}

	// A promotion needs a second instance to promote. One is a legal spec, so
	// this is reported rather than refused.
	haCondition := metav1.Condition{
		Type:               "HighlyAvailable",
		ObservedGeneration: rs.Generation,
		Status:             metav1.ConditionTrue,
		Reason:             "FailoverTargetAvailable",
		Message:            fmt.Sprintf("%d Redis instances, so Sentinel has a replica to promote", rs.Spec.RedisConfig.Replicas),
	}
	if rs.Spec.RedisConfig.Replicas < 2 {
		haCondition.Status = metav1.ConditionFalse
		haCondition.Reason = "NoFailoverTarget"
		haCondition.Message = "redisConfig.replicas is 1: there is no replica to promote, so losing the master loses the cluster until it comes back"
	}
	conditions = append(conditions, haCondition)

	// The previous pass may have ended in setDegraded; a completed pass clears
	// it -- unless a credential push is still stuck, which this pass tolerated
	// but must keep reporting.
	degradedCondition := metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: rs.Generation,
		Reason:             "ReconcileSuccess",
		Message:            "The spec was accepted and applied",
	}
	if rotationDegraded != "" {
		degradedCondition.Status = metav1.ConditionTrue
		degradedCondition.Reason = "CredentialRotationFailed"
		degradedCondition.Message = rotationDegraded
		rs.Status.Phase = phaseDegraded
	}
	conditions = append(conditions, degradedCondition)

	// Available condition
	availableCondition := metav1.Condition{
		Type:               "Available",
		ObservedGeneration: rs.Generation,
	}
	if rs.Status.Phase == phaseRunning {
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
	}

	minQuorum := rs.Spec.SentinelConfig.Replicas/2 + 1
	if sentinelStatefulSet.Status.ReadyReplicas >= minQuorum {
		// Perform actual quorum health check
		quorumHealthy, quorumMsg, err := r.checkSentinelQuorum(ctx, rs, adminPassword, tlsCfg)
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

	if sentinelStatefulSet.Status.ReadyReplicas >= rs.Spec.SentinelConfig.Replicas {
		r.pruneRemovedSentinels(ctx, rs, adminPassword, tlsCfg)
		conditions = append(conditions, r.syncSentinelMonitorConfig(ctx, rs, adminPassword, tlsCfg))
	}

	for _, cond := range conditions {
		meta.SetStatusCondition(&rs.Status.Conditions, cond)
	}
	rs.Status.ObservedGeneration = rs.Generation

	if err := r.Status().Update(ctx, rs); err != nil {
		return false, err
	}

	logger.Info("Updated status", "phase", rs.Status.Phase, "master", rs.Status.MasterNode)
	return writeReady, nil
}

// reconcileMasterLabel stamps the role label on the pod Sentinel reports as
// master and strips it from every other Redis pod, so the write Service
// endpoints follow a failover. Returns whether a running pod carries the
// label. While the operator is down the label cannot move, so the write
// Service keeps selecting the demoted pod until the next reconcile; only a
// controller can make a Service track a Sentinel promotion.
func (r *RedisSentinelReconciler) reconcileMasterLabel(ctx context.Context, rs *redisv1alpha1.RedisSentinel, masterAddr string) (bool, error) {
	logger := log.FromContext(ctx)

	masterIP, _ := parseHostPort(masterAddr)
	if masterIP == "" {
		return false, nil
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": "redis",
		"app.kubernetes.io/instance":  rs.Name,
	}); err != nil {
		return false, err
	}

	labeled := false
	for i := range podList.Items {
		pod := &podList.Items[i]

		// A terminating or non-running pod gets no label even when Sentinel
		// still names its IP: its endpoints are gone anyway, and holding the
		// label would delay the fast requeue that chases the promotion.
		isMaster := pod.Status.PodIP == masterIP &&
			pod.Status.Phase == corev1.PodRunning &&
			pod.DeletionTimestamp.IsZero()
		hasLabel := pod.Labels[builder.RoleLabelKey] == builder.RoleMaster

		if isMaster {
			labeled = true
		}
		if isMaster == hasLabel {
			continue
		}

		patch := client.MergeFrom(pod.DeepCopy())
		if isMaster {
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			pod.Labels[builder.RoleLabelKey] = builder.RoleMaster
			logger.Info("Labeling pod as master", "pod", pod.Name, "ip", masterIP)
		} else {
			delete(pod.Labels, builder.RoleLabelKey)
			logger.Info("Removing master label", "pod", pod.Name)
		}
		if err := r.Patch(ctx, pod, patch); err != nil {
			return false, err
		}
	}

	return labeled, nil
}

// syncSentinelMonitorConfig converges the monitor parameters every running
// sentinel applies towards the spec and reports the outcome as a condition.
// Sentinel keeps durable state on its PVC, so the ConfigMap template is read
// once, on first boot: editing quorum or the timers in the spec rolls the pods
// and changes nothing. The live values are therefore repaired at runtime, and
// because SENTINEL SET never propagates between sentinels, every member is
// read and repaired individually. down-after-milliseconds is ceded to an
// active RedisRestore holding the quiesce: see restoreHoldingQuiesce.
func (r *RedisSentinelReconciler) syncSentinelMonitorConfig(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config) metav1.Condition {
	logger := log.FromContext(ctx)
	masterName := rs.Name + "-master"
	factory := factoryOrDefault(r.RedisFactory)

	cond := metav1.Condition{
		Type:               "SentinelConfigInSync",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: rs.Generation,
		Reason:             "InSync",
		Message:            "Every running sentinel applies the monitor parameters in the spec",
	}

	addrs, err := r.runningPodAddresses(ctx, rs, "sentinel", 26379)
	if err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PodListFailed"
		cond.Message = err.Error()
		return cond
	}

	quiescedBy, err := r.restoreHoldingQuiesce(ctx, rs)
	if err != nil {
		// Not knowing whether a restore holds the quiesce means a repair here
		// could re-arm failure detection mid-restart and cost the restored
		// dataset; skip the pass instead.
		cond.Status = metav1.ConditionFalse
		cond.Reason = "RestoreLookupFailed"
		cond.Message = err.Error()
		return cond
	}

	desired := []struct{ option, value string }{
		{"failover-timeout", fmt.Sprintf("%d", rs.Spec.SentinelConfig.FailoverTimeout)},
		{"parallel-syncs", fmt.Sprintf("%d", rs.Spec.SentinelConfig.ParallelSyncs)},
		{"quorum", fmt.Sprintf("%d", rs.Spec.SentinelConfig.Quorum)},
	}
	if quiescedBy == "" {
		desired = append(desired, struct{ option, value string }{
			"down-after-milliseconds", fmt.Sprintf("%d", rs.Spec.SentinelConfig.DownAfterMilliseconds)})
	}

	var repaired, failures []string
	for _, addr := range addrs {
		pool := factory.NewSentinelPool([]string{addr}, password, tlsCfg)
		info, err := pool.GetMasterFromPool(ctx, masterName)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", addr, err))
			continue
		}
		reported := map[string]string{
			"down-after-milliseconds": info.DownAfterMilliseconds,
			"failover-timeout":        info.FailoverTimeout,
			"parallel-syncs":          info.ParallelSyncs,
			"quorum":                  info.Quorum,
		}
		for _, want := range desired {
			if reported[want.option] == want.value {
				continue
			}
			if err := pool.SetMasterOptionAll(ctx, masterName, want.option, want.value); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %s: %v", addr, want.option, err))
				continue
			}
			repaired = append(repaired, fmt.Sprintf("%s: %s %s -> %s", addr, want.option, reported[want.option], want.value))
		}
	}

	if len(repaired) > 0 {
		logger.Info("Repaired drifted sentinel monitor parameters", "repairs", repaired)
		recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "SentinelConfigRepaired",
			"Reapplied the spec's monitor parameters: "+strings.Join(repaired, ", "))
	}
	if len(failures) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "DriftRepairFailed"
		cond.Message = strings.Join(failures, "; ")
	} else if quiescedBy != "" {
		cond.Reason = "RestoreQuiesced"
		cond.Message = "down-after-milliseconds is held raised by RedisRestore " + quiescedBy + " during its master restart"
	}
	return cond
}

// restoreHoldingQuiesce names the RedisRestore that has quiesced this
// cluster's sentinels, or "" when none does. The restore raises
// down-after-milliseconds before its controlled master restart; converging the
// value back to the spec inside that window re-arms failure detection while
// the master is deliberately down, and the resulting failover resyncs the
// restored dataset away. The marker is the restore's own persisted breadcrumb,
// written before the first SENTINEL SET, so an operator restart cannot
// desynchronise the two controllers. A terminal restore does not hold the
// quiesce even when the breadcrumb survived (its unquiesce failed): from there
// this reconciler is the fallback that converges the spec value back, and a
// deleted restore releases the hold the same way.
func (r *RedisSentinelReconciler) restoreHoldingQuiesce(ctx context.Context, rs *redisv1alpha1.RedisSentinel) (string, error) {
	restores := &redisv1alpha1.RedisRestoreList{}
	if err := r.List(ctx, restores, client.InNamespace(rs.Namespace)); err != nil {
		return "", err
	}
	for i := range restores.Items {
		restore := &restores.Items[i]
		if restore.Spec.RedisClusterRef != rs.Name ||
			restore.Status.QuiescedDownAfterMilliseconds == 0 ||
			restore.Status.Phase == redisv1alpha1.RestorePhaseCompleted ||
			restore.Status.Phase == redisv1alpha1.RestorePhaseFailed {
			continue
		}
		return restore.Name, nil
	}
	return "", nil
}

func (r *RedisSentinelReconciler) getCurrentMaster(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config) (string, error) {
	masterName := rs.Name + "-master"

	// Use sentinel pool to try multiple sentinels
	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(sentinelAddresses(rs), password, tlsCfg)
	masterAddr, err := pool.GetMasterAddrFromPool(ctx, masterName)
	if err != nil {
		return "", err
	}

	return masterAddr, nil
}

// getAdminPassword retrieves the password the cluster currently accepts. The
// applied-credentials Secret is authoritative once it exists: during a pending
// rotation the user Secret already holds the new password while every node
// still requires the previous one, and the operator must keep authenticating
// throughout.
func (r *RedisSentinelReconciler) getAdminPassword(ctx context.Context, rs *redisv1alpha1.RedisSentinel) string {
	if rs.Spec.RedisConfig.Auth == nil || rs.Spec.RedisConfig.Auth.SecretName == "" {
		return ""
	}

	applied := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      appliedAuthSecretName(rs),
		Namespace: rs.Namespace,
	}, applied); err == nil {
		if password, ok := applied.Data[appliedPasswordKey]; ok {
			return string(password)
		}
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

// handleFailover reconfigures replication after a detected master change and
// reports whether the work completed against a confirmed-stable master; false
// asks the caller for a fast requeue. The newMaster snapshot can already be
// stale by the time this runs, so Sentinel is re-queried before any SLAVEOF:
// demoting from an outdated address hits the freshly promoted master and
// discards its acknowledged writes.
func (r *RedisSentinelReconciler) handleFailover(ctx context.Context, rs *redisv1alpha1.RedisSentinel, oldMaster, newMaster, adminPassword string, tlsCfg *tls.Config) bool {
	logger := log.FromContext(ctx)
	logger.Info("Handling failover", "oldMaster", oldMaster, "newMaster", newMaster)

	// CRITICAL: Verify quorum health before making any changes
	// This prevents split-brain scenarios where we might reconfigure nodes incorrectly
	quorumHealthy, quorumMsg, err := r.checkSentinelQuorum(ctx, rs, adminPassword, tlsCfg)
	if err != nil {
		logger.Error(err, "Failed to check quorum health, aborting failover handling")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "FailoverAborted",
			"Cannot verify sentinel quorum, failover handling aborted for safety")
		return false
	}

	if !quorumHealthy {
		logger.Error(nil, "Sentinel quorum not healthy, aborting failover handling",
			"quorumStatus", quorumMsg)
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "FailoverAborted",
			fmt.Sprintf("Sentinel quorum not healthy: %s", quorumMsg))
		return false
	}

	logger.Info("Quorum verified healthy, proceeding with failover handling", "quorumStatus", quorumMsg)

	// A different answer than the one that triggered this handling means
	// another failover completed (or is running) since detection. The cluster
	// is Sentinel's to converge; reconfiguring anyone from the stale snapshot
	// could demote the legitimate master.
	currentMaster, err := r.getCurrentMaster(ctx, rs, adminPassword, tlsCfg)
	if err != nil {
		logger.Error(err, "Cannot re-verify the master before reconfiguration, aborting failover handling")
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "FailoverAborted",
			"Cannot re-verify the current master with Sentinel, failover handling aborted for safety")
		return false
	}
	if currentMaster != newMaster {
		logger.Info("Master changed again since detection, abandoning reconfiguration",
			"detected", newMaster, "current", currentMaster)
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "FailoverSuperseded",
			fmt.Sprintf("Master moved from %s to %s during handling; deferring to Sentinel", newMaster, currentMaster))
		return false
	}

	// Parse new master address (format: IP:port)
	newMasterIP, newMasterPort := parseHostPort(newMaster)
	if newMasterIP == "" {
		logger.Error(nil, "Failed to parse new master address", "address", newMaster)
		return false
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
				oldClient := factoryOrDefault(r.RedisFactory).NewClient(oldMasterAddr, adminPassword, tlsCfg)
				defer oldClient.Close()

				err := oldClient.SlaveOf(ctx, newMasterIP, newMasterPort)
				if err != nil {
					logger.Error(err, "Failed to reconfigure old master as replica", "oldMaster", oldMaster)
				} else {
					logger.Info("Reconfigured old master as replica of new master", "oldMaster", oldMaster, "newMaster", newMaster)
					recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "FailoverHandled",
						fmt.Sprintf("Reconfigured %s as replica of %s", oldMaster, newMaster))
				}
			}()
		}
	}

	// Ensure all other replicas are pointing to the new master
	if err := r.ensureReplicasFollowMaster(ctx, rs, newMasterIP, newMasterPort, adminPassword, tlsCfg); err != nil {
		logger.Error(err, "Failed to ensure all replicas follow new master")
		return false
	}
	return true
}

// pruneOrphanedACLUsers removes, from every enumerable Redis node, the ACL
// accounts the operator itself created whose RedisUser no longer exists. The
// aclfile lives on the retained data volume, so deleting a cluster and
// recreating it under the same name restores every account it ever saved --
// including one whose RedisUser was revoked while the cluster was gone, which
// otherwise stays live under its old password with no CR referencing it.
//
// Only accounts named in the ownership record are candidates: an account an
// administrator created by hand carries no record and is never touched, and
// validateUsername refuses 'default' here exactly as it refuses it in a spec.
// The pass fails closed. An unreadable record or RedisUser list prunes
// nothing -- an unreadable list is not an empty one -- and a node that could
// not be enumerated keeps every record entry, so its copy of the account is
// removed when the node returns instead of being forgotten while it still
// holds it.
func (r *RedisSentinelReconciler) pruneOrphanedACLUsers(ctx context.Context, rs *redisv1alpha1.RedisSentinel, adminPassword string, tlsCfg *tls.Config) {
	logger := log.FromContext(ctx)

	owners := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: aclOwnersName(rs.Name), Namespace: rs.Namespace}, owners); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Cannot read the ACL ownership record, skipping the prune pass")
		}
		// No record: the operator never created an account on this cluster.
		return
	}
	if len(owners.Data) == 0 {
		return
	}

	addrs, err := r.runningPodAddresses(ctx, rs, "redis", 6379)
	if err != nil || len(addrs) == 0 {
		return
	}

	factory := factoryOrDefault(r.RedisFactory)
	clients := make(map[string]redisclient.Client, len(addrs))
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	// Enumerated before the RedisUsers are listed: an account reaches a node
	// only after its CR was written, so every enumerated account's CR, if it
	// exists, is visible to the List below and cannot be taken for an orphan.
	held, unreached := enumerateACLAccounts(ctx, factory, clients, addrs, adminPassword, tlsCfg)

	users := &redisv1alpha1.RedisUserList{}
	if err := r.List(ctx, users, client.InNamespace(rs.Namespace)); err != nil {
		logger.Error(err, "Cannot list RedisUsers, skipping the prune pass")
		return
	}
	declared := make(map[string]struct{}, len(users.Items))
	for i := range users.Items {
		if users.Items[i].Spec.RedisClusterRef == rs.Name {
			declared[users.Items[i].Spec.Username] = struct{}{}
		}
	}

	orphans := make([]string, 0, len(owners.Data))
	for username := range owners.Data {
		// validateUsername refuses 'default' and anything the operator would
		// never have written, so a hand-edited record cannot aim a DELUSER at
		// the admin account.
		if validateUsername(username) != nil {
			continue
		}
		if _, ok := declared[username]; ok {
			continue
		}
		orphans = append(orphans, username)
	}
	if len(orphans) == 0 {
		return
	}
	slices.Sort(orphans)

	pruned, failures := deleteACLAccounts(ctx, clients, addrs, held, orphans)

	for _, username := range orphans {
		nodes := pruned[username]
		if len(nodes) == 0 {
			continue
		}
		logger.Info("Pruned an orphaned ACL account",
			"username", username, "owner", owners.Data[username], "nodes", nodes)
		recordEvent(r.Recorder, rs, corev1.EventTypeNormal, "ACLUserPruned", fmt.Sprintf(
			"Removed ACL account %q from %s: created through RedisUser %q, which no longer exists",
			username, strings.Join(nodes, ", "), owners.Data[username]))
	}

	// Coverage below the spec'd size means a node that exists, or should, was
	// not cleaned; above it -- a held scale-down -- is extra coverage, and the
	// doomed ordinal's aclfile is cleaned before its volume is retained.
	complete := len(unreached) == 0 && len(failures) == 0 &&
		int32(len(addrs)) >= rs.Spec.RedisConfig.Replicas
	if !complete {
		detail := strings.Join(append(append([]string{}, unreached...), failures...), "; ")
		if detail == "" {
			detail = fmt.Sprintf("%d of %d Redis pods running", len(addrs), rs.Spec.RedisConfig.Replicas)
		}
		recordEvent(r.Recorder, rs, corev1.EventTypeWarning, "ACLPruneIncomplete", fmt.Sprintf(
			"Orphaned ACL account(s) %s could not be verified removed from every node (%s); "+
				"the ownership records are kept and the prune is retried",
			strings.Join(orphans, ", "), detail))
		return
	}

	// Every node was enumerated and every removal persisted, so the entries
	// describe nothing any more. Kept longer, they would mark a future
	// hand-made account of the same name for deletion.
	if err := removeACLOwners(ctx, r.Client, rs.Namespace, rs.Name, orphans); err != nil {
		logger.Error(err, "Failed to drop pruned accounts from the ownership record")
	}
}

// enumerateACLAccounts lists the accounts each node holds, dialling through the
// shared client map so the caller closes them. A node that cannot be reached is
// reported rather than treated as holding nothing: an unreadable node is not an
// empty one, and pruning on that assumption would forget an account the node
// still has.
func enumerateACLAccounts(ctx context.Context, factory redisclient.Factory, clients map[string]redisclient.Client,
	addrs []string, adminPassword string, tlsCfg *tls.Config) (map[string]map[string]struct{}, []string) {
	held := make(map[string]map[string]struct{}, len(addrs))
	unreached := make([]string, 0, len(addrs))

	for _, addr := range addrs {
		c := factory.NewClient(addr, adminPassword, tlsCfg)
		clients[addr] = c
		names, err := c.ACLUsers(ctx)
		if err != nil {
			unreached = append(unreached, fmt.Sprintf("%s: %v", addr, err))
			continue
		}
		set := make(map[string]struct{}, len(names))
		for _, name := range names {
			set[name] = struct{}{}
		}
		held[addr] = set
	}
	return held, unreached
}

// deleteACLAccounts removes the named accounts from the nodes that hold them and
// persists each node's aclfile, without which the next restart restores what was
// just deleted. It reports which nodes each account left, and every failure, so
// the caller can tell a complete prune from a partial one.
func deleteACLAccounts(ctx context.Context, clients map[string]redisclient.Client, addrs []string,
	held map[string]map[string]struct{}, orphans []string) (map[string][]string, []string) {
	pruned := map[string][]string{}
	failures := make([]string, 0, len(addrs))

	for _, addr := range addrs {
		names, enumerated := held[addr]
		if !enumerated {
			continue
		}
		removed := false
		for _, username := range orphans {
			if _, ok := names[username]; !ok {
				continue
			}
			if err := clients[addr].ACLDelUser(ctx, username); err != nil {
				failures = append(failures, fmt.Sprintf("%s on %s: %v", username, addr, err))
				continue
			}
			pruned[username] = append(pruned[username], addr)
			removed = true
		}
		if removed {
			if err := clients[addr].ACLSave(ctx); err != nil {
				failures = append(failures, fmt.Sprintf("persisting removals on %s: %v", addr, err))
			}
		}
	}
	return pruned, failures
}

// repointStrayReplicas sends SLAVEOF to every replica whose replication source
// is not the master Sentinel currently reports. Repointing a replica cannot
// lose data: it already holds no authoritative copy and resyncs from the master
// either way. Pods claiming to be master are left alone; deciding those is the
// failover path's job, and demoting one here on a stale view is how acknowledged
// writes get lost.
func (r *RedisSentinelReconciler) repointStrayReplicas(ctx context.Context, rs *redisv1alpha1.RedisSentinel, masterAddr, adminPassword string, tlsCfg *tls.Config) {
	logger := log.FromContext(ctx)

	masterIP, masterPort := parseHostPort(masterAddr)
	if masterIP == "" {
		return
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/component": "redis",
		"app.kubernetes.io/instance":  rs.Name,
	}); err != nil {
		logger.Error(err, "Failed to list Redis pods")
		return
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.Status.PodIP == masterIP {
			continue
		}

		func(podIP, podName string) {
			redisClient := factoryOrDefault(r.RedisFactory).NewClient(fmt.Sprintf("%s:6379", podIP), adminPassword, tlsCfg)
			defer redisClient.Close()

			info, err := redisClient.GetReplicationInfo(ctx)
			if err != nil {
				// A pod that cannot be dialled is reported through the
				// readiness conditions; there is nothing to repair here.
				return
			}
			if info["role"] != "slave" {
				return
			}
			if info["master_host"] == masterIP && info["master_port"] == masterPort {
				return
			}

			logger.Info("Repointing replica at the current master",
				"pod", podName,
				"followed", fmt.Sprintf("%s:%s", info["master_host"], info["master_port"]),
				"master", masterAddr)
			if err := redisClient.SlaveOf(ctx, masterIP, masterPort); err != nil {
				logger.Error(err, "Failed to repoint replica", "pod", podName)
			}
		}(pod.Status.PodIP, pod.Name)
	}
}

// ensureReplicasFollowMaster verifies and corrects replication configuration for all replicas
func (r *RedisSentinelReconciler) ensureReplicasFollowMaster(ctx context.Context, rs *redisv1alpha1.RedisSentinel, masterIP, masterPort, adminPassword string, tlsCfg *tls.Config) error {
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
			redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
			defer redisClient.Close()

			info, err := redisClient.GetReplicationInfo(ctx)
			if err != nil {
				logger.Error(err, "Failed to get replication info", "pod", podName)
				return
			}

			// Check if this is a replica pointing to the correct master
			switch info["role"] {
			case "master":
				// This pod thinks it's a master but shouldn't be - reconfigure it
				logger.Info("Found unexpected master, reconfiguring as replica", "pod", podName)
				if err := redisClient.SlaveOf(ctx, masterIP, masterPort); err != nil {
					logger.Error(err, "Failed to reconfigure pod as replica", "pod", podName)
				}
			case "slave":
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
func (r *RedisSentinelReconciler) checkSentinelQuorum(ctx context.Context, rs *redisv1alpha1.RedisSentinel, password string, tlsCfg *tls.Config) (bool, string, error) {
	masterName := rs.Name + "-master"

	// Use pool to check quorum from any available sentinel
	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(sentinelAddresses(rs), password, tlsCfg)
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

// createOrUpdate creates desired or converges the live object towards it.
// Every attempt re-reads the live object, so a lost optimistic-concurrency
// race against another writer of the same object is retried here instead of
// failing the whole reconcile pass.
func (r *RedisSentinelReconciler) createOrUpdate(ctx context.Context, desired client.Object) error {
	key := client.ObjectKeyFromObject(desired)

	return retry.OnError(retry.DefaultRetry, isRaceError, func() error {
		existing := desired.DeepCopyObject().(client.Object)
		if err := r.Get(ctx, key, existing); err != nil {
			if errors.IsNotFound(err) {
				// A fresh copy per attempt: Create stamps resourceVersion and
				// UID onto the object it is given, which a retry would reject.
				return r.Create(ctx, desired.DeepCopyObject().(client.Object))
			}
			return err
		}

		merged, err := mergeForUpdate(existing, desired)
		if err != nil {
			return err
		}
		return r.Update(ctx, merged)
	})
}

// isRaceError reports whether err is another writer winning: a stale
// resourceVersion, or the object appearing between the Get and the Create.
func isRaceError(err error) bool {
	return errors.IsConflict(err) || errors.IsAlreadyExists(err)
}

// mergeForUpdate applies the desired state onto the live object, restricted to
// the fields the API server allows an update to change. A StatefulSet rejects
// any spec change outside replicas, ordinals, template, updateStrategy,
// persistentVolumeClaimRetentionPolicy and minReadySeconds, so PUTting a full
// desired spec whose volumeClaimTemplates or selector drifted from what is
// running wedges every later reconcile of that CR, status included.
func mergeForUpdate(existing, desired client.Object) (client.Object, error) {
	merged := existing.DeepCopyObject().(client.Object)
	merged.SetLabels(desired.GetLabels())
	merged.SetAnnotations(desired.GetAnnotations())
	merged.SetOwnerReferences(desired.GetOwnerReferences())

	switch m := merged.(type) {
	case *appsv1.StatefulSet:
		d := desired.(*appsv1.StatefulSet)
		m.Spec.Replicas = d.Spec.Replicas
		m.Spec.Ordinals = d.Spec.Ordinals
		m.Spec.Template = d.Spec.Template
		m.Spec.UpdateStrategy = d.Spec.UpdateStrategy
		m.Spec.MinReadySeconds = d.Spec.MinReadySeconds
		m.Spec.PersistentVolumeClaimRetentionPolicy = d.Spec.PersistentVolumeClaimRetentionPolicy
	case *corev1.Service:
		d := desired.(*corev1.Service)
		clusterIP := m.Spec.ClusterIP
		m.Spec = d.Spec
		// Allocated by the API server on create and immutable afterwards.
		m.Spec.ClusterIP = clusterIP
	case *corev1.ConfigMap:
		d := desired.(*corev1.ConfigMap)
		m.Data = d.Data
		m.BinaryData = d.BinaryData
	case *networkingv1.NetworkPolicy:
		d := desired.(*networkingv1.NetworkPolicy)
		m.Spec = d.Spec
	case *policyv1.PodDisruptionBudget:
		d := desired.(*policyv1.PodDisruptionBudget)
		m.Spec = d.Spec
	default:
		return nil, fmt.Errorf("createOrUpdate does not know how to update %T", desired)
	}

	return merged, nil
}

func (r *RedisSentinelReconciler) updateMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel) {
	logger := log.FromContext(ctx)

	namespace := rs.Namespace
	name := rs.Name

	// Update cluster info metric
	clusterUp := 0.0
	if rs.Status.Phase == phaseRunning {
		clusterUp = 1.0
	}
	custmetrics.RedisClusterInfo.WithLabelValues(namespace, name).Set(clusterUp)

	// Instantiated here so alerts on increase() have a series to evaluate
	// before this cluster has ever failed over.
	custmetrics.RedisFailoverTotal.WithLabelValues(namespace, name).Add(0)

	// Resolved once for every connection this collection pass makes. Metrics
	// are best-effort, so a TLS resolution failure skips collection instead of
	// failing the reconcile.
	adminPassword := r.getAdminPassword(ctx, rs)
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, r.Client, rs)
	if err != nil {
		logger.Error(err, "Cannot resolve client TLS config, skipping Redis metrics collection")
		return
	}

	// Update sentinel status using real quorum check
	sentinelHealthy := 0.0
	quorumOK, message, err := r.checkSentinelQuorum(ctx, rs, adminPassword, tlsCfg)
	if err == nil && quorumOK {
		sentinelHealthy = 1.0
	}
	custmetrics.SentinelStatus.WithLabelValues(namespace, name).Set(sentinelHealthy)
	custmetrics.SentinelQuorumHealth.WithLabelValues(namespace, name).Set(sentinelHealthy)

	// Get sentinel master info for additional metrics
	r.updateSentinelMetrics(ctx, rs, adminPassword, tlsCfg)

	// List Redis pods with correct labels
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":  name,
		"app.kubernetes.io/component": "redis",
	}); err != nil {
		logger.Error(err, "Failed to list Redis pods for metrics")
		return
	}

	// Collect metrics from each Redis pod
	collected := make([]string, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		r.collectPodMetrics(ctx, rs, &pod, adminPassword, tlsCfg)
		collected = append(collected, pod.Name)
	}

	// A scaled-down or deleted pod would otherwise keep exporting the last
	// sample taken from it for as long as the operator runs.
	custmetrics.SyncClusterPods(namespace, name, collected)

	logger.V(1).Info("Metrics updated", "quorumHealthy", quorumOK, "quorumMessage", message)
}

// updateSentinelMetrics collects metrics from Sentinel
func (r *RedisSentinelReconciler) updateSentinelMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel, adminPassword string, tlsCfg *tls.Config) {
	logger := log.FromContext(ctx)
	masterName := rs.Name + "-master"

	pool := factoryOrDefault(r.RedisFactory).NewSentinelPool(sentinelAddresses(rs), adminPassword, tlsCfg)
	masterInfo, err := pool.GetMasterFromPool(ctx, masterName)
	if err != nil {
		logger.V(1).Info("Failed to get master info from sentinel", "error", err)
		// The operator monitors exactly one master per cluster, so this is a
		// 0/1 answer to "does Sentinel still know it".
		custmetrics.SentinelMonitoredMasters.WithLabelValues(rs.Namespace, rs.Name).Set(0)
		return
	}
	custmetrics.SentinelMonitoredMasters.WithLabelValues(rs.Namespace, rs.Name).Set(1)

	// Parse sentinel counts
	if masterInfo.NumOtherSentinels != "" {
		custmetrics.SentinelKnownSentinels.WithLabelValues(rs.Namespace, rs.Name).
			Set(infoFloat(masterInfo.NumOtherSentinels))
	}

	if masterInfo.NumSlaves != "" {
		custmetrics.SentinelKnownReplicas.WithLabelValues(rs.Namespace, rs.Name).
			Set(infoFloat(masterInfo.NumSlaves))
	}
}

// collectPodMetrics collects all metrics from a single Redis pod
func (r *RedisSentinelReconciler) collectPodMetrics(ctx context.Context, rs *redisv1alpha1.RedisSentinel, pod *corev1.Pod, adminPassword string, tlsCfg *tls.Config) {
	logger := log.FromContext(ctx)
	namespace := rs.Namespace
	name := rs.Name
	podName := pod.Name

	addr := fmt.Sprintf("%s:6379", pod.Status.PodIP)
	redisClient := factoryOrDefault(r.RedisFactory).NewClient(addr, adminPassword, tlsCfg)
	defer redisClient.Close()

	// --- Memory Metrics ---
	if memInfo, err := redisClient.GetMemoryInfo(ctx); err == nil {
		setInfoGauge(custmetrics.RedisUsedMemory, memInfo, "used_memory", namespace, name, podName)
		setInfoGauge(custmetrics.RedisMaxMemory, memInfo, "maxmemory", namespace, name, podName)
		setInfoGauge(custmetrics.RedisMemoryFragmentationRatio, memInfo, "mem_fragmentation_ratio",
			namespace, name, podName)
	}

	// --- Client Metrics ---
	if clientInfo, err := redisClient.GetClientInfo(ctx); err == nil {
		setInfoGauge(custmetrics.RedisConnectedClients, clientInfo, "connected_clients",
			namespace, name, podName)
		setInfoGauge(custmetrics.RedisBlockedClients, clientInfo, "blocked_clients",
			namespace, name, podName)
	}

	// --- Persistence Metrics ---
	if persistInfo, err := redisClient.GetPersistenceInfo(ctx); err == nil {
		setInfoGauge(custmetrics.RedisRDBLastSaveTime, persistInfo, "rdb_last_save_time",
			namespace, name, podName)
		setInfoGauge(custmetrics.RedisRDBChangesSinceLastSave, persistInfo, "rdb_changes_since_last_save",
			namespace, name, podName)
		if aofEnabled, ok := persistInfo["aof_enabled"]; ok {
			val := 0.0
			if aofEnabled == "1" {
				val = 1.0
			}
			custmetrics.RedisAOFEnabled.WithLabelValues(namespace, name, podName).Set(val)
		}
		setInfoGauge(custmetrics.RedisAOFCurrentSize, persistInfo, "aof_current_size",
			namespace, name, podName)
	}

	r.collectReplicationMetrics(ctx, redisClient, namespace, name, podName)

	// --- Stats Metrics ---
	// INFO reports these as absolute values that restart at zero with the
	// node, so each one is advanced by its increase since the last pass.
	if statsInfo, err := redisClient.GetStatsInfo(ctx); err == nil {
		addInfoCounterDelta(custmetrics.RedisTotalCommandsProcessed, statsInfo, "total_commands_processed",
			namespace, name, podName)
		addInfoCounterDelta(custmetrics.RedisKeyspaceHits, statsInfo, "keyspace_hits",
			namespace, name, podName)
		addInfoCounterDelta(custmetrics.RedisKeyspaceMisses, statsInfo, "keyspace_misses",
			namespace, name, podName)
		addInfoCounterDelta(custmetrics.RedisRejectedConnections, statsInfo, "rejected_connections",
			namespace, name, podName)
	}

	// --- Keyspace Metrics ---
	if keyspaceInfo, err := redisClient.GetKeyspaceInfo(ctx); err == nil {
		for db, info := range keyspaceInfo {
			if !strings.HasPrefix(db, "db") {
				continue
			}
			// Parse "keys=123,expires=45,avg_ttl=6789"
			for _, part := range strings.Split(info, ",") {
				if !strings.HasPrefix(part, "keys=") {
					continue
				}
				var keys float64
				_, _ = fmt.Sscanf(part, "keys=%f", &keys)
				custmetrics.RedisDBKeys.WithLabelValues(namespace, name, podName, db).Set(keys)
			}
		}
	}

	logger.V(2).Info("Collected metrics from pod", "pod", podName)
}

// collectReplicationMetrics publishes what one pod reports about replication.
// A master and a replica answer different questions, so the two halves share
// only the offset gauge and are labelled apart on it.
func (r *RedisSentinelReconciler) collectReplicationMetrics(ctx context.Context, redisClient redisclient.Client,
	namespace, name, podName string) {
	replInfo, err := redisClient.GetReplicationInfo(ctx)
	if err != nil {
		return
	}

	if replInfo["role"] == "master" {
		setInfoGauge(custmetrics.RedisReplicationOffset, replInfo, "master_repl_offset",
			namespace, name, podName, "master")
		// Read off the master rather than derived from ready pod count:
		// a pod can be ready and still not be replicating.
		setInfoGauge(custmetrics.RedisConnectedReplicas, replInfo, "connected_slaves", namespace, name)
		return
	}

	setInfoGauge(custmetrics.RedisReplicationOffset, replInfo, "slave_repl_offset",
		namespace, name, podName, "replica")

	linkStatus := 0.0
	if replInfo["master_link_status"] == "up" {
		linkStatus = 1.0
	}
	custmetrics.RedisMasterLinkStatus.WithLabelValues(namespace, name, podName).Set(linkStatus)

	if lagBytes, err := redisClient.GetReplicationLagByOffset(ctx); err == nil {
		custmetrics.RedisReplicationLagBytes.WithLabelValues(namespace, name, podName).Set(float64(lagBytes))
	}

	// -1 marks a replica whose lag could not be read, which is not the same
	// answer as a replica that is caught up.
	lag, err := redisClient.GetReplicationLag(ctx)
	if err != nil {
		custmetrics.RedisReplicationLag.WithLabelValues(namespace, name, podName).Set(-1)
		return
	}
	custmetrics.RedisReplicationLag.WithLabelValues(namespace, name, podName).Set(float64(lag))
}

// sentinelsForReferencedSecret maps a Secret back to the RedisSentinels that
// reference it as their auth Secret or as one of their TLS secrets. These
// Secrets are created by the user and can be shared, so no owner reference
// points from them to a CR and Owns() cannot see them; without this mapping a
// rotated password or a renewed certificate sits in the Secret while every pod
// keeps serving what it read at startup.
func (r *RedisSentinelReconciler) sentinelsForReferencedSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &redisv1alpha1.RedisSentinelList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list RedisSentinels for a Secret change",
			"secret", obj.GetName(), "namespace", obj.GetNamespace())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		rs := &list.Items[i]
		if !referencesSecret(rs, obj.GetName()) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace},
		})
	}
	return requests
}

// referencesSecret reports whether name is the CR's auth Secret or one of its
// TLS secrets.
func referencesSecret(rs *redisv1alpha1.RedisSentinel, name string) bool {
	if auth := rs.Spec.RedisConfig.Auth; auth != nil && auth.SecretName == name {
		return true
	}
	if tlsSpec := rs.Spec.TLS; tlsSpec != nil && tlsSpec.Enabled {
		if tlsSpec.CertificateSecretRef == name || tlsSpec.CASecretRef == name {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisSentinelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RedisFactory == nil {
		r.RedisFactory = redisclient.DefaultFactory{}
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("redissentinel-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1alpha1.RedisSentinel{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.sentinelsForReferencedSecret)).
		Named("redissentinel").
		Complete(r)
}
