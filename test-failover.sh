#!/bin/bash
set -e

echo "🔄 Redis Sentinel Failover Test"
echo "================================"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

REDIS_NAME="redis-cluster"

# Get current master
echo -e "\n${YELLOW}1. Checking current master...${NC}"
CURRENT_MASTER=$(kubectl exec -it ${REDIS_NAME}-sentinel-0 -- \
    redis-cli -p 26379 SENTINEL get-master-addr-by-name ${REDIS_NAME}-master 2>/dev/null | head -1 | tr -d '\r')

if [ -z "$CURRENT_MASTER" ]; then
    echo -e "${RED}❌ Could not determine current master${NC}"
    exit 1
fi

echo -e "${GREEN}Current master: ${CURRENT_MASTER}${NC}"

# Find master pod
MASTER_POD=""
for i in 0 1 2; do
    POD_NAME="${REDIS_NAME}-redis-${i}"
    POD_IP=$(kubectl get pod $POD_NAME -o jsonpath='{.status.podIP}' 2>/dev/null)
    if [ "$POD_IP" = "$CURRENT_MASTER" ]; then
        MASTER_POD=$POD_NAME
        break
    fi
done

if [ -z "$MASTER_POD" ]; then
    echo -e "${RED}❌ Could not find master pod${NC}"
    exit 1
fi

echo -e "${GREEN}Master pod: ${MASTER_POD}${NC}"

# Write a test key
echo -e "\n${YELLOW}2. Writing test key to master...${NC}"
kubectl exec -it ${MASTER_POD} -- redis-cli SET failover-test "before-failover" > /dev/null
echo -e "${GREEN}✅ Test key written${NC}"

# Get all pod IPs before failover
echo -e "\n${YELLOW}3. Current cluster state:${NC}"
for i in 0 1 2; do
    POD="${REDIS_NAME}-redis-${i}"
    ROLE=$(kubectl exec ${POD} -- redis-cli ROLE 2>/dev/null | head -1)
    IP=$(kubectl get pod $POD -o jsonpath='{.status.podIP}')
    echo -e "  ${BLUE}${POD}${NC}: ${IP} - ${ROLE}"
done

# Delete master pod to trigger failover
echo -e "\n${YELLOW}4. Triggering failover by deleting master pod...${NC}"
echo -e "${RED}⚠️  Deleting ${MASTER_POD}...${NC}"
kubectl delete pod ${MASTER_POD} --grace-period=0 --force

# Wait for pod to be recreated
echo -e "\n${YELLOW}5. Waiting for pod to be recreated...${NC}"
sleep 5
kubectl wait --for=condition=ready --timeout=60s pod/${MASTER_POD}

# Check new master
echo -e "\n${YELLOW}6. Checking new master after failover...${NC}"
sleep 10  # Give Sentinel time to elect new master

NEW_MASTER=$(kubectl exec -it ${REDIS_NAME}-sentinel-0 -- \
    redis-cli -p 26379 SENTINEL get-master-addr-by-name ${REDIS_NAME}-master 2>/dev/null | head -1 | tr -d '\r')

echo -e "${GREEN}New master: ${NEW_MASTER}${NC}"

# Find new master pod
NEW_MASTER_POD=""
for i in 0 1 2; do
    POD_NAME="${REDIS_NAME}-redis-${i}"
    POD_IP=$(kubectl get pod $POD_NAME -o jsonpath='{.status.podIP}' 2>/dev/null)
    if [ "$POD_IP" = "$NEW_MASTER" ]; then
        NEW_MASTER_POD=$POD_NAME
        break
    fi
done

echo -e "${GREEN}New master pod: ${NEW_MASTER_POD}${NC}"

# Verify failover happened
if [ "$CURRENT_MASTER" != "$NEW_MASTER" ]; then
    echo -e "\n${GREEN}✅ FAILOVER SUCCESSFUL!${NC}"
    echo -e "   Old master: ${CURRENT_MASTER} (${MASTER_POD})"
    echo -e "   New master: ${NEW_MASTER} (${NEW_MASTER_POD})"
else
    echo -e "\n${RED}❌ Failover did not occur or same master was re-elected${NC}"
fi

# Verify data persistence
echo -e "\n${YELLOW}7. Verifying data persistence...${NC}"
TEST_VALUE=$(kubectl exec -it ${NEW_MASTER_POD} -- redis-cli GET failover-test 2>/dev/null | tr -d '\r')
if [ "$TEST_VALUE" = "before-failover" ]; then
    echo -e "${GREEN}✅ Data persisted successfully!${NC}"
else
    echo -e "${RED}❌ Data lost during failover${NC}"
fi

# Show final cluster state
echo -e "\n${YELLOW}8. Final cluster state:${NC}"
for i in 0 1 2; do
    POD="${REDIS_NAME}-redis-${i}"
    ROLE=$(kubectl exec ${POD} -- redis-cli ROLE 2>/dev/null | head -1 || echo "not ready")
    IP=$(kubectl get pod $POD -o jsonpath='{.status.podIP}')
    echo -e "  ${BLUE}${POD}${NC}: ${IP} - ${ROLE}"
done

# Check RedisSentinel status
echo -e "\n${YELLOW}9. RedisSentinel CR status:${NC}"
kubectl get redissentinel ${REDIS_NAME} -o jsonpath='{.status}' | jq '.'

echo -e "\n${GREEN}🎉 Failover test complete!${NC}"
