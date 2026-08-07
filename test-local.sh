#!/bin/bash
set -e

echo "🚀 Redguard Local Test Script"
echo "=============================="

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check Docker
echo -e "\n${YELLOW}1. Checking Docker...${NC}"
if ! docker ps &> /dev/null; then
    echo -e "${RED}❌ Docker is not running. Please start Docker Desktop first.${NC}"
    exit 1
fi
echo -e "${GREEN}✅ Docker is running${NC}"

# Create Kind cluster
echo -e "\n${YELLOW}2. Creating Kind cluster...${NC}"
if kind get clusters | grep -q "redguard-test"; then
    echo -e "${GREEN}✅ Cluster 'redguard-test' already exists${NC}"
else
    kind create cluster --name redguard-test
    echo -e "${GREEN}✅ Cluster created${NC}"
fi

# Build Docker image
echo -e "\n${YELLOW}3. Building operator image...${NC}"
make docker-build IMG=redguard:v0.1.0
echo -e "${GREEN}✅ Image built${NC}"

# Load image to Kind
echo -e "\n${YELLOW}4. Loading image to Kind cluster...${NC}"
kind load docker-image redguard:v0.1.0 --name redguard-test
echo -e "${GREEN}✅ Image loaded${NC}"

# Install with Helm
echo -e "\n${YELLOW}5. Installing operator with Helm...${NC}"
helm upgrade --install redguard ./helm/redguard \
    --values ./helm/redguard/values-local.yaml \
    --namespace redguard-system \
    --create-namespace \
    --wait
echo -e "${GREEN}✅ Operator installed${NC}"

# Wait for operator
echo -e "\n${YELLOW}6. Waiting for operator to be ready...${NC}"
kubectl wait --for=condition=available --timeout=120s \
    deployment/redguard -n redguard-system
echo -e "${GREEN}✅ Operator is ready${NC}"

# Deploy sample Redis Sentinel
echo -e "\n${YELLOW}7. Deploying sample RedisSentinel...${NC}"
kubectl apply -f config/samples/redis_v1alpha1_redissentinel.yaml
echo -e "${GREEN}✅ RedisSentinel deployed${NC}"

echo -e "\n${GREEN}🎉 Deployment complete!${NC}"
echo -e "\n${YELLOW}Useful commands:${NC}"
echo "  # Watch resources"
echo "  kubectl get redissentinel -w"
echo ""
echo "  # Check operator logs"
echo "  kubectl logs -n redguard-system -l control-plane=controller-manager -f"
echo ""
echo "  # Check Redis pods"
echo "  kubectl get pods -l app.kubernetes.io/name=redguard"
echo ""
echo "  # Connect to Redis"
echo "  kubectl exec -it redis-cluster-redis-0 -- redis-cli"
echo ""
echo "  # Check Sentinel status"
echo "  kubectl exec -it redis-cluster-sentinel-0 -- redis-cli -p 26379 SENTINEL masters"
echo ""
echo "  # Cleanup"
echo "  ./cleanup-local.sh"
