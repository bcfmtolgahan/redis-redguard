// Package redisclient is the seam between the controllers and the Redis
// protocol clients. Controllers depend on these interfaces and construct
// clients through a Factory, so tests can substitute the in-memory fake and
// TLS wiring stays a one-line change per call site.
package redisclient

import (
	"context"
	"crypto/tls"

	"github.com/redguard/redguard/internal/sentinel"
	"github.com/redguard/redguard/pkg/redisutils"
)

// Client is the exact method surface the controllers use from
// *redisutils.RedisClient. Do not add methods no controller calls.
type Client interface {
	Ping(ctx context.Context) error
	IsMaster(ctx context.Context) (bool, error)
	GetReplicationInfo(ctx context.Context) (map[string]string, error)
	GetReplicationLag(ctx context.Context) (int64, error)
	GetReplicationLagByOffset(ctx context.Context) (int64, error)
	GetMemoryInfo(ctx context.Context) (map[string]string, error)
	GetClientInfo(ctx context.Context) (map[string]string, error)
	GetPersistenceInfo(ctx context.Context) (map[string]string, error)
	GetStatsInfo(ctx context.Context) (map[string]string, error)
	GetKeyspaceInfo(ctx context.Context) (map[string]string, error)
	SlaveOf(ctx context.Context, masterHost, masterPort string) error
	ACLSetUser(ctx context.Context, username string, rules ...string) error
	ACLDelUser(ctx context.Context, username string) error
	BGSave(ctx context.Context) error
	LastSave(ctx context.Context) (int64, error)
	ConfigGet(ctx context.Context, parameter string) (map[string]string, error)
	ConfigSet(ctx context.Context, parameter, value string) error
	ShutdownNoSave(ctx context.Context) error
	DBSize(ctx context.Context) (int64, error)
	Close() error
}

// Sentinel is the exact method surface the controllers use from
// *sentinel.SentinelClientPool.
type Sentinel interface {
	GetMasterAddrFromPool(ctx context.Context, masterName string) (string, error)
	GetMasterFromPool(ctx context.Context, masterName string) (*sentinel.MasterInfo, error)
	CheckQuorumFromPool(ctx context.Context, masterName string) (bool, int, error)
	SetMasterOptionAll(ctx context.Context, masterName, option, value string) error
}

// Factory constructs protocol clients. A nil tlsConfig selects the plaintext
// constructors; a non-nil one selects the TLS constructors.
type Factory interface {
	NewClient(addr, password string, tlsConfig *tls.Config) Client
	NewSentinelPool(addrs []string, password string, tlsConfig *tls.Config) Sentinel
}

// DefaultFactory builds real network clients over the concrete types.
type DefaultFactory struct{}

func (DefaultFactory) NewClient(addr, password string, tlsConfig *tls.Config) Client {
	if tlsConfig != nil {
		return redisutils.NewRedisClientWithTLS(addr, password, tlsConfig)
	}
	return redisutils.NewRedisClient(addr, password)
}

func (DefaultFactory) NewSentinelPool(addrs []string, password string, tlsConfig *tls.Config) Sentinel {
	if tlsConfig != nil {
		return sentinel.NewSentinelClientPoolWithTLS(addrs, password, tlsConfig)
	}
	return sentinel.NewSentinelClientPool(addrs, password)
}

// The concrete-type assertions live here, not in the implementation packages:
// this package must import both of them to build DefaultFactory, so asserting
// there would create an import cycle.
var (
	_ Client   = (*redisutils.RedisClient)(nil)
	_ Sentinel = (*sentinel.SentinelClientPool)(nil)
	_ Factory  = DefaultFactory{}
)
