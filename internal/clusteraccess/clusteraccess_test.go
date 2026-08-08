package clusteraccess

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add redis types: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func sentinelWithAuth(secretName string) *redisv1alpha1.RedisSentinel {
	rs := &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns1"},
	}
	if secretName != "" {
		rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: secretName}
	}
	return rs
}

func secret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"},
		Data:       data,
	}
}

func TestSentinelAddresses(t *testing.T) {
	rs := &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns1"},
	}
	rs.Spec.SentinelConfig.Replicas = 2

	got := SentinelAddresses(rs)
	want := []string{
		"demo-sentinel-0.demo-sentinel-headless.ns1.svc.cluster.local:26379",
		"demo-sentinel-1.demo-sentinel-headless.ns1.svc.cluster.local:26379",
	}
	if len(got) != len(want) {
		t.Fatalf("SentinelAddresses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SentinelAddresses[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMasterName(t *testing.T) {
	rs := sentinelWithAuth("")
	if got := MasterName(rs); got != "demo-master" {
		t.Errorf("MasterName = %q, want %q", got, "demo-master")
	}
}

func TestAdminPasswordWithoutAuthConfigured(t *testing.T) {
	rs := sentinelWithAuth("")
	if got := AdminPassword(context.Background(), testClient(t), rs); got != "" {
		t.Errorf("AdminPassword = %q, want empty when the spec configures no auth", got)
	}
}

// The applied-credentials Secret must win over the user Secret: during a
// rotation the user Secret already holds the password no node accepts yet.
func TestAdminPasswordPrefersAppliedCredentials(t *testing.T) {
	rs := sentinelWithAuth("user-auth")
	c := testClient(t,
		secret("user-auth", map[string][]byte{"password": []byte("rotated-to")}),
		secret(AppliedAuthSecretName(rs), map[string][]byte{"password": []byte("still-accepted")}),
	)
	if got := AdminPassword(context.Background(), c, rs); got != "still-accepted" {
		t.Errorf("AdminPassword = %q, want the applied password %q", got, "still-accepted")
	}
}

func TestAdminPasswordFallsBackToUserSecret(t *testing.T) {
	rs := sentinelWithAuth("user-auth")
	c := testClient(t, secret("user-auth", map[string][]byte{"password": []byte("first-boot")}))
	if got := AdminPassword(context.Background(), c, rs); got != "first-boot" {
		t.Errorf("AdminPassword = %q, want %q", got, "first-boot")
	}
}

func TestAdminPasswordWithMissingSecrets(t *testing.T) {
	rs := sentinelWithAuth("user-auth")
	if got := AdminPassword(context.Background(), testClient(t), rs); got != "" {
		t.Errorf("AdminPassword = %q, want empty when no Secret exists", got)
	}
}
