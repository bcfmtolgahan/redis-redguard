// Package sentinelwatch feeds the RedisSentinel controller a reconcile
// trigger the moment a sentinel announces a promotion, instead of leaving the
// role label to the periodic pass.
//
// The subscription is a trigger, never a source of truth. An event carries
// only which cluster to reconcile; the reconcile then resolves the master
// with Sentinel exactly as a periodic pass does. A publication that arrives
// late, duplicated, out of order or forged by something that reached the
// sentinel port can therefore cost at most one wasted reconcile -- it cannot
// move a label or issue a SLAVEOF, which is what re-acting on a stale master
// view once did to acknowledged writes. The periodic requeue stays as the
// path that converges the cluster when the subscription is down or a message
// is missed.
//
// Only +switch-master is subscribed. It is the one event that marks a
// completed promotion, published by every sentinel as it adopts the new
// configuration. The failure-detection chatter around it (+sdown, +odown,
// +failover-state-*) fires while there is nothing for the controller to act
// on yet, and waking on it would turn an unstable cluster into a reconcile
// hot loop.
package sentinelwatch

import (
	"context"
	"crypto/tls"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
	"github.com/bcfmtolgahan/redis-redguard/internal/clusteraccess"
	"github.com/bcfmtolgahan/redis-redguard/internal/sentinel"
	"github.com/bcfmtolgahan/redis-redguard/internal/tlsutil"
)

const (
	// defaultResync paces how quickly the subscriber set follows created and
	// deleted RedisSentinels. It reads the informer cache, not the API server,
	// so following closely is cheap.
	defaultResync = 10 * time.Second

	// Reconnect backoff per sentinel connection. The cap bounds how hard an
	// unreachable sentinel is hammered; the polling reconcile covers the gap.
	defaultBackoffBase = time.Second
	defaultBackoffCap  = 30 * time.Second
)

// subscribeFunc is the seam the tests replace: dial one sentinel and stream
// the published master names to onEvent until the connection fails or ctx
// ends.
type subscribeFunc func(ctx context.Context, addr, password string, tlsCfg *tls.Config, onEvent func(masterName string)) error

// Watcher subscribes to the +switch-master channel of every monitored
// cluster's sentinels. One connection per sentinel, not one per cluster: the
// node that takes the master down often hosts a sentinel too, and a single
// subscription pointed at that sentinel would miss the very promotion it
// exists to catch. Duplicate publications from the other members collapse in
// the controller's workqueue.
type Watcher struct {
	client    client.Client
	subscribe subscribeFunc

	resync      time.Duration
	backoffBase time.Duration
	backoffCap  time.Duration

	events chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel]

	wg       sync.WaitGroup
	mu       sync.Mutex
	clusters map[types.NamespacedName]*clusterSubs
}

type clusterSubs struct {
	cancel context.CancelFunc
	// fingerprint is the joined address list; a spec change that reshapes the
	// sentinel set restarts the cluster's subscriptions against the new set.
	fingerprint string
}

// New builds a Watcher over the manager's client.
func New(c client.Client) *Watcher {
	return &Watcher{
		client:      c,
		subscribe:   sentinel.SubscribeSwitchMaster,
		resync:      defaultResync,
		backoffBase: defaultBackoffBase,
		backoffCap:  defaultBackoffCap,
		clusters:    map[types.NamespacedName]*clusterSubs{},
		// Sized for bursts only: the send never blocks, and a drop is safe
		// because the periodic reconcile still converges the cluster.
		events: make(chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel], 64),
	}
}

// Events is the channel SetupWithManager wires as a source.Channel. It is
// never closed; both ends stop with the manager context.
func (w *Watcher) Events() <-chan event.TypedGenericEvent[*redisv1alpha1.RedisSentinel] {
	return w.events
}

// NeedLeaderElection defers the subscriptions to the elected leader: only the
// leader runs the reconcilers, so a non-leader's events would go nowhere and
// its connections would be pure load on the sentinels.
func (w *Watcher) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable: it follows the RedisSentinel set until
// ctx ends, then stops every subscription and waits for the goroutines. Added
// to the manager rather than owned by the controller so its lifetime is
// exactly the manager's: it starts after the caches sync and leadership is
// won, and the manager's shutdown is what tears it down.
func (w *Watcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.resync)
	defer ticker.Stop()

	for {
		w.sync(ctx)
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for key, cs := range w.clusters {
				cs.cancel()
				delete(w.clusters, key)
			}
			w.mu.Unlock()
			w.wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

// sync diffs the running subscriptions against the RedisSentinels that exist,
// starting and cancelling per-cluster subscription sets as clusters come, go
// and reshape.
func (w *Watcher) sync(ctx context.Context) {
	list := &redisv1alpha1.RedisSentinelList{}
	if err := w.client.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "Cannot list RedisSentinels; keeping the current sentinel subscriptions")
		return
	}

	desired := map[types.NamespacedName]string{}
	for i := range list.Items {
		rs := &list.Items[i]
		if !rs.DeletionTimestamp.IsZero() {
			continue
		}
		addrs := clusteraccess.SentinelAddresses(rs)
		if len(addrs) == 0 {
			continue
		}
		desired[types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}] = strings.Join(addrs, ",")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for key, cs := range w.clusters {
		if fp, ok := desired[key]; !ok || fp != cs.fingerprint {
			cs.cancel()
			delete(w.clusters, key)
		}
	}

	for key, fp := range desired {
		if _, ok := w.clusters[key]; ok {
			continue
		}
		subCtx, cancel := context.WithCancel(ctx)
		w.clusters[key] = &clusterSubs{cancel: cancel, fingerprint: fp}
		for _, addr := range strings.Split(fp, ",") {
			w.wg.Add(1)
			go w.run(subCtx, key, addr)
		}
	}
}

// run keeps one sentinel subscribed for one cluster, reconnecting with capped
// exponential backoff so an unreachable sentinel costs a bounded dial rate
// and nothing else.
func (w *Watcher) run(ctx context.Context, key types.NamespacedName, addr string) {
	defer w.wg.Done()

	backoff := w.backoffBase
	for {
		started := time.Now()
		err := w.attempt(ctx, key, addr)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.FromContext(ctx).V(1).Info("Sentinel subscription ended, reconnecting",
				"cluster", key.String(), "sentinel", addr, "after", backoff.String(), "error", err.Error())
		}
		// A connection that lived past the cap was healthy; its failure
		// starts a fresh backoff instead of continuing an old one.
		if time.Since(started) > w.backoffCap {
			backoff = w.backoffBase
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, w.backoffCap)
	}
}

// attempt resolves the connection parameters fresh -- the password may have
// rotated and the certificates may have been renewed since the last dial --
// and holds one subscription until it fails or ctx ends.
func (w *Watcher) attempt(ctx context.Context, key types.NamespacedName, addr string) error {
	rs := &redisv1alpha1.RedisSentinel{}
	if err := w.client.Get(ctx, key, rs); err != nil {
		// Deleted between syncs; the next sync cancels this goroutine.
		return err
	}

	password := clusteraccess.AdminPassword(ctx, w.client, rs)
	// A TLS resolution failure aborts the attempt: falling back to plaintext
	// against TLS-only sentinels would hand the password to whoever answers.
	tlsCfg, err := tlsutil.BuildClientTLSConfig(ctx, w.client, rs)
	if err != nil {
		return err
	}

	masterName := clusteraccess.MasterName(rs)
	ref := &redisv1alpha1.RedisSentinel{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}
	return w.subscribe(ctx, addr, password, tlsCfg, func(published string) {
		// Only this cluster's master wakes its reconcile. The name is the
		// entire trust surface: addresses in the payload are never parsed,
		// so a forged publication can only cause a reconcile that re-asks
		// Sentinel and finds nothing to do.
		if published != masterName {
			return
		}
		select {
		case w.events <- event.TypedGenericEvent[*redisv1alpha1.RedisSentinel]{Object: ref}:
		default:
			// Full buffer means a reconcile flood is already queued; the
			// periodic pass covers whatever this drop misses.
		}
	})
}
