# Redguard - Redis Sentinel Operator

Redguard is a Kubernetes-native operator for managing highly available Redis clusters with Sentinel for automatic failover.

## Overview

Redguard simplifies Redis deployment and management on Kubernetes by automating:
- Redis master-replica setup with automatic failover
- Sentinel deployment for high availability
- Persistent storage and configuration management
- User access control with Redis ACL
- Automated backups to S3-compatible storage
- Health monitoring and metrics export

## Features

### Core Features
- **High Availability**: Automatic failover with Redis Sentinel
- **Kubernetes Native**: Custom Resource Definitions for declarative management
- **Persistent Storage**: StatefulSet-based deployment with PVC support
- **Custom Configuration**: Support for Redis and Sentinel configuration parameters
- **Resource Management**: Configurable CPU and memory limits
- **Service Types**: ClusterIP, NodePort, and LoadBalancer support

### Security
- **TLS/SSL Encryption**: Secure Redis connections with certificate-based authentication
- **ACL Support**: Fine-grained access control with Redis 6+ ACLs
- **RedisUser CRD**: Declarative user and permission management
- **Mutual TLS**: Optional client certificate verification
- **Secret Management**: Password storage via Kubernetes Secrets

### Backup & Recovery
- **S3 Backups**: Automated backups to AWS S3 and S3-compatible storage (MinIO, DigitalOcean Spaces)
- **RedisBackup CRD**: Declarative backup configuration
- **Scheduled Backups**: Cron-based automatic backups
- **Retention Policies**: Automatic cleanup of old backups
- **Compression**: Gzip compression support
- **IAM Role Support**: IRSA on EKS for secure credential management

### Observability
- **Prometheus Metrics**: Built-in metrics for cluster health, replication lag, and backup status
- **Status Reporting**: Real-time status updates for all resources
- **Event Recording**: Kubernetes events for important operations

## Installation

### Prerequisites
- Kubernetes 1.20+
- kubectl configured
- Helm 3+

### Install with Helm

```bash
# Add Helm repository
helm repo add redguard https://bcfmtolgahan.github.io/redis-redguard
helm repo update

# Install the operator
helm install redguard redguard/redguard \
  --namespace redguard-system \
  --create-namespace

# Verify installation
kubectl get pods -n redguard-system
```


## Quick Start

### Create a Redis Cluster

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
  sentinelConfig:
    replicas: 3
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000
  serviceType: ClusterIP
```

Apply the configuration:

```bash
kubectl apply -f redis-cluster.yaml

# Check status
kubectl get redissentinel my-redis
kubectl get pods -l redis.redguard.io/redis-name=my-redis
```

### Connect to Redis

```bash
# Port-forward to Redis service
kubectl port-forward svc/my-redis-redis 6379:6379

# Connect with redis-cli
redis-cli -h localhost
```

## Custom Resources

### RedisSentinel

Manages Redis cluster with Sentinel for high availability. Supports custom configuration, TLS, authentication, and resource limits.

**Example with Authentication:**

```yaml
apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: prod-redis
spec:
  redisConfig:
    replicas: 5
    auth:
      secretName: redis-password
    customConfig:
      maxmemory: "1gb"
      maxmemory-policy: "allkeys-lru"
  sentinelConfig:
    replicas: 5
    quorum: 3
```

Create password secret:

```bash
kubectl create secret generic redis-password \
  --from-literal=password=mysecurepassword
```

### RedisUser

Manages Redis ACL users with fine-grained permissions.

**Example:**

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
    keys: ["~app:*"]
    channels: ["&notifications:*"]
```

### RedisBackup

Automated S3 backups with scheduling and retention policies.

**Example:**

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
    credentialsSecretRef: s3-credentials
  retentionPolicy: 7
  compression: true
```

## Monitoring

The operator exposes Prometheus metrics on port 8080:

```bash
# Port-forward metrics service
kubectl port-forward -n redguard-system \
  svc/redguard-controller-manager-metrics-service 8080:8080

# Query metrics
curl http://localhost:8080/metrics | grep redis_
```

**Available Metrics:**
- `redis_cluster_info` - Cluster status
- `redis_connected_replicas` - Number of connected replicas
- `redis_replication_lag_seconds` - Replication lag per pod
- `redis_sentinel_status` - Sentinel health
- `redis_failover_total` - Total failover count
- `redis_backup_status` - Backup success/failure status
- `redis_backup_duration_seconds` - Backup duration
- `redis_user_acl_status` - ACL user status

## Architecture

Redguard creates the following Kubernetes resources:

- **StatefulSet (Redis)**: Master and replica pods with persistent storage
- **StatefulSet (Sentinel)**: Sentinel pods for monitoring and failover
- **Services**: Headless and ClusterIP services for pod discovery
- **ConfigMaps**: Redis and Sentinel configuration
- **PersistentVolumeClaims**: Storage for Redis data

**Failover Process:**
1. Sentinel detects master failure
2. Quorum of Sentinels agree on failure
3. New master is elected from healthy replicas
4. Replicas reconfigured to follow new master
5. Operator updates status with new master information

## Examples

Additional configuration examples are available in [config/samples/](./config/samples/):
- [Basic Redis cluster](./config/samples/redis_v1alpha1_redissentinel.yaml)
- [Redis with TLS](./config/samples/redis_v1alpha1_redissentinel_tls.yaml)
- [Redis User ACL](./config/samples/redis_v1alpha1_redisuser.yaml)
- [Redis Backup](./config/samples/redis_v1alpha1_redisbackup.yaml)

## Best Practices

For production deployments:
- Use at least 3 Redis replicas
- Use at least 3 Sentinel replicas (odd number)
- Set appropriate quorum (typically N/2 + 1)
- Enable authentication with strong passwords
- Use TLS for production environments
- Configure resource limits
- Use fast SSD storage
- Set up automated backups with retention policies
- Monitor with Prometheus and Grafana
- Test failover scenarios regularly

## Uninstalling

To remove the operator and all resources:

```bash
# Delete all Redis instances first
kubectl delete redissentinel --all --all-namespaces

# Uninstall the operator
helm uninstall redguard -n redguard-system

# Delete CRDs (optional - this will delete all custom resources)
kubectl delete crd redissentinels.redis.redguard.io
kubectl delete crd redisusers.redis.redguard.io
kubectl delete crd redisbackups.redis.redguard.io
```

## Contributing

Contributions are welcome. Please submit issues and pull requests to the repository.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
