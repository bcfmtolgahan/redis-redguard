// Package fake is an in-memory redisclient.Factory for controller tests. One
// node exists per dialed address and its state survives across NewClient
// calls, so a test can assert what a reconcile did to each pod.
package fake

import (
	"context"
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/redguard/redguard/internal/redisclient"
	"github.com/redguard/redguard/internal/sentinel"
)

var (
	_ redisclient.Factory  = (*Factory)(nil)
	_ redisclient.Client   = (*client)(nil)
	_ redisclient.Sentinel = (*pool)(nil)
)

// Factory hands out fake clients backed by shared in-memory state. Safe for
// concurrent use; every client method appends "<addr>:<method>" to one call
// log so tests can assert which pods were touched, in what order.
type Factory struct {
	mu         sync.Mutex
	nodes      map[string]*node
	masterAddr string
	users      map[string][]string
	calls      []string
}

// node is the state of one fake Redis instance, keyed by dial address.
type node struct {
	role       string
	masterHost string
	masterPort string
	users      map[string][]string
	dbSize     int64
	lastSave   int64
}

func NewFactory() *Factory {
	return &Factory{
		nodes: map[string]*node{},
		users: map[string][]string{},
	}
}

// SetMaster declares addr ("host:port", the address clients will dial) the
// current master. The sentinel pool reports it and every node's role is
// recomputed: the matching node becomes master, all others become replicas of
// it. With no master configured, nodes default to standalone masters and the
// sentinel pool errors.
func (f *Factory) SetMaster(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.masterAddr = addr
	for a, n := range f.nodes {
		f.syncRole(a, n)
	}
}

// Users returns the rules of the most recent ACLSetUser per username, across
// all nodes. Deleted users are absent.
func (f *Factory) Users() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]string, len(f.users))
	for u, rules := range f.users {
		out[u] = append([]string(nil), rules...)
	}
	return out
}

// Calls returns the ordered log of every client method invoked through this
// factory, formatted "<addr>:<method>". Sentinel pool entries use the
// comma-joined pool addresses.
func (f *Factory) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *Factory) NewClient(addr, password string, tlsConfig *tls.Config) redisclient.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.node(addr)
	return &client{f: f, addr: addr}
}

func (f *Factory) NewSentinelPool(addrs []string, password string, tlsConfig *tls.Config) redisclient.Sentinel {
	return &pool{f: f, addrs: append([]string(nil), addrs...)}
}

// node returns the state for addr, creating it on first dial. Caller holds f.mu.
func (f *Factory) node(addr string) *node {
	n, ok := f.nodes[addr]
	if !ok {
		n = &node{users: map[string][]string{}}
		f.nodes[addr] = n
		f.syncRole(addr, n)
	}
	return n
}

// syncRole derives a node's role from the configured master. Caller holds f.mu.
func (f *Factory) syncRole(addr string, n *node) {
	if f.masterAddr == "" || addr == f.masterAddr {
		n.role = "master"
		n.masterHost, n.masterPort = "", ""
		return
	}
	n.role = "slave"
	n.masterHost, n.masterPort = splitHostPort(f.masterAddr)
}

// record appends to the call log. Caller holds f.mu.
func (f *Factory) record(addr, method string) {
	f.calls = append(f.calls, addr+":"+method)
}

func splitHostPort(addr string) (string, string) {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, "6379"
}

// client implements redisclient.Client against one node of the factory.
type client struct {
	f    *Factory
	addr string
}

func (c *client) Ping(ctx context.Context) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "Ping")
	return nil
}

func (c *client) IsMaster(ctx context.Context) (bool, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "IsMaster")
	return c.f.node(c.addr).role == "master", nil
}

func (c *client) GetReplicationInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetReplicationInfo")
	n := c.f.node(c.addr)
	info := map[string]string{
		"role":               n.role,
		"master_repl_offset": "0",
	}
	if n.role == "slave" {
		info["master_host"] = n.masterHost
		info["master_port"] = n.masterPort
		info["master_link_status"] = "up"
		info["slave_repl_offset"] = "0"
	}
	return info, nil
}

func (c *client) GetReplicationLag(ctx context.Context) (int64, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetReplicationLag")
	return 0, nil
}

func (c *client) GetReplicationLagByOffset(ctx context.Context) (int64, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetReplicationLagByOffset")
	return 0, nil
}

func (c *client) GetMemoryInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetMemoryInfo")
	return map[string]string{}, nil
}

func (c *client) GetClientInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetClientInfo")
	return map[string]string{}, nil
}

// GetPersistenceInfo reports an idle, successful persistence state so backup
// wait loops complete immediately.
func (c *client) GetPersistenceInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetPersistenceInfo")
	return map[string]string{
		"rdb_bgsave_in_progress":      "0",
		"rdb_last_bgsave_status":      "ok",
		"rdb_changes_since_last_save": "0",
	}, nil
}

func (c *client) GetStatsInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetStatsInfo")
	return map[string]string{}, nil
}

func (c *client) GetKeyspaceInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetKeyspaceInfo")
	return map[string]string{}, nil
}

func (c *client) SlaveOf(ctx context.Context, masterHost, masterPort string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "SlaveOf")
	n := c.f.node(c.addr)
	if strings.EqualFold(masterHost, "no") && strings.EqualFold(masterPort, "one") {
		n.role = "master"
		n.masterHost, n.masterPort = "", ""
		return nil
	}
	n.role = "slave"
	n.masterHost, n.masterPort = masterHost, masterPort
	return nil
}

func (c *client) ACLSetUser(ctx context.Context, username string, rules ...string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ACLSetUser")
	stored := append([]string(nil), rules...)
	c.f.node(c.addr).users[username] = stored
	c.f.users[username] = stored
	return nil
}

func (c *client) ACLDelUser(ctx context.Context, username string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ACLDelUser")
	delete(c.f.node(c.addr).users, username)
	delete(c.f.users, username)
	return nil
}

// BGSave bumps the node's last-save timestamp, so a LastSave taken before it
// compares lower afterwards, as controllers expect.
func (c *client) BGSave(ctx context.Context) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "BGSave")
	c.f.node(c.addr).lastSave++
	return nil
}

func (c *client) LastSave(ctx context.Context) (int64, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "LastSave")
	return c.f.node(c.addr).lastSave, nil
}

func (c *client) ConfigGet(ctx context.Context, parameter string) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ConfigGet")
	defaults := map[string]string{
		"dir":        "/data",
		"dbfilename": "dump.rdb",
	}
	if v, ok := defaults[parameter]; ok {
		return map[string]string{parameter: v}, nil
	}
	return map[string]string{}, nil
}

func (c *client) DBSize(ctx context.Context) (int64, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "DBSize")
	return c.f.node(c.addr).dbSize, nil
}

func (c *client) Close() error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "Close")
	return nil
}

// pool implements redisclient.Sentinel over the factory's configured master.
type pool struct {
	f     *Factory
	addrs []string
}

func (p *pool) key() string {
	return strings.Join(p.addrs, ",")
}

func (p *pool) GetMasterAddrFromPool(ctx context.Context, masterName string) (string, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "GetMasterAddrFromPool")
	if p.f.masterAddr == "" {
		return "", fmt.Errorf("fake: no master configured, call Factory.SetMaster")
	}
	return p.f.masterAddr, nil
}

func (p *pool) GetMasterFromPool(ctx context.Context, masterName string) (*sentinel.MasterInfo, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "GetMasterFromPool")
	if p.f.masterAddr == "" {
		return nil, fmt.Errorf("fake: no master configured, call Factory.SetMaster")
	}
	host, port := splitHostPort(p.f.masterAddr)
	slaves := 0
	for _, n := range p.f.nodes {
		if n.role == "slave" {
			slaves++
		}
	}
	return &sentinel.MasterInfo{
		Name:              masterName,
		IP:                host,
		Port:              port,
		Flags:             "master",
		NumSlaves:         strconv.Itoa(slaves),
		NumOtherSentinels: strconv.Itoa(max(len(p.addrs)-1, 0)),
		Quorum:            "2",
	}, nil
}

func (p *pool) CheckQuorumFromPool(ctx context.Context, masterName string) (bool, int, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "CheckQuorumFromPool")
	return true, len(p.addrs), nil
}
