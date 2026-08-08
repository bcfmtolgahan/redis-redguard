package builder

import (
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

const (
	redisPort    = 6379
	sentinelPort = 26379

	// operatorNamespaceEnv overrides the namespace the policies admit; the
	// chart can inject it from metadata.namespace via fieldRef.
	operatorNamespaceEnv = "POD_NAMESPACE"

	// defaultOperatorNamespace is the chart's install namespace, used only when
	// the running namespace cannot be determined.
	defaultOperatorNamespace = "redguard-system"
)

// serviceAccountNamespacePath is the in-cluster projection of the running pod's
// namespace. A variable so tests can point it elsewhere.
var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// BuildRedisNetworkPolicy creates the NetworkPolicy for Redis pods.
//
// Ingress is open to the whole workload namespace on the client port: clients
// carry no label the operator can predict, and Sentinel and the replicas live
// there too. Other namespaces are denied except the one the operator runs in.
func BuildRedisNetworkPolicy(rs *redisv1alpha1.RedisSentinel) *networkingv1.NetworkPolicy {
	labels := buildLabels(rs, "redis")

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis-netpol",
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{localNamespacePeer()},
					Ports: tcpPorts(redisPort),
				},
				{
					From:  []networkingv1.NetworkPolicyPeer{componentPeer(rs, "sentinel")},
					Ports: tcpPorts(redisPort),
				},
				{
					From:  []networkingv1.NetworkPolicyPeer{operatorNamespacePeer()},
					Ports: tcpPorts(redisPort),
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{
					// Replication.
					To:    []networkingv1.NetworkPolicyPeer{componentPeer(rs, "redis")},
					Ports: tcpPorts(redisPort),
				},
				{
					// init.sh asks Sentinel who the current master is.
					To:    []networkingv1.NetworkPolicyPeer{componentPeer(rs, "sentinel")},
					Ports: tcpPorts(sentinelPort),
				},
			},
		},
	}
}

// BuildSentinelNetworkPolicy creates the NetworkPolicy for Sentinel pods. Same
// posture as the Redis policy, on the Sentinel port.
func BuildSentinelNetworkPolicy(rs *redisv1alpha1.RedisSentinel) *networkingv1.NetworkPolicy {
	labels := buildLabels(rs, "sentinel")

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel-netpol",
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Sentinel-aware clients discover the master here.
					From:  []networkingv1.NetworkPolicyPeer{localNamespacePeer()},
					Ports: tcpPorts(sentinelPort),
				},
				{
					// Quorum gossip.
					From:  []networkingv1.NetworkPolicyPeer{componentPeer(rs, "sentinel")},
					Ports: tcpPorts(sentinelPort),
				},
				{
					From:  []networkingv1.NetworkPolicyPeer{operatorNamespacePeer()},
					Ports: tcpPorts(sentinelPort),
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{
					// Monitoring the masters and replicas.
					To:    []networkingv1.NetworkPolicyPeer{componentPeer(rs, "redis")},
					Ports: tcpPorts(redisPort),
				},
				{
					To:    []networkingv1.NetworkPolicyPeer{componentPeer(rs, "sentinel")},
					Ports: tcpPorts(sentinelPort),
				},
			},
		},
	}
}

// componentPeer selects one component of one cluster. The selector must be the
// pod template's own labels: a peer that matches no pod allows nothing.
func componentPeer(rs *redisv1alpha1.RedisSentinel, component string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: buildLabels(rs, component)},
	}
}

// localNamespacePeer selects every pod in the policy's own namespace.
func localNamespacePeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
}

// operatorNamespacePeer selects the namespace the operator runs in, which is
// never the workload namespace. Without it the operator cannot reach the
// clusters it manages and every status field goes stale.
func operatorNamespacePeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{corev1.LabelMetadataName: operatorNamespace()},
		},
	}
}

// dnsEgressRule allows resolution against kube-dns. Without it the init scripts
// cannot resolve the headless Service and the cluster never bootstraps.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{corev1.LabelMetadataName: metav1.NamespaceSystem},
				},
			},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intPtr(53)},
			{Protocol: protocolPtr(corev1.ProtocolUDP), Port: intPtr(53)},
		},
	}
}

// operatorNamespace resolves the namespace the operator itself runs in. The
// environment wins so a non-standard deployment can pin it; otherwise the
// service account projection every pod carries is authoritative.
func operatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv(operatorNamespaceEnv)); ns != "" {
		return ns
	}
	if data, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return defaultOperatorNamespace
}

func tcpPorts(ports ...int) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, networkingv1.NetworkPolicyPort{
			Protocol: protocolPtr(corev1.ProtocolTCP),
			Port:     intPtr(p),
		})
	}
	return out
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func intPtr(i int) *intstr.IntOrString {
	intOrStr := intstr.FromInt(i)
	return &intOrStr
}
