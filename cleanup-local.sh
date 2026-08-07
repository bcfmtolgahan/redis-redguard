#!/bin/bash
set -e

echo "🧹 Cleaning up Redguard test environment"
echo "========================================"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Delete RedisSentinel CR
echo -e "\n${YELLOW}1. Deleting RedisSentinel resources...${NC}"
kubectl delete -f config/samples/redis_v1alpha1_redissentinel.yaml --ignore-not-found=true
echo -e "${GREEN}✅ RedisSentinel deleted${NC}"

# Uninstall Helm release
echo -e "\n${YELLOW}2. Uninstalling Helm release...${NC}"
helm uninstall redguard -n redguard-system --ignore-not-found
echo -e "${GREEN}✅ Helm release uninstalled${NC}"

# Delete namespace
echo -e "\n${YELLOW}3. Deleting namespace...${NC}"
kubectl delete namespace redguard-system --ignore-not-found=true
echo -e "${GREEN}✅ Namespace deleted${NC}"

# Delete Kind cluster
echo -e "\n${YELLOW}4. Deleting Kind cluster...${NC}"
read -p "Do you want to delete the Kind cluster 'redguard-test'? (y/N): " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    kind delete cluster --name redguard-test
    echo -e "${GREEN}✅ Cluster deleted${NC}"
else
    echo -e "${YELLOW}⏭️  Cluster kept${NC}"
fi

echo -e "\n${GREEN}🎉 Cleanup complete!${NC}"
