package builder

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// TestNetworkPolicySelectorMatchesPodLabels is the load-bearing NetworkPolicy
// test: a policy whose podSelector matches no pod is silently inert, so the
// pods it was meant to protect stay wide open. The same applies to every
// pod-selecting peer in an ingress/egress rule — a peer nothing matches allows
// nothing.
func TestNetworkPolicySelectorMatchesPodLabels(t *testing.T) {
	rs := testSentinel()
	redisPods := BuildRedisStatefulSet(rs).Spec.Template.Labels
	sentinelPods := BuildSentinelStatefulSet(rs).Spec.Template.Labels

	tests := []struct {
		name      string
		netpol    *networkingv1.NetworkPolicy
		podLabels map[string]string
	}{
		{"redis", BuildRedisNetworkPolicy(rs), redisPods},
		{"sentinel", BuildSentinelNetworkPolicy(rs), sentinelPods},
	}

	// The policy must apply to the pods it is named for.
	for _, tc := range tests {
		if len(tc.netpol.Spec.PodSelector.MatchLabels) == 0 {
			t.Errorf("%s: netpol podSelector is empty; it would apply to every pod in the namespace", tc.name)
		}
		for k, v := range tc.netpol.Spec.PodSelector.MatchLabels {
			if tc.podLabels[k] != v {
				t.Errorf("%s: netpol selects %s=%s but pods carry %v — policy matches zero pods",
					tc.name, k, v, tc.podLabels)
			}
		}
	}

	// Every pod-selecting peer must resolve to the redis or the sentinel pods.
	checkPeer := func(policy string, sel map[string]string) {
		if len(sel) == 0 {
			return // empty selector == "all pods in this namespace", intentional
		}
		if subsetOf(sel, redisPods) || subsetOf(sel, sentinelPods) {
			return
		}
		t.Errorf("%s: peer selector %v matches neither redis pods %v nor sentinel pods %v",
			policy, sel, redisPods, sentinelPods)
	}

	for _, tc := range tests {
		for _, rule := range tc.netpol.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil {
					checkPeer(tc.name, peer.PodSelector.MatchLabels)
				}
			}
		}
		for _, rule := range tc.netpol.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector != nil {
					checkPeer(tc.name, peer.PodSelector.MatchLabels)
				}
			}
		}
	}
}

func TestBuildRedisNetworkPolicy_Metadata(t *testing.T) {
	rs := testSentinel()
	np := BuildRedisNetworkPolicy(rs)

	if got, want := np.Name, "test-rs-redis-netpol"; got != want {
		t.Errorf("NetworkPolicy name = %q, want %q", got, want)
	}
	if got, want := np.Namespace, "default"; got != want {
		t.Errorf("NetworkPolicy namespace = %q, want %q", got, want)
	}
	assertPolicyTypes(t, np)
}

func TestBuildSentinelNetworkPolicy_Metadata(t *testing.T) {
	rs := testSentinel()
	np := BuildSentinelNetworkPolicy(rs)

	if got, want := np.Name, "test-rs-sentinel-netpol"; got != want {
		t.Errorf("NetworkPolicy name = %q, want %q", got, want)
	}
	assertPolicyTypes(t, np)
}

func TestBuildRedisNetworkPolicy_IngressPorts(t *testing.T) {
	np := BuildRedisNetworkPolicy(testSentinel())

	if len(np.Spec.Ingress) == 0 {
		t.Fatal("redis NetworkPolicy has no ingress rules; ingress is denied entirely")
	}
	for i, rule := range np.Spec.Ingress {
		if !allowsPort(rule.Ports, corev1.ProtocolTCP, 6379) {
			t.Errorf("ingress rule %d does not allow TCP/6379: %v", i, rule.Ports)
		}
	}
}

func TestBuildSentinelNetworkPolicy_IngressPorts(t *testing.T) {
	np := BuildSentinelNetworkPolicy(testSentinel())

	if len(np.Spec.Ingress) == 0 {
		t.Fatal("sentinel NetworkPolicy has no ingress rules; ingress is denied entirely")
	}
	for i, rule := range np.Spec.Ingress {
		if !allowsPort(rule.Ports, corev1.ProtocolTCP, 26379) {
			t.Errorf("ingress rule %d does not allow TCP/26379: %v", i, rule.Ports)
		}
	}
}

// Without DNS egress the init scripts cannot resolve the headless Service and
// the whole cluster fails to bootstrap.
func TestNetworkPolicies_AllowDNSEgress(t *testing.T) {
	rs := testSentinel()

	for name, np := range map[string]*networkingv1.NetworkPolicy{
		"redis":    BuildRedisNetworkPolicy(rs),
		"sentinel": BuildSentinelNetworkPolicy(rs),
	} {
		var foundTCP, foundUDP bool
		for _, rule := range np.Spec.Egress {
			if allowsPort(rule.Ports, corev1.ProtocolTCP, 53) {
				foundTCP = true
			}
			if allowsPort(rule.Ports, corev1.ProtocolUDP, 53) {
				foundUDP = true
			}
		}
		if !foundTCP {
			t.Errorf("%s: no egress rule allows TCP/53", name)
		}
		if !foundUDP {
			t.Errorf("%s: no egress rule allows UDP/53; DNS resolution will fail", name)
		}
	}
}

func TestNetworkPolicies_EgressReachesPeerComponents(t *testing.T) {
	rs := testSentinel()

	// Redis must reach other Redis pods (replication) and the Sentinels.
	redis := BuildRedisNetworkPolicy(rs)
	if !hasEgressPort(redis, 6379) {
		t.Error("redis netpol has no egress to TCP/6379; replication would be blocked")
	}
	if !hasEgressPort(redis, 26379) {
		t.Error("redis netpol has no egress to TCP/26379; init.sh could not query Sentinel")
	}

	// Sentinel must reach the Redis instances it monitors and its peers.
	sentinel := BuildSentinelNetworkPolicy(rs)
	if !hasEgressPort(sentinel, 6379) {
		t.Error("sentinel netpol has no egress to TCP/6379; monitoring would be blocked")
	}
	if !hasEgressPort(sentinel, 26379) {
		t.Error("sentinel netpol has no egress to TCP/26379; sentinel quorum would be blocked")
	}
}

func TestNetworkPolicies_NamespaceAndNameFollowCR(t *testing.T) {
	rs := testSentinel()
	rs.Name = "prod-cache"
	rs.Namespace = "redis-prod"

	for _, np := range []*networkingv1.NetworkPolicy{
		BuildRedisNetworkPolicy(rs),
		BuildSentinelNetworkPolicy(rs),
	} {
		if np.Namespace != "redis-prod" {
			t.Errorf("%s namespace = %q, want %q", np.Name, np.Namespace, "redis-prod")
		}
		if np.Labels["app.kubernetes.io/instance"] != "prod-cache" {
			t.Errorf("%s: instance label = %q, want %q",
				np.Name, np.Labels["app.kubernetes.io/instance"], "prod-cache")
		}
	}
}

// The operator dials Redis and Sentinel from its own namespace, which is not
// the namespace the cluster runs in. Without this rule the policies lock the
// operator out of every cluster it manages.
func TestNetworkPolicies_AdmitOperatorNamespace(t *testing.T) {
	rs := testSentinel()

	for _, tc := range []struct {
		name string
		np   *networkingv1.NetworkPolicy
		port int32
	}{
		{"redis", BuildRedisNetworkPolicy(rs), 6379},
		{"sentinel", BuildSentinelNetworkPolicy(rs), 26379},
	} {
		if !admitsNamespace(tc.np, "redguard-system", tc.port) {
			t.Errorf("%s: no ingress rule admits namespace %q on TCP/%d; the operator cannot reach the cluster",
				tc.name, "redguard-system", tc.port)
		}
	}
}

// The operator is not pinned to redguard-system, so the admitted namespace has
// to follow the namespace it actually runs in.
func TestNetworkPolicies_OperatorNamespaceFollowsEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "redguard-ops")
	rs := testSentinel()

	for _, tc := range []struct {
		name string
		np   *networkingv1.NetworkPolicy
		port int32
	}{
		{"redis", BuildRedisNetworkPolicy(rs), 6379},
		{"sentinel", BuildSentinelNetworkPolicy(rs), 26379},
	} {
		if !admitsNamespace(tc.np, "redguard-ops", tc.port) {
			t.Errorf("%s: ingress does not admit the operator namespace from the environment", tc.name)
		}
		if admitsNamespace(tc.np, "redguard-system", tc.port) {
			t.Errorf("%s: ingress still admits the default operator namespace", tc.name)
		}
	}
}

// Clients live beside the cluster and have no label the operator can predict,
// so the workload namespace stays open on the client port. Everything else is
// denied, which is only true if no peer selects all namespaces.
func TestNetworkPolicies_AdmitLocalNamespaceOnly(t *testing.T) {
	rs := testSentinel()

	for _, tc := range []struct {
		name string
		np   *networkingv1.NetworkPolicy
		port int32
	}{
		{"redis", BuildRedisNetworkPolicy(rs), 6379},
		{"sentinel", BuildSentinelNetworkPolicy(rs), 26379},
	} {
		var local bool
		for _, rule := range tc.np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil &&
					len(peer.NamespaceSelector.MatchLabels) == 0 &&
					len(peer.NamespaceSelector.MatchExpressions) == 0 {
					t.Errorf("%s: an ingress peer selects every namespace in the cluster", tc.name)
				}
				if peer.NamespaceSelector == nil && peer.PodSelector != nil &&
					len(peer.PodSelector.MatchLabels) == 0 &&
					allowsPort(rule.Ports, corev1.ProtocolTCP, tc.port) {
					local = true
				}
			}
		}
		if !local {
			t.Errorf("%s: no ingress rule admits same-namespace clients on TCP/%d", tc.name, tc.port)
		}
	}
}

// In-cluster the service account projection is the only source that knows the
// namespace, so it has to be consulted before the built-in default.
func TestOperatorNamespaceResolution(t *testing.T) {
	projected := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(projected, []byte("redguard-prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		env  string
		path string
		want string
	}{
		{"env wins", "redguard-ops", projected, "redguard-ops"},
		{"projected namespace", "", projected, "redguard-prod"},
		{"no source", "", filepath.Join(t.TempDir(), "absent"), "redguard-system"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POD_NAMESPACE", tc.env)
			orig := serviceAccountNamespacePath
			serviceAccountNamespacePath = tc.path
			defer func() { serviceAccountNamespacePath = orig }()

			if got := operatorNamespace(); got != tc.want {
				t.Errorf("operatorNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertPolicyTypes(t *testing.T, np *networkingv1.NetworkPolicy) {
	t.Helper()

	var ingress, egress bool
	for _, pt := range np.Spec.PolicyTypes {
		switch pt {
		case networkingv1.PolicyTypeIngress:
			ingress = true
		case networkingv1.PolicyTypeEgress:
			egress = true
		}
	}
	if !ingress || !egress {
		t.Errorf("%s: policyTypes = %v, want both Ingress and Egress", np.Name, np.Spec.PolicyTypes)
	}
}

func allowsPort(ports []networkingv1.NetworkPolicyPort, proto corev1.Protocol, port int32) bool {
	for _, p := range ports {
		if p.Protocol == nil || *p.Protocol != proto {
			continue
		}
		if p.Port != nil && int32(p.Port.IntValue()) == port {
			return true
		}
	}
	return false
}

func hasEgressPort(np *networkingv1.NetworkPolicy, port int32) bool {
	for _, rule := range np.Spec.Egress {
		if allowsPort(rule.Ports, corev1.ProtocolTCP, port) {
			return true
		}
	}
	return false
}

// admitsNamespace reports whether np has an ingress rule opening port to every
// pod of namespace ns. A peer that also carries a podSelector grants less than
// that, so it does not count.
func admitsNamespace(np *networkingv1.NetworkPolicy, ns string, port int32) bool {
	for _, rule := range np.Spec.Ingress {
		if !allowsPort(rule.Ports, corev1.ProtocolTCP, port) {
			continue
		}
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil || peer.PodSelector != nil {
				continue
			}
			if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == ns {
				return true
			}
		}
	}
	return false
}

func subsetOf(sub, super map[string]string) bool {
	for k, v := range sub {
		if super[k] != v {
			return false
		}
	}
	return true
}
