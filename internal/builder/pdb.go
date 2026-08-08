package builder

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// BuildRedisPodDisruptionBudget bounds voluntary disruption of the Redis pods.
// Without a budget a node drain evicts every pod it hosts at once, so draining
// one node of a co-located cluster ends the cluster; a Redis pod also holds the
// only writable copy of the data while it is master.
func BuildRedisPodDisruptionBudget(rs *redisv1alpha1.RedisSentinel) *policyv1.PodDisruptionBudget {
	return buildPodDisruptionBudget(rs, "redis")
}

// BuildSentinelPodDisruptionBudget bounds voluntary disruption of the Sentinel
// pods. Sentinel needs a majority of its own set both to declare a master down
// and to elect the sentinel that runs the failover, so losing two of three at
// once leaves a cluster that cannot fail over.
func BuildSentinelPodDisruptionBudget(rs *redisv1alpha1.RedisSentinel) *policyv1.PodDisruptionBudget {
	return buildPodDisruptionBudget(rs, "sentinel")
}

// buildPodDisruptionBudget allows one voluntary disruption at a time.
//
// maxUnavailable is fixed at 1 rather than derived from replicas and quorum.
// One is the largest budget that is safe for every legal topology: with three
// sentinels and a quorum of two, taking a second pod removes the quorum. It is
// also the smallest value that cannot wedge a drain outright -- a budget of 0
// blocks every eviction forever, which converts a routine node drain into an
// indefinite hang and protects nothing that admission does not already refuse.
func buildPodDisruptionBudget(rs *redisv1alpha1.RedisSentinel, component string) *policyv1.PodDisruptionBudget {
	labels := buildLabels(rs, component)
	maxUnavailable := intstr.FromInt32(1)
	// A pod that is not Ready serves no traffic and counts towards no quorum.
	// Under the default IfHealthyBudget it is still undisruptible while the
	// budget is exhausted, so one wedged pod blocks the drain indefinitely.
	evictUnhealthy := policyv1.AlwaysAllow

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-" + component + "-pdb",
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable:             &maxUnavailable,
			Selector:                   &metav1.LabelSelector{MatchLabels: labels},
			UnhealthyPodEvictionPolicy: &evictUnhealthy,
		},
	}
}
