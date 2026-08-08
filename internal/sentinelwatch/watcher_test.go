package sentinelwatch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	"github.com/bcfmtolgahan/redis-redguard/internal/clusteraccess"
)

type subCall struct {
	addr     string
	password string
	tls      bool
}

// fakeSubscriber stands in for the sentinel protocol connection. behave, when
// set, scripts the lifetime of one connection; the default blocks until the
// watcher cancels it, which is what a healthy idle subscription does.
type fakeSubscriber struct {
	mu     sync.Mutex
	calls  []subCall
	active int
	behave func(ctx context.Context, addr string, onEvent func(string)) error
}

func (f *fakeSubscriber) fn(ctx context.Context, addr, password string, tlsCfg *tls.Config, onEvent func(string)) error {
	f.mu.Lock()
	f.calls = append(f.calls, subCall{addr: addr, password: password, tls: tlsCfg != nil})
	f.active++
	behave := f.behave
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if behave != nil {
		return behave(ctx, addr, onEvent)
	}
	<-ctx.Done()
	return nil
}

func (f *fakeSubscriber) snapshot() []subCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]subCall(nil), f.calls...)
}

func (f *fakeSubscriber) activeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add redis types: %v", err)
	}
	return scheme
}

// startWatcher builds a watcher over the given objects with test-sized
// timings, starts it, and stops it on test cleanup.
func startWatcher(t *testing.T, sub *fakeSubscriber, objs ...client.Object) (*Watcher, client.Client, context.CancelFunc) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	w := New(c)
	w.subscribe = sub.fn
	w.resync = 20 * time.Millisecond
	w.backoffBase = 25 * time.Millisecond
	w.backoffCap = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Start(ctx); err != nil {
			t.Errorf("watcher Start returned %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("watcher Start did not return after its context was cancelled")
		}
	})
	return w, c, cancel
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testCluster(name, ns string, sentinels int32) *redisv1alpha1.RedisSentinel {
	rs := &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	rs.Spec.SentinelConfig.Replicas = sentinels
	return rs
}

func TestWatcherSubscribesEverySentinelWithTheAppliedPassword(t *testing.T) {
	rs := testCluster("demo", "ns1", 2)
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "user-auth"}
	sub := &fakeSubscriber{}

	startWatcher(t, sub,
		rs,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "user-auth", Namespace: "ns1"},
			Data:       map[string][]byte{"password": []byte("rotated-to")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: clusteraccess.AppliedAuthSecretName(rs), Namespace: "ns1"},
			Data:       map[string][]byte{"password": []byte("still-accepted")},
		},
	)

	want := clusteraccess.SentinelAddresses(rs)
	waitFor(t, 5*time.Second, func() bool {
		seen := map[string]bool{}
		for _, call := range sub.snapshot() {
			if call.password == "still-accepted" {
				seen[call.addr] = true
			}
		}
		for _, addr := range want {
			if !seen[addr] {
				return false
			}
		}
		return true
	}, "not every sentinel got a subscription presenting the applied password")
}

func TestWatcherEmitsOneEventPerPublication(t *testing.T) {
	rs := testCluster("demo", "ns1", 2)
	sub := &fakeSubscriber{}
	sub.behave = func(ctx context.Context, addr string, onEvent func(string)) error {
		if strings.Contains(addr, "-sentinel-0.") {
			onEvent(clusteraccess.MasterName(rs))
		}
		<-ctx.Done()
		return nil
	}

	w, _, _ := startWatcher(t, sub, rs)

	select {
	case evt := <-w.Events():
		if evt.Object.GetName() != "demo" || evt.Object.GetNamespace() != "ns1" {
			t.Fatalf("event for %s/%s, want ns1/demo", evt.Object.GetNamespace(), evt.Object.GetName())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event for a +switch-master publication naming this cluster's master")
	}

	select {
	case evt := <-w.Events():
		t.Fatalf("second event %v for a single publication", evt.Object.GetName())
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatcherIgnoresPublicationsForForeignMasters(t *testing.T) {
	rs := testCluster("demo", "ns1", 1)
	sub := &fakeSubscriber{}
	sub.behave = func(ctx context.Context, addr string, onEvent func(string)) error {
		onEvent("someone-else-master")
		<-ctx.Done()
		return nil
	}

	w, _, _ := startWatcher(t, sub, rs)

	select {
	case evt := <-w.Events():
		t.Fatalf("event %v for a publication naming a master this cluster does not own", evt.Object.GetName())
	case <-time.After(400 * time.Millisecond):
	}
}

func TestWatcherRedialsAFailedConnectionWithBackoffNotASpin(t *testing.T) {
	rs := testCluster("demo", "ns1", 1)
	sub := &fakeSubscriber{}
	sub.behave = func(ctx context.Context, addr string, onEvent func(string)) error {
		return errors.New("connection refused")
	}

	startWatcher(t, sub, rs)

	waitFor(t, 5*time.Second, func() bool { return len(sub.snapshot()) >= 3 },
		"a failed connection was never redialled")

	// base 25ms doubling to a 100ms cap: ~10 attempts fit in 600ms. A loop
	// that ignores the backoff makes tens of thousands.
	time.Sleep(600 * time.Millisecond)
	if calls := len(sub.snapshot()); calls > 20 {
		t.Fatalf("%d dial attempts in ~600ms; the reconnect loop is spinning", calls)
	}
}

func TestWatcherStopsTheSubscribersOfADeletedCluster(t *testing.T) {
	rs := testCluster("demo", "ns1", 2)
	sub := &fakeSubscriber{}

	_, c, _ := startWatcher(t, sub, rs)

	waitFor(t, 5*time.Second, func() bool { return sub.activeCount() == 2 },
		"subscriptions never started")

	if err := c.Delete(context.Background(), rs); err != nil {
		t.Fatalf("delete RedisSentinel: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return sub.activeCount() == 0 },
		"the deleted cluster's subscribers were not cancelled")

	settled := len(sub.snapshot())
	time.Sleep(200 * time.Millisecond)
	if calls := len(sub.snapshot()); calls != settled {
		t.Fatalf("%d new dial attempts after the cluster was deleted", calls-settled)
	}
}

func TestWatcherNeverDialsPlaintextWhenTLSResolutionFails(t *testing.T) {
	rs := testCluster("demo", "ns1", 1)
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "absent"}
	sub := &fakeSubscriber{}

	startWatcher(t, sub, rs)

	time.Sleep(400 * time.Millisecond)
	if calls := sub.snapshot(); len(calls) != 0 {
		t.Fatalf("subscribed %v although the TLS config could not be resolved; that connection would be plaintext against TLS-only pods", calls)
	}
}

func TestWatcherThreadsTheResolvedTLSConfig(t *testing.T) {
	rs := testCluster("demo", "ns1", 1)
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "tls-cert"}
	certPEM, keyPEM := selfSignedPEM(t)
	sub := &fakeSubscriber{}

	startWatcher(t, sub, rs, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-cert", Namespace: "ns1"},
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	})

	waitFor(t, 5*time.Second, func() bool {
		calls := sub.snapshot()
		return len(calls) > 0 && calls[0].tls
	}, "the subscription was not handed the TLS config the spec requires")
}

func TestWatcherRunsOnlyOnTheLeader(t *testing.T) {
	w := New(fake.NewClientBuilder().WithScheme(testScheme(t)).Build())
	if !w.NeedLeaderElection() {
		t.Fatal("the watcher must wait for leadership: only the leader reconciles, so a non-leader's events go nowhere and its connections are pure load")
	}
}

// selfSignedPEM returns a freshly generated self-signed certificate and key.
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
