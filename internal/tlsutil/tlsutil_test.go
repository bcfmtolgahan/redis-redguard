package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// selfSignedPEM returns a freshly generated self-signed certificate and its
// key, both PEM-encoded. Good enough for both the client pair and the CA.
func selfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redis-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func newFakeClient(t *testing.T, secrets ...*corev1.Secret) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types to scheme: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, s := range secrets {
		b = b.WithObjects(s)
	}
	return b.Build()
}

func secretWith(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"},
		Data:       data,
	}
}

func sentinelWithTLS(tlsSpec *redisv1alpha1.TLSConfig) *redisv1alpha1.RedisSentinel {
	return &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns1"},
		Spec:       redisv1alpha1.RedisSentinelSpec{TLS: tlsSpec},
	}
}

func TestBuildClientTLSConfig_DisabledReturnsNil(t *testing.T) {
	c := newFakeClient(t)

	for name, tlsSpec := range map[string]*redisv1alpha1.TLSConfig{
		"nil spec":      nil,
		"enabled false": {Enabled: false, CertificateSecretRef: "redis-tls"},
	} {
		cfg, err := BuildClientTLSConfig(context.Background(), c, sentinelWithTLS(tlsSpec))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if cfg != nil {
			t.Errorf("%s: config = %+v, want nil when TLS is disabled", name, cfg)
		}
	}
}

func TestBuildClientTLSConfig_LoadsCertAndCA(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	caPEM, _ := selfSignedPEM(t)
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}),
		secretWith("redis-ca", map[string][]byte{"ca.crt": caPEM}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-ca",
	})

	cfg, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("config is nil although TLS is enabled")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates has %d entries, want 1", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil although a CA secret is referenced")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set; server verification must stay on")
	}
}

// Without a CA reference the config must leave RootCAs nil so crypto/tls
// falls back to the system trust store, never to InsecureSkipVerify.
func TestBuildClientTLSConfig_NoCAUsesSystemPool(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
	})

	cfg, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("config is nil although TLS is enabled")
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs is set although no CA secret is referenced")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set; server verification must stay on")
	}
}

func TestBuildClientTLSConfig_SharedSecretForCertAndCA(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  certPEM,
		}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-tls",
	})

	cfg, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("config is nil although TLS is enabled")
	}
	if len(cfg.Certificates) != 1 || cfg.RootCAs == nil {
		t.Errorf("Certificates = %d entries, RootCAs = %v; want 1 entry and a pool", len(cfg.Certificates), cfg.RootCAs)
	}
}

func TestBuildClientTLSConfig_MissingSecretFails(t *testing.T) {
	c := newFakeClient(t)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "no-such-secret",
	})

	_, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err == nil {
		t.Fatal("no error although the certificate secret does not exist; a silent plaintext fallback would follow")
	}
	if !strings.Contains(err.Error(), "no-such-secret") {
		t.Errorf("error %q does not name the missing secret", err)
	}
}

func TestBuildClientTLSConfig_MissingKeyFails(t *testing.T) {
	certPEM, _ := selfSignedPEM(t)
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{"tls.crt": certPEM}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
	})

	_, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err == nil {
		t.Fatal("no error although the secret has no tls.key")
	}
	if !strings.Contains(err.Error(), "tls.key") {
		t.Errorf("error %q does not name the missing key", err)
	}
}

func TestBuildClientTLSConfig_MissingCAKeyFails(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}),
		secretWith("redis-ca", map[string][]byte{"wrong-key": certPEM}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-ca",
	})

	_, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err == nil {
		t.Fatal("no error although the CA secret has no ca.crt")
	}
	if !strings.Contains(err.Error(), "ca.crt") {
		t.Errorf("error %q does not name the missing key", err)
	}
}

func TestBuildClientTLSConfig_EmptyCertRefFails(t *testing.T) {
	c := newFakeClient(t)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{Enabled: true})

	_, err := BuildClientTLSConfig(context.Background(), c, rs)
	if err == nil {
		t.Fatal("no error although TLS is enabled with no certificateSecretRef")
	}
}

func TestBuildClientTLSConfig_GarbageKeyPairFails(t *testing.T) {
	c := newFakeClient(t,
		secretWith("redis-tls", map[string][]byte{
			"tls.crt": []byte("not a certificate"),
			"tls.key": []byte("not a key"),
		}),
	)
	rs := sentinelWithTLS(&redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
	})

	if _, err := BuildClientTLSConfig(context.Background(), c, rs); err == nil {
		t.Fatal("no error although the key pair does not parse")
	}
}
