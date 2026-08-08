package redisutils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient wraps redis client with utility methods
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient creates a new Redis client
func NewRedisClient(addr string, password string) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	return &RedisClient{client: client}
}

// Ping checks if Redis is responsive
func (rc *RedisClient) Ping(ctx context.Context) error {
	return rc.client.Ping(ctx).Err()
}

// GetRole returns the role of the Redis instance (master or slave)
func (rc *RedisClient) GetRole(ctx context.Context) (string, error) {
	result, err := rc.client.Info(ctx, "replication").Result()
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(result, "\n") {
		if strings.HasPrefix(line, "role:") {
			return strings.TrimSpace(strings.Split(line, ":")[1]), nil
		}
	}

	return "", fmt.Errorf("role not found in info replication")
}

// IsMaster checks if the instance is a master
func (rc *RedisClient) IsMaster(ctx context.Context) (bool, error) {
	role, err := rc.GetRole(ctx)
	if err != nil {
		return false, err
	}
	return role == "master", nil
}

// GetReplicationInfo returns replication information
func (rc *RedisClient) GetReplicationInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "replication").Result()
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

// SlaveOf configures instance as a replica
func (rc *RedisClient) SlaveOf(ctx context.Context, masterHost string, masterPort string) error {
	return rc.client.SlaveOf(ctx, masterHost, masterPort).Err()
}

// SlaveOfNoOne promotes instance to master
func (rc *RedisClient) SlaveOfNoOne(ctx context.Context) error {
	return rc.client.SlaveOf(ctx, "NO", "ONE").Err()
}

// Close closes the client connection
func (rc *RedisClient) Close() error {
	return rc.client.Close()
}

// ACLSetUser creates or updates a Redis ACL user
func (rc *RedisClient) ACLSetUser(ctx context.Context, username string, rules ...string) error {
	args := []interface{}{"ACL", "SETUSER", username}
	for _, rule := range rules {
		args = append(args, rule)
	}
	return rc.client.Do(ctx, args...).Err()
}

// ACLDelUser deletes a Redis ACL user
func (rc *RedisClient) ACLDelUser(ctx context.Context, username string) error {
	return rc.client.Do(ctx, "ACL", "DELUSER", username).Err()
}

// ACLUsers returns the usernames this node currently knows, 'default' included.
func (rc *RedisClient) ACLUsers(ctx context.Context) ([]string, error) {
	return rc.client.Do(ctx, "ACL", "USERS").StringSlice()
}

// ACLSave writes the in-memory ACL users to the configured aclfile. Without it
// every SETUSER/DELUSER is lost when the server restarts.
func (rc *RedisClient) ACLSave(ctx context.Context) error {
	return rc.client.Do(ctx, "ACL", "SAVE").Err()
}

// ACLGetUser retrieves ACL information for a user
func (rc *RedisClient) ACLGetUser(ctx context.Context, username string) (map[string]interface{}, error) {
	result, err := rc.client.Do(ctx, "ACL", "GETUSER", username).Result()
	if err != nil {
		return nil, err
	}

	// Parse the result into a map
	arr, ok := result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type")
	}

	userInfo := make(map[string]interface{})
	for i := 0; i < len(arr); i += 2 {
		if i+1 < len(arr) {
			key := fmt.Sprint(arr[i])
			userInfo[key] = arr[i+1]
		}
	}

	return userInfo, nil
}

// GetReplicationLag returns replication lag in seconds for a replica (legacy method)
func (rc *RedisClient) GetReplicationLag(ctx context.Context) (int64, error) {
	info, err := rc.GetReplicationInfo(ctx)
	if err != nil {
		return 0, err
	}

	role := info["role"]
	if role != "slave" {
		return 0, nil // Master has no lag
	}

	// Check master_link_status
	if info["master_link_status"] != "up" {
		return -1, fmt.Errorf("master link down")
	}

	// Get lag from master_last_io_seconds_ago
	lagStr, ok := info["master_last_io_seconds_ago"]
	if !ok {
		return 0, nil
	}

	var lag int64
	_, err = fmt.Sscanf(lagStr, "%d", &lag)
	if err != nil {
		return 0, err
	}

	return lag, nil
}

// GetReplicationLagByOffset calculates accurate lag using replication offset comparison
func (rc *RedisClient) GetReplicationLagByOffset(ctx context.Context) (int64, error) {
	info, err := rc.GetReplicationInfo(ctx)
	if err != nil {
		return 0, err
	}

	role := info["role"]
	if role != "slave" {
		return 0, nil // Master has no lag
	}

	// Check master_link_status
	if info["master_link_status"] != "up" {
		return -1, fmt.Errorf("master link down")
	}

	// Get slave_repl_offset (how much data this replica has received)
	slaveOffsetStr, ok := info["slave_repl_offset"]
	if !ok {
		// Fallback to master_repl_offset if slave_repl_offset not available
		slaveOffsetStr = info["master_repl_offset"]
	}

	slaveOffset, err := strconv.ParseInt(slaveOffsetStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse slave offset: %w", err)
	}

	// Get master_repl_offset (reported by master via replication)
	masterOffsetStr, ok := info["master_repl_offset"]
	if !ok {
		return 0, nil // Cannot determine lag without master offset
	}

	masterOffset, err := strconv.ParseInt(masterOffsetStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse master offset: %w", err)
	}

	// Return byte lag (master offset - slave offset)
	return masterOffset - slaveOffset, nil
}

// BGSave triggers a background RDB save
func (rc *RedisClient) BGSave(ctx context.Context) error {
	return rc.client.BgSave(ctx).Err()
}

// LastSave returns the Unix timestamp of the last successful RDB save
func (rc *RedisClient) LastSave(ctx context.Context) (int64, error) {
	return rc.client.LastSave(ctx).Result()
}

// LastSaveTime returns the Time of the last successful RDB save
func (rc *RedisClient) LastSaveTime(ctx context.Context) (time.Time, error) {
	result, err := rc.client.LastSave(ctx).Result()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(result, 0), nil
}

// GetMemoryInfo returns Redis memory statistics
func (rc *RedisClient) GetMemoryInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "memory").Result()
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

// GetClientInfo returns Redis client connection statistics
func (rc *RedisClient) GetClientInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "clients").Result()
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

// GetPersistenceInfo returns Redis persistence statistics
func (rc *RedisClient) GetPersistenceInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "persistence").Result()
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

// GetStatsInfo returns Redis general statistics
func (rc *RedisClient) GetStatsInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "stats").Result()
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

// ConfigSet sets a Redis configuration parameter at runtime
func (rc *RedisClient) ConfigSet(ctx context.Context, parameter, value string) error {
	return rc.client.ConfigSet(ctx, parameter, value).Err()
}

// ConfigGet gets a Redis configuration parameter
func (rc *RedisClient) ConfigGet(ctx context.Context, parameter string) (map[string]string, error) {
	return rc.client.ConfigGet(ctx, parameter).Result()
}

// ConfigRewrite rewrites the redis.conf file with the current runtime configuration
func (rc *RedisClient) ConfigRewrite(ctx context.Context) error {
	return rc.client.ConfigRewrite(ctx).Err()
}

// NewRedisClientWithTLS creates a new Redis client with TLS configuration
func NewRedisClientWithTLS(addr, password string, tlsConfig *tls.Config) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		TLSConfig:    tlsConfig,
	})

	return &RedisClient{client: client}
}

// DBSize returns the number of keys in the currently selected database
func (rc *RedisClient) DBSize(ctx context.Context) (int64, error) {
	return rc.client.DBSize(ctx).Result()
}

// ShutdownNoSave asks the server to exit without a final RDB save. A server
// that obeys closes the connection instead of replying, so only a Redis error
// reply (a refusal) is surfaced; network errors mean the server went away as
// requested. A server that ignored the command is caught by the caller's
// follow-up state checks, not here.
func (rc *RedisClient) ShutdownNoSave(ctx context.Context) error {
	err := rc.client.ShutdownNoSave(ctx).Err()
	if err == nil {
		return nil
	}
	var redisErr redis.Error
	if errors.As(err, &redisErr) {
		return err
	}
	return nil
}

// GetKeyspaceInfo returns keyspace statistics per database
func (rc *RedisClient) GetKeyspaceInfo(ctx context.Context) (map[string]string, error) {
	result, err := rc.client.Info(ctx, "keyspace").Result()
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
