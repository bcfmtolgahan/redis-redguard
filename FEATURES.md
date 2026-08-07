# Redguard Advanced Features

This document describes the advanced production-ready features of Redguard.

## Table of Contents
- [Authentication & Authorization (ACL)](#authentication--authorization-acl)
- [TLS/SSL Encryption](#tlsssl-encryption)
- [Backup & Restore](#backup--restore)
- [Examples](#examples)

---

## Authentication & Authorization (ACL)

Redguard supports Redis 6+ Access Control Lists (ACL) through the `RedisUser` CRD.

### Features

- **Fine-grained Permissions**: Control access to specific commands, keys, and pub/sub channels
- **Category-based ACLs**: Use predefined command categories (@read, @write, @dangerous, etc.)
- **Password Management**: Secure password storage using Kubernetes Secrets
- **Multi-user Support**: Create multiple users with different permissions per cluster

### RedisUser CRD

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: app-user
spec:
  redisClusterRef: redis-cluster
  username: app-user
  passwordSecretRef: app-user-password
  aclRules:
    categories:
      - "+@read"      # Allow read commands
      - "+@write"     # Allow write commands
      - "-@dangerous" # Deny dangerous commands
    commands:
      - "+get"
      - "+set"
      - "-flushdb"
    keys:
      - "~app:*"      # Access keys matching pattern
    channels:
      - "&notifications:*"
  enabled: true
```

### ACL Categories

| Category | Description |
|----------|-------------|
| `@read` | Read-only commands (GET, MGET, etc.) |
| `@write` | Write commands (SET, DEL, etc.) |
| `@admin` | Administrative commands |
| `@dangerous` | Potentially dangerous commands (FLUSHDB, FLUSHALL, etc.) |
| `@fast` | O(1) time complexity commands |
| `@slow` | Commands that may be slow |
| `@pubsub` | Pub/Sub commands |
| `@scripting` | Lua scripting commands |

### Creating a RedisUser

1. Create password secret:
```bash
kubectl create secret generic app-user-password \
  --from-literal=password=mysecurepassword
```

2. Apply RedisUser:
```bash
kubectl apply -f redisuser.yaml
```

3. Check status:
```bash
kubectl get redisuser app-user
```

### Common ACL Patterns

#### Read-Only User
```yaml
aclRules:
  categories:
    - "+@read"
    - "-@write"
    - "-@admin"
  keys:
    - "~*"  # All keys
```

#### Application User (Limited Write)
```yaml
aclRules:
  categories:
    - "+@read"
    - "+@write"
    - "-@dangerous"
  keys:
    - "~app:*"
    - "~cache:*"
  commands:
    - "-flushdb"
    - "-flushall"
    - "-shutdown"
```

#### Admin User
```yaml
aclRules:
  categories:
    - "+@all"
  keys:
    - "~*"
```

---

## TLS/SSL Encryption

Redguard supports TLS encryption for secure Redis connections.

### Features

- **Encryption in Transit**: All Redis traffic encrypted with TLS
- **Certificate Management**: Uses Kubernetes Secrets for certificate storage
- **Mutual TLS**: Optional client certificate verification
- **CA Support**: Custom Certificate Authority support

### TLS Configuration

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: redis-cluster-tls
spec:
  tls:
    enabled: true
    certificateSecretRef: redis-tls-cert
    caSecretRef: redis-ca-cert
    mutualTLS: true
  # ... rest of config
```

### Setting Up TLS

#### 1. Generate Certificates

Using OpenSSL:
```bash
# Generate CA
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 -out ca.crt

# Generate server certificate
openssl genrsa -out tls.key 2048
openssl req -new -key tls.key -out tls.csr
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 365 -sha256

# For production, use cert-manager or your PKI
```

#### 2. Create Secrets

```bash
# TLS certificate
kubectl create secret tls redis-tls-cert \
  --cert=tls.crt \
  --key=tls.key

# CA certificate
kubectl create secret generic redis-ca-cert \
  --from-file=ca.crt
```

#### 3. Deploy with TLS

```bash
kubectl apply -f redis-sentinel-tls.yaml
```

### Connecting with TLS

```bash
# Using redis-cli
redis-cli --tls \
  --cert ./tls.crt \
  --key ./tls.key \
  --cacert ./ca.crt \
  -h redis-cluster-redis-0 \
  -p 6379
```

---

## Backup & Restore

Redguard provides automated backups to S3-compatible storage.

### Features

- **Scheduled Backups**: Cron-based automatic backups
- **S3 Compatible**: Works with AWS S3, MinIO, DigitalOcean Spaces, etc.
- **Retention Policies**: Automatic cleanup of old backups
- **Compression**: Gzip compression support
- **IAM Role Support**: Use IRSA on EKS or similar
- **One-time Backups**: Manual backup on-demand

### RedisBackup CRD

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: redis-daily-backup
spec:
  redisClusterRef: redis-cluster
  schedule: "0 2 * * *"  # Daily at 2 AM
  s3:
    bucket: my-redis-backups
    region: us-east-1
    prefix: redis-cluster/backups
    credentialsSecretRef: aws-s3-creds
  retentionPolicy: 7
  compression: true
```

### Backup Schedules

Use cron expressions for scheduling:

| Schedule | Description |
|----------|-------------|
| `0 2 * * *` | Daily at 2 AM |
| `0 */6 * * *` | Every 6 hours |
| `0 0 * * 0` | Weekly on Sunday |
| `0 0 1 * *` | Monthly on the 1st |
| `*/30 * * * *` | Every 30 minutes |

### Setting Up Backups

#### 1. Create S3 Credentials Secret

```bash
kubectl create secret generic aws-s3-creds \
  --from-literal=accessKeyId=YOUR_ACCESS_KEY \
  --from-literal=secretAccessKey=YOUR_SECRET_KEY
```

#### 2. Create RedisBackup

```bash
kubectl apply -f redisbackup.yaml
```

#### 3. Monitor Backups

```bash
# List backups
kubectl get redisbackup

# Check status
kubectl describe redisbackup redis-daily-backup

# View backup history
kubectl get redisbackup redis-daily-backup -o yaml | grep -A 10 status
```

### Using IAM Roles (IRSA on EKS)

```yaml
spec:
  s3:
    bucket: my-redis-backups
    region: us-east-1
    useIAMRole: true  # No credentials needed
```

### One-Time Backup

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: redis-manual-backup
spec:
  redisClusterRef: redis-cluster
  # No schedule = one-time backup
  s3:
    bucket: my-redis-backups
    region: us-east-1
  retentionPolicy: 30
  compression: true
```

### Restore from Backup

**Note**: Restore functionality is implemented through manual RDB file restoration.

1. Download backup from S3:
```bash
aws s3 cp s3://my-redis-backups/redis-cluster/backups/dump-2026-01-09.rdb.gz ./
gunzip dump-2026-01-09.rdb.gz
```

2. Copy to Redis pod:
```bash
kubectl cp dump-2026-01-09.rdb redis-cluster-redis-0:/data/dump.rdb
```

3. Restart Redis:
```bash
kubectl delete pod redis-cluster-redis-0
```

---

## Examples

### Complete Production Setup

```yaml
# 1. Create secrets
kubectl create secret generic redis-password --from-literal=password=prod-password
kubectl create secret tls redis-tls-cert --cert=tls.crt --key=tls.key
kubectl create secret generic redis-ca-cert --from-file=ca.crt
kubectl create secret generic aws-s3-creds \
  --from-literal=accessKeyId=xxx \
  --from-literal=secretAccessKey=yyy

# 2. Deploy RedisSentinel with TLS and Auth
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: prod-redis
spec:
  redisConfig:
    replicas: 3
    storage:
      size: 10Gi
    auth:
      secretName: redis-password
  sentinelConfig:
    replicas: 3
    quorum: 2
  tls:
    enabled: true
    certificateSecretRef: redis-tls-cert
    caSecretRef: redis-ca-cert
    mutualTLS: true
EOF

# 3. Create admin user
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: admin
spec:
  redisClusterRef: prod-redis
  username: admin
  passwordSecretRef: admin-password
  aclRules:
    categories:
      - "+@all"
    keys:
      - "~*"
EOF

# 4. Create app user
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisUser
metadata:
  name: app-user
spec:
  redisClusterRef: prod-redis
  username: appuser
  passwordSecretRef: app-user-password
  aclRules:
    categories:
      - "+@read"
      - "+@write"
      - "-@dangerous"
    keys:
      - "~app:*"
EOF

# 5. Setup daily backups
kubectl apply -f - <<EOF
apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: prod-backup
spec:
  redisClusterRef: prod-redis
  schedule: "0 2 * * *"
  s3:
    bucket: prod-redis-backups
    region: us-east-1
    prefix: prod/backups
    credentialsSecretRef: aws-s3-creds
  retentionPolicy: 30
  compression: true
EOF
```

### Monitoring

Check all resources:
```bash
kubectl get redissentinel,redisuser,redisbackup
```

Output:
```
NAME                                    PHASE     MASTER             REPLICAS   SENTINELS   AGE
redissentinel.redis.redguard.io/prod-redis   Running   10.244.0.30:6379   3          3           5m

NAME                              USERNAME   CLUSTER      ENABLED   PHASE   AGE
redisuser.redis.redguard.io/admin        admin      prod-redis   true      Active  3m
redisuser.redis.redguard.io/app-user     appuser    prod-redis   true      Active  2m

NAME                                       CLUSTER      SCHEDULE      PHASE       LAST BACKUP   AGE
redisbackup.redis.redguard.io/prod-backup  prod-redis   0 2 * * *     Scheduled                 1m
```

---

## Best Practices

### Security

1. **Always use authentication** in production
2. **Enable TLS** for encryption in transit
3. **Use ACLs** to limit user permissions
4. **Rotate passwords** regularly
5. **Use IAM roles** instead of static credentials when possible

### Backups

1. **Test restores** regularly
2. **Store backups** in a different region
3. **Enable compression** to save costs
4. **Set appropriate retention** based on compliance requirements
5. **Monitor backup status** and set up alerts

### High Availability

1. **Use 3+ Sentinel instances** for quorum
2. **Run 3+ Redis replicas** for redundancy
3. **Set appropriate failover timeouts** for your workload
4. **Monitor master changes** and investigate frequent failovers
5. **Use persistent storage** for data durability

---

## Troubleshooting

### ACL Issues

```bash
# Check user status
kubectl get redisuser app-user -o yaml

# Test ACL from Redis
kubectl exec redis-cluster-redis-0 -- redis-cli ACL LIST

# Check applied ACL rules
kubectl exec redis-cluster-redis-0 -- redis-cli ACL GETUSER app-user
```

### TLS Issues

```bash
# Verify certificate secrets
kubectl get secret redis-tls-cert redis-ca-cert

# Check Redis TLS configuration
kubectl exec redis-cluster-redis-0 -- redis-cli CONFIG GET tls-*

# Test TLS connection
kubectl exec redis-cluster-redis-0 -- \
  redis-cli --tls --cert /etc/tls/tls.crt --key /etc/tls/tls.key ping
```

### Backup Issues

```bash
# Check backup status
kubectl describe redisbackup redis-daily-backup

# View backup job logs
kubectl logs -l app=redis-backup,backup=redis-daily-backup

# Manually trigger backup (for testing)
kubectl delete redisbackup test-backup
kubectl apply -f test-backup.yaml
```

---

## Migration Guide

### Migrating Existing Redis to Redguard

1. **Backup existing data**
2. **Deploy Redguard cluster**
3. **Import data** using redis-cli or RDB files
4. **Update applications** to use new endpoints
5. **Enable TLS and ACLs** incrementally

### Upgrading Redguard

1. **Backup data** before upgrade
2. **Update CRDs**: `kubectl apply -f crds/`
3. **Upgrade operator**: `helm upgrade redguard ./helm/redguard`
4. **Monitor pods**: `kubectl get pods -w`
5. **Verify functionality**: Run tests

---

For more information, see:
- [Main README](./README.md)
- [Quick Start Guide](./QUICKSTART.md)
- [API Reference](./api/v1alpha1/)
