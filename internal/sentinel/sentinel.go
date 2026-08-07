package sentinel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SentinelClient wraps redis client for Sentinel commands
type SentinelClient struct {
	client *redis.Client
}

// MasterInfo contains information about the monitored master
type MasterInfo struct {
	Name              string
	IP                string
	Port              string
	Quorum            string
	NumSlaves         string
	NumOtherSentinels string
	Flags             string
}

// ReplicaInfo contains information about a replica
type ReplicaInfo struct {
	Name             string
	IP               string
	Port             string
	Flags            string
	MasterLinkStatus string
	MasterHost       string
	MasterPort       string
}

// NewSentinelClient creates a new Sentinel client
func NewSentinelClient(addr string) *SentinelClient {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second,
	})

	return &SentinelClient{client: client}
}

// NewSentinelClientWithAuth creates a new Sentinel client with authentication
func NewSentinelClientWithAuth(addr, password string) *SentinelClient {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DialTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second,
	})

	return &SentinelClient{client: client}
}

// NewSentinelClientWithTLS creates a new Sentinel client with TLS
func NewSentinelClientWithTLS(addr, password string, tlsConfig *tls.Config) *SentinelClient {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DialTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second,
		TLSConfig:   tlsConfig,
	})

	return &SentinelClient{client: client}
}

// SentinelClientPool manages connections to multiple sentinels with fallback
type SentinelClientPool struct {
	addresses []string
	password  string
	tlsConfig *tls.Config
}

// NewSentinelClientPool creates a new pool for multiple sentinels
func NewSentinelClientPool(addresses []string, password string) *SentinelClientPool {
	return &SentinelClientPool{
		addresses: addresses,
		password:  password,
	}
}

// NewSentinelClientPoolWithTLS creates a new pool with TLS support
func NewSentinelClientPoolWithTLS(addresses []string, password string, tlsConfig *tls.Config) *SentinelClientPool {
	return &SentinelClientPool{
		addresses: addresses,
		password:  password,
		tlsConfig: tlsConfig,
	}
}

// GetMasterAddrFromPool tries multiple sentinels to get the master address
func (p *SentinelClientPool) GetMasterAddrFromPool(ctx context.Context, masterName string) (string, error) {
	var lastErr error
	for _, addr := range p.addresses {
		var client *SentinelClient
		if p.tlsConfig != nil {
			client = NewSentinelClientWithTLS(addr, p.password, p.tlsConfig)
		} else if p.password != "" {
			client = NewSentinelClientWithAuth(addr, p.password)
		} else {
			client = NewSentinelClient(addr)
		}

		masterAddr, err := client.GetMasterAddr(ctx, masterName)
		client.Close()

		if err == nil && masterAddr != "" {
			return masterAddr, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return "", fmt.Errorf("all sentinels failed, last error: %w", lastErr)
	}
	return "", fmt.Errorf("no sentinels available")
}

// GetMasterFromPool tries multiple sentinels to get master info
func (p *SentinelClientPool) GetMasterFromPool(ctx context.Context, masterName string) (*MasterInfo, error) {
	var lastErr error
	for _, addr := range p.addresses {
		var client *SentinelClient
		if p.tlsConfig != nil {
			client = NewSentinelClientWithTLS(addr, p.password, p.tlsConfig)
		} else if p.password != "" {
			client = NewSentinelClientWithAuth(addr, p.password)
		} else {
			client = NewSentinelClient(addr)
		}

		masterInfo, err := client.GetMaster(ctx, masterName)
		client.Close()

		if err == nil && masterInfo != nil {
			return masterInfo, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all sentinels failed, last error: %w", lastErr)
	}
	return nil, fmt.Errorf("no sentinels available")
}

// SetMasterOptionAll applies SENTINEL SET to every sentinel in the pool.
// SENTINEL SET is per-instance state that never propagates between sentinels,
// so unlike the read paths this must reach all of them; any failure is
// returned because one sentinel left on the old value defeats the caller's
// intent, for example quiescing failure detection during a restore.
func (p *SentinelClientPool) SetMasterOptionAll(ctx context.Context, masterName, option, value string) error {
	var errs []error
	for _, addr := range p.addresses {
		client := p.newClient(addr)
		err := client.SetMasterOption(ctx, masterName, option, value)
		client.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("sentinel %s: %w", addr, err))
		}
	}
	return errors.Join(errs...)
}

// ResetMasterAll makes every sentinel in the pool forget what it learned about
// the master and rediscover it. Like SENTINEL SET this is per-instance state
// that never propagates, so it has to reach all of them: one sentinel left
// counting removed peers still refuses to vote a failover leader through.
func (p *SentinelClientPool) ResetMasterAll(ctx context.Context, masterName string) error {
	var errs []error
	for _, addr := range p.addresses {
		client := p.newClient(addr)
		_, err := client.SentinelReset(ctx, masterName)
		client.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("sentinel %s: %w", addr, err))
		}
	}
	return errors.Join(errs...)
}

// FailoverFromPool asks the first reachable sentinel to promote a replica.
// SENTINEL FAILOVER is deliberately not broadcast: it forces a failover without
// asking the other sentinels for agreement, so one acceptance is the whole
// operation and sending it to every member would only start it again on a
// cluster that is already mid-promotion.
func (p *SentinelClientPool) FailoverFromPool(ctx context.Context, masterName string) error {
	var errs []error
	for _, addr := range p.addresses {
		client := p.newClient(addr)
		err := client.Failover(ctx, masterName)
		client.Close()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("sentinel %s: %w", addr, err))
	}
	if len(errs) == 0 {
		return fmt.Errorf("no sentinels available")
	}
	return fmt.Errorf("no sentinel accepted the failover: %w", errors.Join(errs...))
}

// newClient builds a client for one pool member with the pool's credentials.
func (p *SentinelClientPool) newClient(addr string) *SentinelClient {
	if p.tlsConfig != nil {
		return NewSentinelClientWithTLS(addr, p.password, p.tlsConfig)
	}
	if p.password != "" {
		return NewSentinelClientWithAuth(addr, p.password)
	}
	return NewSentinelClient(addr)
}

// CheckQuorumFromPool tries to check quorum health from available sentinels
func (p *SentinelClientPool) CheckQuorumFromPool(ctx context.Context, masterName string) (bool, int, error) {
	for _, addr := range p.addresses {
		var client *SentinelClient
		if p.tlsConfig != nil {
			client = NewSentinelClientWithTLS(addr, p.password, p.tlsConfig)
		} else if p.password != "" {
			client = NewSentinelClientWithAuth(addr, p.password)
		} else {
			client = NewSentinelClient(addr)
		}

		healthy, count, err := client.CheckQuorumHealth(ctx, masterName)
		client.Close()

		if err == nil {
			return healthy, count, nil
		}
	}

	return false, 0, fmt.Errorf("unable to check quorum from any sentinel")
}

// Ping checks if Sentinel is responsive
func (sc *SentinelClient) Ping(ctx context.Context) error {
	return sc.client.Ping(ctx).Err()
}

// replyFields flattens one sentinel field reply into its fields. RESP2 answers
// with a flat array of alternating name and value; RESP3, which go-redis
// negotiates by default, answers with a map.
func replyFields(reply interface{}) (map[string]string, error) {
	switch v := reply.(type) {
	case map[interface{}]interface{}:
		fields := make(map[string]string, len(v))
		for name, value := range v {
			fields[fmt.Sprintf("%v", name)] = fmt.Sprintf("%v", value)
		}
		return fields, nil
	case []interface{}:
		if len(v)%2 != 0 {
			return nil, fmt.Errorf("sentinel reply has %d fields, want an even count", len(v))
		}
		fields := make(map[string]string, len(v)/2)
		for i := 0; i < len(v); i += 2 {
			fields[fmt.Sprintf("%v", v[i])] = fmt.Sprintf("%v", v[i+1])
		}
		return fields, nil
	default:
		return nil, fmt.Errorf("unexpected sentinel reply type %T", reply)
	}
}

// replyEntries flattens a sentinel reply that is a list of field replies.
func replyEntries(reply interface{}) ([]map[string]string, error) {
	values, ok := reply.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected sentinel reply type %T", reply)
	}
	entries := make([]map[string]string, 0, len(values))
	for _, v := range values {
		fields, err := replyFields(v)
		if err != nil {
			return nil, err
		}
		entries = append(entries, fields)
	}
	return entries, nil
}

func parseMasterReply(reply interface{}) (*MasterInfo, error) {
	fields, err := replyFields(reply)
	if err != nil {
		return nil, err
	}
	return &MasterInfo{
		Name:              fields["name"],
		IP:                fields["ip"],
		Port:              fields["port"],
		Quorum:            fields["quorum"],
		NumSlaves:         fields["num-slaves"],
		NumOtherSentinels: fields["num-other-sentinels"],
		Flags:             fields["flags"],
	}, nil
}

func parseSentinelsReply(reply interface{}) ([]map[string]string, error) {
	return replyEntries(reply)
}

func parseReplicasReply(reply interface{}) ([]ReplicaInfo, error) {
	entries, err := replyEntries(reply)
	if err != nil {
		return nil, err
	}
	replicas := make([]ReplicaInfo, 0, len(entries))
	for _, fields := range entries {
		replicas = append(replicas, ReplicaInfo{
			Name:             fields["name"],
			IP:               fields["ip"],
			Port:             fields["port"],
			Flags:            fields["flags"],
			MasterLinkStatus: fields["master-link-status"],
			MasterHost:       fields["master-host"],
			MasterPort:       fields["master-port"],
		})
	}
	return replicas, nil
}

// GetMaster returns the current master information
func (sc *SentinelClient) GetMaster(ctx context.Context, masterName string) (*MasterInfo, error) {
	result, err := sc.client.Do(ctx, "SENTINEL", "master", masterName).Result()
	if err != nil {
		return nil, err
	}
	return parseMasterReply(result)
}

// GetMasterAddr returns the master address
func (sc *SentinelClient) GetMasterAddr(ctx context.Context, masterName string) (string, error) {
	result, err := sc.client.Do(ctx, "SENTINEL", "get-master-addr-by-name", masterName).Result()
	if err != nil {
		return "", err
	}

	if result == nil {
		return "", fmt.Errorf("master not found")
	}

	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return "", fmt.Errorf("unexpected result format")
	}

	ip := fmt.Sprintf("%v", values[0])
	port := fmt.Sprintf("%v", values[1])

	return fmt.Sprintf("%s:%s", ip, port), nil
}

// IsMasterDown checks if the master is considered down
func (sc *SentinelClient) IsMasterDown(ctx context.Context, masterName string) (bool, error) {
	info, err := sc.GetMaster(ctx, masterName)
	if err != nil {
		return false, err
	}

	return strings.Contains(info.Flags, "down"), nil
}

// Failover triggers a manual failover
func (sc *SentinelClient) Failover(ctx context.Context, masterName string) error {
	return sc.client.Do(ctx, "SENTINEL", "failover", masterName).Err()
}

// GetSentinels returns the list of sentinels for the master
func (sc *SentinelClient) GetSentinels(ctx context.Context, masterName string) ([]map[string]string, error) {
	result, err := sc.client.Do(ctx, "SENTINEL", "sentinels", masterName).Result()
	if err != nil {
		return nil, err
	}
	return parseSentinelsReply(result)
}

// Close closes the client connection
func (sc *SentinelClient) Close() error {
	return sc.client.Close()
}

// CheckQuorumHealth verifies that the sentinel quorum is healthy for the given master
// Returns: (isHealthy, numSentinelsAgree, error)
func (sc *SentinelClient) CheckQuorumHealth(ctx context.Context, masterName string) (bool, int, error) {
	// SENTINEL CKQUORUM <master-name>
	// Returns OK if quorum can be reached, error otherwise
	result, err := sc.client.Do(ctx, "SENTINEL", "CKQUORUM", masterName).Result()
	if err != nil {
		// Check if it's a quorum error vs connection error
		errStr := err.Error()
		if strings.Contains(errStr, "NOQUORUM") {
			return false, 0, nil
		}
		return false, 0, err
	}

	// Parse the OK response - usually "OK <n> usable Sentinels..."
	resultStr := fmt.Sprintf("%v", result)
	if strings.HasPrefix(resultStr, "OK") {
		// Try to extract number of sentinels from response
		// Format: "OK 3 usable Sentinels. Quorum and failover authorization is possible."
		var count int
		_, scanErr := fmt.Sscanf(resultStr, "OK %d usable", &count)
		if scanErr != nil {
			// Could not parse count, but quorum is healthy
			count = -1
		}
		return true, count, nil
	}

	return false, 0, fmt.Errorf("unexpected response: %s", resultStr)
}

// GetReplicas returns the list of replicas for the given master
func (sc *SentinelClient) GetReplicas(ctx context.Context, masterName string) ([]ReplicaInfo, error) {
	// SENTINEL REPLICAS <master-name> (or SENTINEL SLAVES for older versions)
	result, err := sc.client.Do(ctx, "SENTINEL", "REPLICAS", masterName).Result()
	if err != nil {
		// Fallback to SLAVES command for older Redis versions
		result, err = sc.client.Do(ctx, "SENTINEL", "SLAVES", masterName).Result()
		if err != nil {
			return nil, err
		}
	}
	return parseReplicasReply(result)
}

// SentinelReset resets all the masters matching the given pattern
// This forces sentinels to re-discover replicas and other sentinels
func (sc *SentinelClient) SentinelReset(ctx context.Context, pattern string) (int64, error) {
	result, err := sc.client.Do(ctx, "SENTINEL", "RESET", pattern).Result()
	if err != nil {
		return 0, err
	}

	count, ok := result.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected result type")
	}

	return count, nil
}

// FlushConfig forces sentinel to rewrite its configuration on disk
func (sc *SentinelClient) FlushConfig(ctx context.Context) error {
	return sc.client.Do(ctx, "SENTINEL", "FLUSHCONFIG").Err()
}

// GetSentinelInfo returns information about the sentinel itself
func (sc *SentinelClient) GetSentinelInfo(ctx context.Context) (map[string]string, error) {
	result, err := sc.client.Info(ctx, "sentinel").Result()
	if err != nil {
		return nil, err
	}

	info := make(map[string]string)
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			info[parts[0]] = strings.TrimSpace(parts[1])
		}
	}

	return info, nil
}

// MonitorMaster adds a new master to monitor
func (sc *SentinelClient) MonitorMaster(ctx context.Context, name, ip string, port, quorum int) error {
	return sc.client.Do(ctx, "SENTINEL", "MONITOR", name, ip, port, quorum).Err()
}

// RemoveMaster removes a master from monitoring
func (sc *SentinelClient) RemoveMaster(ctx context.Context, name string) error {
	return sc.client.Do(ctx, "SENTINEL", "REMOVE", name).Err()
}

// SetMasterOption sets a configuration option for a monitored master
func (sc *SentinelClient) SetMasterOption(ctx context.Context, masterName, option, value string) error {
	return sc.client.Do(ctx, "SENTINEL", "SET", masterName, option, value).Err()
}
