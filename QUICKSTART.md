# Redguard Quick Start Guide

This guide will help you deploy and test Redguard locally using Kind and Helm.

## Prerequisites

- Docker Desktop running
- Kind installed (`brew install kind`)
- Kubectl installed (`brew install kubectl`)
- Helm installed (`brew install helm`)

## 🚀 Quick Deploy

### Automated Test (Recommended)

```bash
# Run the automated test script
./test-local.sh
```

This script will:
1. ✅ Check Docker is running
2. ✅ Create Kind cluster
3. ✅ Build operator image
4. ✅ Load image to Kind
5. ✅ Install operator with Helm
6. ✅ Deploy sample RedisSentinel
7. ✅ Wait for everything to be ready

### Manual Steps

If you prefer manual control:

#### 1. Start Docker Desktop
```bash
open -a Docker
```

#### 2. Create Kind Cluster
```bash
kind create cluster --name redguard-test
kubectl cluster-info
```

#### 3. Build & Load Image
```bash
# Build operator image
make docker-build IMG=redguard:v0.1.0

# Load to Kind cluster
kind load docker-image redguard:v0.1.0 --name redguard-test
```

#### 4. Install with Helm
```bash
helm install redguard ./helm/redguard \
  --values ./helm/redguard/values-local.yaml \
  --namespace redguard-system \
  --create-namespace \
  --wait
```

#### 5. Deploy Redis Sentinel
```bash
kubectl apply -f config/samples/redis_v1alpha1_redissentinel.yaml
```

## 📊 Monitoring

### Watch Resources
```bash
# Watch RedisSentinel CR
kubectl get redissentinel -w

# Watch all pods
kubectl get pods -w
```

### Check Status
```bash
# RedisSentinel status
kubectl get redissentinel redis-cluster
kubectl describe redissentinel redis-cluster

# Operator logs
kubectl logs -n redguard-system -l control-plane=controller-manager -f

# Redis pods
kubectl get pods -l app.kubernetes.io/component=redis

# Sentinel pods
kubectl get pods -l app.kubernetes.io/component=sentinel
```

### Expected Output
```bash
$ kubectl get redissentinel redis-cluster

NAME            PHASE     MASTER                  REPLICAS   SENTINELS   AGE
redis-cluster   Running   10.244.0.5:6379        3          3           2m
```

## 🔍 Testing

### Connect to Redis
```bash
# Connect to Redis master
kubectl exec -it redis-cluster-redis-0 -- redis-cli

# Try some commands
127.0.0.1:6379> SET mykey "Hello Redguard"
OK
127.0.0.1:6379> GET mykey
"Hello Redguard"
127.0.0.1:6379> exit
```

### Check Sentinel
```bash
# Connect to Sentinel
kubectl exec -it redis-cluster-sentinel-0 -- redis-cli -p 26379

# Check master info
127.0.0.1:26379> SENTINEL masters
# ... master info ...

127.0.0.1:26379> SENTINEL get-master-addr-by-name redis-cluster-master
1) "10.244.0.5"
2) "6379"

127.0.0.1:26379> exit
```

### Test Failover (Automated)
```bash
# Run automated failover test
./test-failover.sh
```

This will:
1. Identify current master
2. Write test data
3. Delete master pod to trigger failover
4. Verify new master is elected
5. Verify data persistence

### Manual Failover Test
```bash
# 1. Get current master
kubectl exec redis-cluster-sentinel-0 -- \
  redis-cli -p 26379 SENTINEL get-master-addr-by-name redis-cluster-master

# 2. Write test data
kubectl exec redis-cluster-redis-0 -- redis-cli SET test-key "test-value"

# 3. Delete master pod (force failover)
kubectl delete pod redis-cluster-redis-0 --grace-period=0 --force

# 4. Watch failover happen
kubectl get pods -w

# 5. Check new master (after ~10 seconds)
kubectl exec redis-cluster-sentinel-0 -- \
  redis-cli -p 26379 SENTINEL get-master-addr-by-name redis-cluster-master

# 6. Verify data persisted
kubectl exec redis-cluster-redis-1 -- redis-cli GET test-key
```

## 🔧 Troubleshooting

### Operator Not Starting
```bash
# Check operator logs
kubectl logs -n redguard-system -l control-plane=controller-manager

# Check deployment
kubectl get deployment -n redguard-system
kubectl describe deployment redguard -n redguard-system
```

### Redis Pods Not Ready
```bash
# Check pod logs
kubectl logs redis-cluster-redis-0

# Check events
kubectl get events --sort-by='.lastTimestamp'

# Check PVC
kubectl get pvc
```

### Sentinel Issues
```bash
# Check Sentinel logs
kubectl logs redis-cluster-sentinel-0

# Check Sentinel config
kubectl exec redis-cluster-sentinel-0 -- cat /tmp/sentinel.conf
```

### Image Pull Issues
```bash
# Verify image in Kind
docker exec -it redguard-test-control-plane crictl images | grep redguard

# If missing, reload image
kind load docker-image redguard:v0.1.0 --name redguard-test
```

## 🧹 Cleanup

### Automated Cleanup
```bash
./cleanup-local.sh
```

### Manual Cleanup
```bash
# Delete RedisSentinel
kubectl delete -f config/samples/redis_v1alpha1_redissentinel.yaml

# Uninstall Helm release
helm uninstall redguard -n redguard-system

# Delete Kind cluster
kind delete cluster --name redguard-test
```

## 📝 Next Steps

1. **Customize Configuration**: Edit `config/samples/redis_v1alpha1_redissentinel.yaml`
2. **Add Authentication**: Create a secret and configure `auth.secretName`
3. **Scale Up**: Increase `redisConfig.replicas` and `sentinelConfig.replicas`
4. **Production Deploy**: Build and push to a real registry, deploy to production cluster

## 🐛 Common Issues

| Issue | Solution |
|-------|----------|
| Docker not running | Start Docker Desktop |
| Image not found | Run `kind load docker-image` |
| Pods stuck in Pending | Check PVC storage class |
| Sentinel can't elect master | Wait 10-15 seconds for quorum |
| Operator CrashLoopBackOff | Check RBAC permissions |

## 📚 More Info

- [Main README](./README.md)
- [CRD Reference](./api/v1alpha1/redissentinel_types.go)
- [Helm Chart](./helm/redguard/)
- [Sample CR](./config/samples/)
