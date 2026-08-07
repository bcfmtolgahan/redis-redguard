package builder

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildRedisHeadlessService(t *testing.T) {
	rs := testSentinel()
	svc := BuildRedisHeadlessService(rs)

	if got, want := svc.Name, "test-rs-redis-headless"; got != want {
		t.Errorf("Service name = %q, want %q", got, want)
	}
	if got, want := svc.Namespace, "default"; got != want {
		t.Errorf("Service namespace = %q, want %q", got, want)
	}
	if got, want := svc.Spec.ClusterIP, corev1.ClusterIPNone; got != want {
		t.Errorf("clusterIP = %q, want %q (service must be headless for stable pod DNS)", got, want)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("publishNotReadyAddresses = false; pod DNS must resolve before readiness or bootstrap deadlocks")
	}
	if got, want := svc.Spec.Type, corev1.ServiceTypeClusterIP; got != want {
		t.Errorf("type = %q, want %q", got, want)
	}
	assertSinglePort(t, svc, "redis", 6379)
}

func TestBuildSentinelHeadlessService(t *testing.T) {
	rs := testSentinel()
	svc := BuildSentinelHeadlessService(rs)

	if got, want := svc.Name, "test-rs-sentinel-headless"; got != want {
		t.Errorf("Service name = %q, want %q", got, want)
	}
	if got, want := svc.Spec.ClusterIP, corev1.ClusterIPNone; got != want {
		t.Errorf("clusterIP = %q, want %q", got, want)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("publishNotReadyAddresses = false; sentinels must be discoverable before they are ready")
	}
	assertSinglePort(t, svc, "sentinel", 26379)
}

func TestBuildRedisService_HonoursServiceType(t *testing.T) {
	for _, typ := range []corev1.ServiceType{
		corev1.ServiceTypeClusterIP,
		corev1.ServiceTypeNodePort,
		corev1.ServiceTypeLoadBalancer,
	} {
		rs := testSentinel()
		rs.Spec.ServiceType = typ

		svc := BuildRedisService(rs)

		if svc.Spec.Type != typ {
			t.Errorf("serviceType %q: got %q", typ, svc.Spec.Type)
		}
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			t.Errorf("serviceType %q: client Service must not be headless", typ)
		}
		if svc.Spec.PublishNotReadyAddresses {
			t.Errorf("serviceType %q: client Service must not publish not-ready addresses", typ)
		}
	}
}

func TestBuildRedisService_Metadata(t *testing.T) {
	svc := BuildRedisService(testSentinel())

	if got, want := svc.Name, "test-rs-redis"; got != want {
		t.Errorf("Service name = %q, want %q", got, want)
	}
	assertSinglePort(t, svc, "redis", 6379)
}

// Sentinel is an internal control-plane endpoint and is never exposed, whatever
// serviceType the user asks for on the Redis client Service.
func TestBuildSentinelService_AlwaysClusterIP(t *testing.T) {
	rs := testSentinel()
	rs.Spec.ServiceType = corev1.ServiceTypeLoadBalancer

	svc := BuildSentinelService(rs)

	if got, want := svc.Name, "test-rs-sentinel"; got != want {
		t.Errorf("Service name = %q, want %q", got, want)
	}
	if got, want := svc.Spec.Type, corev1.ServiceTypeClusterIP; got != want {
		t.Errorf("type = %q, want %q even when spec.serviceType is LoadBalancer", got, want)
	}
	assertSinglePort(t, svc, "sentinel", 26379)
}

// Every Service selector must actually match the pods its StatefulSet creates,
// otherwise the Service has no endpoints.
func TestServiceSelectorsMatchStatefulSetPodLabels(t *testing.T) {
	rs := testSentinel()

	redisPodLabels := BuildRedisStatefulSet(rs).Spec.Template.Labels
	sentinelPodLabels := BuildSentinelStatefulSet(rs).Spec.Template.Labels

	tests := []struct {
		name      string
		svc       *corev1.Service
		podLabels map[string]string
	}{
		{"redis-headless", BuildRedisHeadlessService(rs), redisPodLabels},
		{"redis", BuildRedisService(rs), redisPodLabels},
		{"sentinel-headless", BuildSentinelHeadlessService(rs), sentinelPodLabels},
		{"sentinel", BuildSentinelService(rs), sentinelPodLabels},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.svc.Spec.Selector) == 0 {
				t.Fatal("Service has an empty selector; it would select every pod in the namespace")
			}
			for k, v := range tc.svc.Spec.Selector {
				if tc.podLabels[k] != v {
					t.Errorf("selector wants %s=%s but pods carry %v — Service gets zero endpoints",
						k, v, tc.podLabels)
				}
			}
		})
	}
}

// The FQDNs baked into the generated configs must correspond to real Services.
func TestServiceNamesMatchGeneratedConfigHostnames(t *testing.T) {
	rs := testSentinel()

	// buildRedisInitScript / BuildSentinelConfigMap derive hostnames of the form
	// <name>-redis-0.<name>-redis-headless.<ns>.svc.cluster.local, which requires
	// the headless Services to be named exactly this way.
	if got, want := BuildRedisHeadlessService(rs).Name, rs.Name+"-redis-headless"; got != want {
		t.Errorf("redis headless Service name = %q, want %q", got, want)
	}
	if got, want := BuildSentinelHeadlessService(rs).Name, rs.Name+"-sentinel-headless"; got != want {
		t.Errorf("sentinel headless Service name = %q, want %q", got, want)
	}
	if got, want := BuildRedisHeadlessService(rs).Name, BuildRedisStatefulSet(rs).Spec.ServiceName; got != want {
		t.Errorf("redis headless Service %q does not match StatefulSet serviceName %q", got, want)
	}
	if got, want := BuildSentinelHeadlessService(rs).Name, BuildSentinelStatefulSet(rs).Spec.ServiceName; got != want {
		t.Errorf("sentinel headless Service %q does not match StatefulSet serviceName %q", got, want)
	}
}

func TestServices_Labels(t *testing.T) {
	rs := testSentinel()

	tests := []struct {
		svc       *corev1.Service
		component string
	}{
		{BuildRedisHeadlessService(rs), "redis"},
		{BuildRedisService(rs), "redis"},
		{BuildSentinelHeadlessService(rs), "sentinel"},
		{BuildSentinelService(rs), "sentinel"},
	}

	for _, tc := range tests {
		want := buildLabels(rs, tc.component)
		for k, v := range want {
			if tc.svc.Labels[k] != v {
				t.Errorf("%s: label %s = %q, want %q", tc.svc.Name, k, tc.svc.Labels[k], v)
			}
		}
	}
}

func TestServices_NamespaceFollowsCR(t *testing.T) {
	rs := testSentinel()
	rs.Namespace = "redis-prod"

	for _, svc := range []*corev1.Service{
		BuildRedisHeadlessService(rs),
		BuildRedisService(rs),
		BuildSentinelHeadlessService(rs),
		BuildSentinelService(rs),
	} {
		if svc.Namespace != "redis-prod" {
			t.Errorf("%s namespace = %q, want %q", svc.Name, svc.Namespace, "redis-prod")
		}
	}
}

func assertSinglePort(t *testing.T, svc *corev1.Service, name string, port int32) {
	t.Helper()

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("%s: got %d ports, want 1: %v", svc.Name, len(svc.Spec.Ports), svc.Spec.Ports)
	}
	p := svc.Spec.Ports[0]
	if p.Name != name {
		t.Errorf("%s: port name = %q, want %q", svc.Name, p.Name, name)
	}
	if p.Port != port {
		t.Errorf("%s: port = %d, want %d", svc.Name, p.Port, port)
	}
	if p.TargetPort != intstr.FromInt(int(port)) {
		t.Errorf("%s: targetPort = %v, want %d", svc.Name, p.TargetPort, port)
	}
	if p.Protocol != corev1.ProtocolTCP {
		t.Errorf("%s: protocol = %q, want TCP", svc.Name, p.Protocol)
	}
}
