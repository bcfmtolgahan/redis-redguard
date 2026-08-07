package builder

import (
	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// BuildRedisService creates a service for Redis external access
func BuildRedisService(rs *redisv1alpha1.RedisSentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis",
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
