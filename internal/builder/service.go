package builder

import (
	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// RoleLabelKey marks the pod Sentinel currently reports as master. The
// reconciler stamps it on the master and strips it from every other Redis
// pod; the client Service selects on it so writes never reach a replica.
// It is deliberately absent from the pod template: a template-borne value
// would reassert itself on restart regardless of the real role.
const (
	RoleLabelKey = "redis.redguard.io/role"
	RoleMaster   = "master"
)

// BuildRedisHeadlessService creates a headless service for Redis StatefulSet
func BuildRedisHeadlessService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis-headless",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "redis"),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None", // Headless
			Selector:  buildLabels(rs, "redis"),
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromInt(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			PublishNotReadyAddresses: true, // Important for StatefulSet discovery
		},
	}
}

// BuildRedisService creates the client-facing write endpoint. It selects only
// the pod carrying the master role label, so between a master failing and the
// reconciler moving the label the Service has no endpoints: writes fail fast
// instead of landing on a read-only replica. If the operator itself is down,
// the label cannot move and the Service keeps pointing at the last known
// master until the operator returns.
func BuildRedisService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	selector := buildLabels(rs, "redis")
	selector[RoleLabelKey] = RoleMaster
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "redis"),
		},
		Spec: corev1.ServiceSpec{
			Type:     rs.Spec.ServiceType,
			Selector: selector,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromInt(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// BuildRedisReplicasService creates a read-scaling endpoint selecting every
// Redis pod, master included. Connections may land on any node, so it is only
// suitable for read-only clients.
func BuildRedisReplicasService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis-replicas",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "redis"),
		},
		Spec: corev1.ServiceSpec{
			Type:     rs.Spec.ServiceType,
			Selector: buildLabels(rs, "redis"),
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromInt(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// BuildSentinelHeadlessService creates a headless service for Sentinel StatefulSet
func BuildSentinelHeadlessService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel-headless",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "sentinel"),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None", // Headless
			Selector:  buildLabels(rs, "sentinel"),
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       26379,
					TargetPort: intstr.FromInt(26379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			PublishNotReadyAddresses: true,
		},
	}
}

// BuildSentinelService creates a service for Sentinel external access
func BuildSentinelService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel",
			Namespace: rs.Namespace,
			Labels:    buildLabels(rs, "sentinel"),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP, // Always ClusterIP for Sentinel
			Selector: buildLabels(rs, "sentinel"),
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       26379,
					TargetPort: intstr.FromInt(26379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
