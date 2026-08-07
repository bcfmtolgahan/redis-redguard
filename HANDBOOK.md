# RedGuard Operator Handbook

Quick guide for installing, configuring, and using the RedGuard Redis Operator.

## Table of Contents
- [Installation](#installation)
- [Quick Start](#quick-start)
- [CRD Reference](#crd-reference)
- [Monitoring](#monitoring)
- [Backup & Recovery](#backup--recovery)
- [User Management](#user-management)
- [Troubleshooting](#troubleshooting)

---

## Installation

### Prerequisites
- Kubernetes 1.20+
- kubectl configured
- Helm 3+ (optional)

### Method 1: Helm (Recommended)

```bash
# Install CRDs and operator
helm install redguard ./helm/redguard \
  --namespace redguard-system \
  --create-namespace

# Verify installation
kubectl get pods -n redguard-system
```

### Method 2: kubectl + Kustomize

```bash
# Apply CRDs
kubectl apply -f config/crd/bases/

# Deploy operator
kubectl apply -k config/default
```

### Method 3: Local Development (Kind)

```bash
# Create Kind cluster
kind create cluster --name redguard-test

# Build and load image
make docker-build IMG=redguard:dev
kind load docker-image redguard:dev --name redguard-test

# Deploy
kubectl apply -f config/crd/bases/
kubectl apply -k config/default
```

---

## Quick Start

### 1. Create a Redis Cluster

Create a basic Redis cluster with Sentinel:

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: my-redis
  namespace: default
spec:
  redisConfig:
    replicas: 3
    image: redis:7-alpine
    resources:
      requests:
        memory: "256Mi"
        cpu: "100m"
      limits:
        memory: "512Mi"
        cpu: "500m"
    storage:
      size: 1Gi
      storageClassName: standard
    auth:
      secretName: redis-password  # Create secret first

  sentinelConfig:
    replicas: 3
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000

  serviceType: ClusterIP
```

Create password secret:

```bash
kubectl create secret generic redis-password \
  --from-literal=password=mysecurepassword
```

Apply:

```bash
kubectl apply -f redis-cluster.yaml

# Check status
kubectl get redissentinel my-redis
kubectl get pods -l redis.redguard.io/redis-name=my-redis
```

### 2. Connect to Redis

```bash
# Port-forward to Redis service
kubectl port-forward svc/my-redis-redis 6379:6379

# Connect with redis-cli
redis-cli -h localhost -a mysecurepassword
```

---

## CRD Reference

### RedisSentinel

Main CRD for Redis cluster management.

**Minimal Example:**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: simple-redis
spec:
  redisConfig:
    replicas: 3
  sentinelConfig:
    replicas: 3
    quorum: 2
```

**Production Example with TLS:**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: prod-redis
spec:
  redisConfig:
    replicas: 5
    image: redis:7-alpine
    resources:
      requests:
        memory: "1Gi"
        cpu: "500m"
      limits:
        memory: "2Gi"
        cpu: "1000m"
    storage:
      size: 10Gi
      storageClassName: fast-ssd
    auth:
      secretName: redis-password
    customConfig:
      maxmemory: "1gb"
      maxmemory-policy: "allkeys-lru"

  sentinelConfig:
    replicas: 5
    quorum: 3
    downAfterMilliseconds: 3000
    failoverTimeout: 15000
    parallelSyncs: 2

  tls:
    enabled: true
    certificateSecretRef: redis-tls-cert
    clientCertRequired: true

  serviceType: LoadBalancer
```

**Status Fields:**

```bash
kubectl get redissentinel my-redis -o jsonpath='{.status}' | jq
```

- `phase`: Current phase (Pending, Running, Failed)
- `masterNode`: Current master pod address
- `readyReplicas`: Number of ready Redis replicas
- `readySentinels`: Number of ready Sentinel instances
- `lastFailoverTime`: Timestamp of last failover

---

### RedisUser

Manages Redis ACL users (Redis 6+).

**Example: Read-only User**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: readonly-user
spec:
  redisClusterRef: my-redis
  username: readonly
  passwordSecretRef: readonly-password
  enabled: true
  aclRules:
    categories: ["+@read"]
    keys: ["~*"]
```

**Example: App User with Limited Access**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: app-user
spec:
  redisClusterRef: my-redis
  username: appuser
  passwordSecretRef: app-password
  enabled: true
  aclRules:
    categories: ["+@read", "+@write", "-@admin"]
    commands: ["+get", "+set", "+del", "+incr", "+decr"]
    keys: ["~app:*", "~cache:*"]
    channels: ["&notifications:*"]
```

Create user password:

```bash
kubectl create secret generic app-password \
  --from-literal=password=appSecurePass123
```

**ACL Rules Reference:**

- **Categories**: `+@all`, `+@read`, `+@write`, `+@admin`, `+@dangerous`, etc.
- **Commands**: `+get`, `+set`, `-del`, `+info`, etc.
- **Keys**: `~*` (all), `~app:*` (prefix match), `~user:1234` (exact)
- **Channels**: `&*` (all), `&notifications:*` (prefix match)

---

### RedisBackup

Automated S3 backups with scheduling.

**Example: Daily Backup**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: daily-backup
spec:
  redisClusterRef: my-redis
  schedule: "0 2 * * *"  # Daily at 2 AM
  s3:
    bucket: my-redis-backups
    region: us-east-1
    prefix: production/
    credentialsSecretRef: s3-credentials
  retentionPolicy: 7  # Keep 7 backups
  compression: true
  suspend: false
```

**Example: MinIO (S3-compatible)**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: minio-backup
spec:
  redisClusterRef: my-redis
  schedule: "0 */6 * * *"  # Every 6 hours
  s3:
    bucket: redis-backups
    endpoint: http://minio.default.svc.cluster.local:9000
    region: us-east-1
    prefix: cluster-1/
    credentialsSecretRef: minio-credentials
  retentionPolicy: 14
  compression: true
```

**Example: One-time Backup**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: manual-backup
spec:
  redisClusterRef: my-redis
  # No schedule = one-time backup
  s3:
    bucket: redis-backups
    region: us-east-1
    useIAMRole: true  # Use IRSA on EKS
  compression: true
```

Create S3 credentials secret:

```bash
kubectl create secret generic s3-credentials \
  --from-literal=accessKeyId=AKIAIOSFODNN7EXAMPLE \
  --from-literal=secretAccessKey=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

**Cron Schedule Examples:**

- `0 2 * * *` - Daily at 2:00 AM
- `0 */6 * * *` - Every 6 hours
- `0 0 * * 0` - Weekly on Sunday midnight
- `0 0 1 * *` - Monthly on 1st day

**Check Backup Status:**

```bash
kubectl get redisbackup daily-backup -o jsonpath='{.status}' | jq
```

---

## Monitoring

### Prometheus Metrics

The operator exposes Prometheus metrics on `:8080/metrics`.

**Available Metrics:**

| Metric | Type | Description |
|--------|------|-------------|
| `redis_cluster_info` | Gauge | Cluster status (1=up, 0=down) |
| `redis_connected_replicas` | Gauge | Number of connected replicas |
| `redis_replication_lag_seconds` | Gauge | Replication lag per pod |
| `redis_sentinel_status` | Gauge | Sentinel health (1=healthy, 0=unhealthy) |
| `redis_sentinel_monitored_masters` | Gauge | Number of monitored masters |
| `redis_failover_total` | Counter | Total failover count |
| `redis_backup_status` | Gauge | Backup status (1=success, 0=failed) |
| `redis_backup_duration_seconds` | Histogram | Backup duration |
| `redis_backup_size_bytes` | Gauge | Backup size |
| `redis_user_acl_status` | Gauge | ACL user status |

**Access Metrics:**

```bash
# Port-forward metrics service
kubectl port-forward -n redguard-system \
  svc/redguard-controller-manager-metrics-service 8080:8080

# Query metrics
curl http://localhost:8080/metrics | grep redis_
```

### Grafana Dashboard

Import pre-built dashboard (if available) or create custom dashboard with these queries:

**Cluster Health:**
```promql
redis_cluster_info{namespace="default"}
```

**Replication Lag:**
```promql
redis_replication_lag_seconds{namespace="default"}
```

**Backup Success Rate:**
```promql
rate(redis_backup_status{status="success"}[5m])
```

---

## Backup & Recovery

### Manual Backup

Trigger immediate backup:

```bash
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: manual-backup-$(date +%Y%m%d-%H%M%S)
spec:
  redisClusterRef: my-redis
  s3:
    bucket: redis-backups
    region: us-east-1
    credentialsSecretRef: s3-credentials
  compression: true
EOF
```

### Check Backup Status

```bash
kubectl get redisbackup
kubectl describe redisbackup manual-backup-20260113-120000
```

### Restore from Backup

Currently manual restore process:

1. Download RDB file from S3:
```bash
aws s3 cp s3://redis-backups/production/my-redis/backup-20260113-020000.rdb.gz ./
gunzip backup-20260113-020000.rdb.gz
```

2. Stop Redis pods:
```bash
kubectl scale statefulset my-redis-redis --replicas=0
```

3. Copy RDB to persistent volume:
```bash
kubectl cp backup-20260113-020000.rdb my-redis-redis-0:/data/dump.rdb
```

4. Restart Redis:
```bash
kubectl scale statefulset my-redis-redis --replicas=3
```

---

## User Management

### Create User

```bash
# Create password secret
kubectl create secret generic myapp-password \
  --from-literal=password=SecurePassword123

# Create user
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: myapp-user
spec:
  redisClusterRef: my-redis
  username: myapp
  passwordSecretRef: myapp-password
  enabled: true
  aclRules:
    categories: ["+@read", "+@write"]
    keys: ["~myapp:*"]
EOF
```

### Test User Access

```bash
# Connect as user
redis-cli -h localhost -p 6379 --user myapp --pass SecurePassword123

# Test permissions
127.0.0.1:6379> SET myapp:key1 value1
OK
127.0.0.1:6379> SET other:key1 value1
(error) NOPERM this user has no permissions to access one of the keys used as arguments
```

### Update User Permissions

Edit the RedisUser resource:

```bash
kubectl edit redisuser myapp-user
```

Changes are applied immediately.

### Disable User

```bash
kubectl patch redisuser myapp-user --type merge -p '{"spec":{"enabled":false}}'
```

### Delete User

```bash
kubectl delete redisuser myapp-user
```

---

## Troubleshooting

### Check Operator Logs

```bash
kubectl logs -n redguard-system -l control-plane=controller-manager --tail=100 -f
```

### Check Resource Status

```bash
# RedisSentinel status
kubectl get redissentinel my-redis -o yaml

# Pod status
kubectl get pods -l redis.redguard.io/redis-name=my-redis

# Events
kubectl get events --sort-by=.lastTimestamp | grep redis
```

### Common Issues

**1. Pods not starting**

Check resources and storage:
```bash
kubectl describe pod my-redis-redis-0
kubectl get pvc
```

**2. Sentinel not detecting master**

Check Sentinel logs:
```bash
kubectl logs my-redis-sentinel-0
```

**3. Failover not working**

Check quorum configuration:
```bash
kubectl get redissentinel my-redis -o jsonpath='{.spec.sentinelConfig.quorum}'
```

Quorum must be ≤ (sentinels / 2) + 1

**4. User ACL not applied**

Check RedisUser status:
```bash
kubectl describe redisuser myapp-user
```

Verify master pod is accessible:
```bash
kubectl exec -it my-redis-redis-0 -- redis-cli -a mysecurepassword ACL LIST
```

**5. Backup failing**

Check S3 credentials and permissions:
```bash
kubectl get secret s3-credentials -o yaml
kubectl logs -n redguard-system -l control-plane=controller-manager | grep -i backup
```

### Debug Mode

Enable verbose logging:

```bash
kubectl set env deployment/redguard-controller-manager -n redguard-system \
  LOG_LEVEL=debug
```

---

## Advanced Configuration

### TLS/SSL Setup

Generate self-signed certificate:

```bash
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout redis.key -out redis.crt \
  -subj "/CN=redis-cluster"

kubectl create secret generic redis-tls-cert \
  --from-file=tls.crt=redis.crt \
  --from-file=tls.key=redis.key
```

Enable TLS in RedisSentinel:

```yaml
spec:
  tls:
    enabled: true
    certificateSecretRef: redis-tls-cert
    clientCertRequired: false
```

### Custom Redis Configuration

```yaml
spec:
  redisConfig:
    customConfig:
      maxmemory: "2gb"
      maxmemory-policy: "allkeys-lru"
      slowlog-log-slower-than: "10000"
      slowlog-max-len: "128"
      tcp-keepalive: "300"
```

### Resource Quotas

```yaml
spec:
  redisConfig:
    resources:
      requests:
        memory: "2Gi"
        cpu: "1000m"
      limits:
        memory: "4Gi"
        cpu: "2000m"
  sentinelConfig:
    resources:
      requests:
        memory: "256Mi"
        cpu: "100m"
      limits:
        memory: "512Mi"
        cpu: "500m"
```

---

## Best Practices

### Production Checklist

- ✅ Use at least 3 Redis replicas
- ✅ Use at least 3 Sentinel replicas (odd number)
- ✅ Set appropriate quorum (typically N/2 + 1)
- ✅ Enable authentication with strong passwords
- ✅ Use TLS for production
- ✅ Configure resource limits
- ✅ Use fast SSD storage (e.g., gp3, local-ssd)
- ✅ Set up automated backups with retention
- ✅ Monitor with Prometheus/Grafana
- ✅ Use ACL for fine-grained access control
- ✅ Test failover scenarios regularly

### High Availability

Spread pods across nodes/zones:

```yaml
spec:
  redisConfig:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app: redis
          topologyKey: kubernetes.io/hostname
```

### Security

1. **Network Policies**: Restrict access to Redis pods
2. **Pod Security Standards**: Use restricted PSS
3. **Secret Management**: Use external secret managers (Vault, AWS Secrets Manager)
4. **RBAC**: Limit who can create/modify Redis resources

---

## Support & Contributing

- **Issues**: [GitHub Issues](https://github.com/redguard/redguard/issues)
- **Documentation**: [Full Docs](https://github.com/redguard/redguard/docs)
- **License**: Apache 2.0

---

**Version**: v0.1.0-alpha
**Last Updated**: 2026-01-13
