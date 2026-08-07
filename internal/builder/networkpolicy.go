package builder

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// BuildRedisNetworkPolicy creates NetworkPolicy for Redis pods
func BuildRedisNetworkPolicy(rs *redisv1alpha1.RedisSentinel) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis-netpol",
			Namespace: rs.Namespace,
			Labels: map[string]string{
				"app":                          "redis",
				"redis.redguard.io/redis-name": rs.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                          "redis",
					"redis.redguard.io/redis-name": rs.Name,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Allow from same namespace pods
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(6379),
						},
					},
				},
				{
					// Allow from Sentinel pods
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "sentinel",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(6379),
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow DNS
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": "kube-system",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(53),
						},
						{
							Protocol: protocolPtr(corev1.ProtocolUDP),
							Port:     intPtr(53),
						},
					},
				},
				{
					// Allow to other Redis pods (replication)
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "redis",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(6379),
						},
					},
				},
				{
					// Allow to Sentinel
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "sentinel",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(26379),
						},
					},
				},
			},
		},
	}
}

// BuildSentinelNetworkPolicy creates NetworkPolicy for Sentinel pods
func BuildSentinelNetworkPolicy(rs *redisv1alpha1.RedisSentinel) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel-netpol",
			Namespace: rs.Namespace,
			Labels: map[string]string{
				"app":                          "sentinel",
				"redis.redguard.io/redis-name": rs.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                          "sentinel",
					"redis.redguard.io/redis-name": rs.Name,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Allow from same namespace (operator needs to query)
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(26379),
						},
					},
				},
				{
					// Allow from other Sentinels
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "sentinel",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(26379),
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow DNS
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": "kube-system",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(53),
						},
						{
							Protocol: protocolPtr(corev1.ProtocolUDP),
							Port:     intPtr(53),
						},
					},
				},
				{
					// Allow to Redis pods (monitoring)
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "redis",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(6379),
						},
					},
				},
				{
					// Allow to other Sentinels
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app":                          "sentinel",
									"redis.redguard.io/redis-name": rs.Name,
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port:     intPtr(26379),
						},
					},
				},
			},
		},
	}
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func intPtr(i int) *intstr.IntOrString {
	intOrStr := intstr.FromInt(i)
	return &intOrStr
}
