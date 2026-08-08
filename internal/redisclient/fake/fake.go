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

// Roles exactly as Redis spells them in INFO replication, which is what the
// controllers compare against.
const (
	roleMaster  = "master"
	roleReplica = "slave"
)

// Factory hands out fake clients backed by shared in-memory state. Safe for
// concurrent use; every client method appends "<addr>:<method>" to one call
// log so tests can assert which pods were touched, in what order.
type Factory struct {
	mu            sync.Mutex
	nodes         map[string]*node
	masterAddr    string
	users         map[string][]string
	masterOptions map[string]string
	// monitorDefault seeds the monitor state a sentinel reports for the
	// master: quorum, down-after-milliseconds, failover-timeout,
	// parallel-syncs. monitorByAddr holds each member's own copy, keyed by
	// dial address, because SENTINEL SET is per-instance state that never
	// propagates on its own: a pool-wide write updates each member it
	// reaches, and only those, no matter which pool later reads them.
	monitorDefault map[string]string
	monitorByAddr  map[string]map[string]string
	errs           map[string]error
	calls          []string
	// ops is a second log carrying the arguments the calls log drops, so a
	// test can assert what was written and in which order across nodes.
	ops       []string
	failovers []string
	resets    []string
	// knownSentinels overrides the peer count the pool reports. Zero means
	// derive it from the pool size, which is a set that agrees with itself.
	knownSentinels int
}

// node is the state of one fake Redis instance, keyed by dial address.
// users is runtime state; savedUsers is what an ACL SAVE persisted and is
// therefore what a restart of that node would come back with.
type node struct {
	role       string
	masterHost string
	masterPort string
	users      map[string][]string
	savedUsers map[string][]string
	keyspace   map[int]int64
	config     map[string]string
	lastSave   int64
}

func NewFactory() *Factory {
	return &Factory{
		nodes:          map[string]*node{},
		users:          map[string][]string{},
		masterOptions:  map[string]string{},
		monitorDefault: map[string]string{},
		monitorByAddr:  map[string]map[string]string{},
		errs:           map[string]error{},
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

// SetReplicaOf pins addr as a replica of followed, whatever the configured
// master is. It models a node that came back following an address Sentinel no
// longer reports, which is what a pod restarted during a failover does. The
// next SetMaster recomputes the role and overwrites this.
func (f *Factory) SetReplicaOf(addr, followed string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.node(addr)
	n.role = roleReplica
	n.masterHost, n.masterPort = splitHostPort(followed)
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

// SavedUsers returns the users one node persisted through ACL SAVE, which is
// the set that would survive a restart of that node.
func (f *Factory) SavedUsers(addr string) map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for u, rules := range f.node(addr).savedUsers {
		out[u] = append([]string(nil), rules...)
	}
	return out
}

// SetKeyspace declares the per-database key counts of one node, driving both
// GetKeyspaceInfo and DBSize (which, like the real command, sees DB 0 only).
func (f *Factory) SetKeyspace(addr string, dbs map[int]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ks := make(map[int]int64, len(dbs))
	for db, n := range dbs {
		ks[db] = n
	}
	f.node(addr).keyspace = ks
}

// SetError injects err into every future call of method on addr. For sentinel
// pool methods, addr is the comma-joined pool address list.
func (f *Factory) SetError(addr, method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[addr+":"+method] = err
}

// Configs returns the parameters written to addr through ConfigSet.
func (f *Factory) Configs(addr string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.node(addr).config {
		out[k] = v
	}
	return out
}

// Failovers returns the master names a forced failover was requested for, in
// order.
func (f *Factory) Failovers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.failovers...)
}

// SetKnownSentinels declares how many sentinels the pool reports as monitoring
// the master, itself included. A count above the number of pods that exist is
// what a scale-down leaves behind: the survivors keep the removed peers until
// a SENTINEL RESET.
func (f *Factory) SetKnownSentinels(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.knownSentinels = n
}

// Resets returns the master names SENTINEL RESET was issued for, in order.
func (f *Factory) Resets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resets...)
}

// MasterOptions returns the last value written per option through
// SetMasterOptionAll, across all pools.
func (f *Factory) MasterOptions() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.masterOptions))
	for k, v := range f.masterOptions {
		out[k] = v
	}
	return out
}

// SetMonitorConfig declares the monitor parameters every fake sentinel reports
// for the master: quorum, down-after-milliseconds, failover-timeout,
// parallel-syncs. Unset keys report as empty. Per-member state written earlier
// through SetMasterOptionAll is discarded.
func (f *Factory) SetMonitorConfig(cfg map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitorDefault = map[string]string{}
	for k, v := range cfg {
		f.monitorDefault[k] = v
	}
	f.monitorByAddr = map[string]map[string]string{}
}

// monitorFor returns the monitor state one sentinel member reports, seeding it
// from the default on first use. Caller holds f.mu.
func (f *Factory) monitorFor(addr string) map[string]string {
	state, ok := f.monitorByAddr[addr]
	if !ok {
		state = map[string]string{}
		for k, v := range f.monitorDefault {
			state[k] = v
		}
		f.monitorByAddr[addr] = state
	}
	return state
}

// Ops returns the ordered argument-level log: every write with the values it
// carried, formatted "<addr>:<method>:<args>". Calls() keeps the coarse form.
func (f *Factory) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// recordOp appends to the argument-level log. Caller holds f.mu.
func (f *Factory) recordOp(entry string) {
	f.ops = append(f.ops, entry)
}

// errorFor returns the injected error for addr and method. Caller holds f.mu.
func (f *Factory) errorFor(addr, method string) error {
	return f.errs[addr+":"+method]
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
		n = &node{
			users:      map[string][]string{},
			savedUsers: map[string][]string{},
			keyspace:   map[int]int64{},
			config:     map[string]string{},
		}
		f.nodes[addr] = n
		f.syncRole(addr, n)
	}
	return n
}

// syncRole derives a node's role from the configured master. Caller holds f.mu.
func (f *Factory) syncRole(addr string, n *node) {
	if f.masterAddr == "" || addr == f.masterAddr {
		n.role = roleMaster
		n.masterHost, n.masterPort = "", ""
		return
	}
	n.role = roleReplica
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
	return c.f.errorFor(c.addr, "Ping")
}

func (c *client) IsMaster(ctx context.Context) (bool, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "IsMaster")
	if err := c.f.errorFor(c.addr, "IsMaster"); err != nil {
		return false, err
	}
	return c.f.node(c.addr).role == roleMaster, nil
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
	if n.role == roleReplica {
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

// GetKeyspaceInfo reports the node's keyspace in the INFO keyspace shape:
// one "dbN" entry per database that holds keys.
func (c *client) GetKeyspaceInfo(ctx context.Context) (map[string]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "GetKeyspaceInfo")
	if err := c.f.errorFor(c.addr, "GetKeyspaceInfo"); err != nil {
		return nil, err
	}
	info := map[string]string{}
	for db, n := range c.f.node(c.addr).keyspace {
		if n == 0 {
			continue
		}
		info[fmt.Sprintf("db%d", db)] = fmt.Sprintf("keys=%d,expires=0,avg_ttl=0", n)
	}
	return info, nil
}

func (c *client) SlaveOf(ctx context.Context, masterHost, masterPort string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "SlaveOf")
	n := c.f.node(c.addr)
	if strings.EqualFold(masterHost, "no") && strings.EqualFold(masterPort, "one") {
		n.role = roleMaster
		n.masterHost, n.masterPort = "", ""
		return nil
	}
	n.role = roleReplica
	n.masterHost, n.masterPort = masterHost, masterPort
	return nil
}

func (c *client) ACLSetUser(ctx context.Context, username string, rules ...string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ACLSetUser")
	c.f.recordOp(c.addr + ":ACLSetUser:" + username + ":" + strings.Join(rules, " "))
	if err := c.f.errorFor(c.addr, "ACLSetUser"); err != nil {
		return err
	}
	stored := append([]string(nil), rules...)
	c.f.node(c.addr).users[username] = stored
	c.f.users[username] = stored
	return nil
}

func (c *client) ACLDelUser(ctx context.Context, username string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ACLDelUser")
	if err := c.f.errorFor(c.addr, "ACLDelUser"); err != nil {
		return err
	}
	delete(c.f.node(c.addr).users, username)
	delete(c.f.users, username)
	return nil
}

// ACLSave snapshots the node's runtime users into its saved set, mirroring the
// real command writing them to the aclfile.
func (c *client) ACLSave(ctx context.Context) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ACLSave")
	if err := c.f.errorFor(c.addr, "ACLSave"); err != nil {
		return err
	}
	n := c.f.node(c.addr)
	n.savedUsers = make(map[string][]string, len(n.users))
	for u, rules := range n.users {
		n.savedUsers[u] = append([]string(nil), rules...)
	}
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
	if err := c.f.errorFor(c.addr, "DBSize"); err != nil {
		return 0, err
	}
	return c.f.node(c.addr).keyspace[0], nil
}

func (c *client) ConfigSet(ctx context.Context, parameter, value string) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ConfigSet")
	c.f.recordOp(c.addr + ":ConfigSet:" + parameter + "=" + value)
	if err := c.f.errorFor(c.addr, "ConfigSet"); err != nil {
		return err
	}
	c.f.node(c.addr).config[parameter] = value
	return nil
}

func (c *client) ShutdownNoSave(ctx context.Context) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.record(c.addr, "ShutdownNoSave")
	return c.f.errorFor(c.addr, "ShutdownNoSave")
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
		if n.role == roleReplica {
			slaves++
		}
	}
	others := max(len(p.addrs)-1, 0)
	if p.f.knownSentinels > 0 {
		others = p.f.knownSentinels - 1
	}
	// The real pool serves the read from the first member that answers, and
	// monitor state is that member's own view.
	memberAddr := p.key()
	if len(p.addrs) > 0 {
		memberAddr = p.addrs[0]
	}
	monitor := p.f.monitorFor(memberAddr)
	quorum := monitor["quorum"]
	if quorum == "" {
		quorum = "2"
	}
	return &sentinel.MasterInfo{
		Name:                  masterName,
		IP:                    host,
		Port:                  port,
		Flags:                 roleMaster,
		NumSlaves:             strconv.Itoa(slaves),
		NumOtherSentinels:     strconv.Itoa(others),
		Quorum:                quorum,
		DownAfterMilliseconds: monitor["down-after-milliseconds"],
		FailoverTimeout:       monitor["failover-timeout"],
		ParallelSyncs:         monitor["parallel-syncs"],
	}, nil
}

// ResetMasterAll records the call. The real command makes every sentinel forget
// the master's replicas and peers and rediscover them, which no in-memory state
// here can usefully model.
func (p *pool) ResetMasterAll(ctx context.Context, masterName string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "ResetMasterAll")
	if err := p.f.errorFor(p.key(), "ResetMasterAll"); err != nil {
		return err
	}
	p.f.resets = append(p.f.resets, masterName)
	return nil
}

func (p *pool) CheckQuorumFromPool(ctx context.Context, masterName string) (bool, int, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "CheckQuorumFromPool")
	return true, len(p.addrs), nil
}

// FailoverFromPool records the request but does not move the master. Which
// replica a real Sentinel promotes is its own decision and takes seconds, so a
// test drives the outcome with SetMaster and can assert on the state in
// between: the request made and the promotion not yet done.
func (p *pool) FailoverFromPool(ctx context.Context, masterName string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "FailoverFromPool")
	if err := p.f.errorFor(p.key(), "FailoverFromPool"); err != nil {
		return err
	}
	p.f.failovers = append(p.f.failovers, masterName)
	return nil
}

func (p *pool) SetMasterOptionAll(ctx context.Context, masterName, option, value string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "SetMasterOptionAll")
	p.f.recordOp(p.key() + ":SetMasterOptionAll:" + option + "=" + value)
	if err := p.f.errorFor(p.key(), "SetMasterOptionAll"); err != nil {
		return err
	}
	p.f.masterOptions[option] = value
	switch option {
	case "quorum", "down-after-milliseconds", "failover-timeout", "parallel-syncs":
		// The real command runs against every member individually, so each
		// member's own view updates and later single-member reads see it.
		for _, addr := range p.addrs {
			p.f.monitorFor(addr)[option] = value
		}
	}
	return nil
}

func (p *pool) AddPasswordAll(ctx context.Context, password string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "AddPasswordAll")
	p.f.recordOp(p.key() + ":AddPasswordAll:" + password)
	return p.f.errorFor(p.key(), "AddPasswordAll")
}

func (p *pool) ResetPasswordAll(ctx context.Context, password string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "ResetPasswordAll")
	p.f.recordOp(p.key() + ":ResetPasswordAll:" + password)
	return p.f.errorFor(p.key(), "ResetPasswordAll")
}

func (p *pool) SetOutboundPasswordAll(ctx context.Context, masterName, password string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.record(p.key(), "SetOutboundPasswordAll")
	p.f.recordOp(p.key() + ":SetOutboundPasswordAll:" + masterName + ":" + password)
	return p.f.errorFor(p.key(), "SetOutboundPasswordAll")
}
