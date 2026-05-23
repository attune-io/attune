#!/usr/bin/env bash
# hack/demo.sh - Interactive demo of kube-rightsize on a local k3d cluster.
#
# This script provisions a cluster, deploys the operator, creates an
# over-provisioned workload, applies a RightSizePolicy in Recommend mode,
# waits for recommendations to appear, then promotes to Auto mode and
# shows the in-place resize happening.
#
# Prerequisites:
#   - Docker
#   - k3d (>= 5.x)
#   - kubectl
#   - helm (>= 3.x)
#
# Usage:
#   ./hack/demo.sh          # full demo (creates + tears down cluster)
#   ./hack/demo.sh --skip-cluster  # reuse existing cluster
#   ./hack/demo.sh --no-cleanup    # keep cluster after demo
#
# Designed for recording with asciinema or VHS (Charm).

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────
CLUSTER_NAME="kube-rightsize-demo"
K3S_VERSION="v1.35.4-k3s1"
IMG="kube-rightsize:demo"
SKIP_CLUSTER="${SKIP_CLUSTER:-false}"
NO_CLEANUP="${NO_CLEANUP:-false}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Parse flags.
for arg in "$@"; do
  case "$arg" in
    --skip-cluster) SKIP_CLUSTER=true ;;
    --no-cleanup)   NO_CLEANUP=true ;;
    --help|-h)
      sed -n '2,/^$/s/^# //p' "$0"
      exit 0
      ;;
  esac
done

# ── Helpers ──────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
RESET='\033[0m'

banner() { echo -e "\n${CYAN}${BOLD}═══ $1 ═══${RESET}\n"; }
info()   { echo -e "${GREEN}▸${RESET} $1"; }
warn()   { echo -e "${YELLOW}▸${RESET} $1"; }
run()    { echo -e "${BOLD}\$ $*${RESET}"; "$@"; }

# Wait for a condition with a spinner.
wait_for() {
  local msg="$1"; shift
  local timeout="${1:-120}"; shift || true
  echo -ne "${GREEN}▸${RESET} Waiting: ${msg}..."
  local elapsed=0
  while ! eval "$@" >/dev/null 2>&1; do
    sleep 2
    elapsed=$((elapsed + 2))
    if [ "$elapsed" -ge "$timeout" ]; then
      echo " TIMEOUT"
      return 1
    fi
    echo -ne "."
  done
  echo " done"
}

cleanup() {
  if [ "$NO_CLEANUP" = "true" ]; then
    warn "Cluster '$CLUSTER_NAME' left running (--no-cleanup). Delete with: k3d cluster delete $CLUSTER_NAME"
    return
  fi
  banner "Cleanup"
  info "Deleting cluster '$CLUSTER_NAME'..."
  k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# ── Step 1: Create cluster ───────────────────────────────────────────────────
banner "Step 1: Create Kubernetes cluster"

if [ "$SKIP_CLUSTER" = "true" ]; then
  info "Reusing existing cluster (--skip-cluster)"
else
  info "Creating k3d cluster '$CLUSTER_NAME' with K3s $K3S_VERSION..."
  k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
  run k3d cluster create "$CLUSTER_NAME" \
    --image "rancher/k3s:$K3S_VERSION" \
    --k3s-arg "--disable=traefik,servicelb@server:*" \
    --wait --timeout 120s
fi

run kubectl cluster-info
echo

# ── Step 2: Install prerequisites ───────────────────────────────────────────
banner "Step 2: Install cert-manager and Prometheus"

info "Installing cert-manager..."
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.17.2/cert-manager.yaml >/dev/null
wait_for "cert-manager webhook ready" 120 \
  "kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=5s"

info "Installing Prometheus..."
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
helm repo update >/dev/null
helm install prometheus prometheus-community/prometheus \
  --namespace monitoring --create-namespace \
  --set server.persistentVolume.enabled=false \
  --set alertmanager.enabled=false \
  --set prometheus-pushgateway.enabled=false \
  --wait --timeout 3m >/dev/null 2>&1 || true

wait_for "Prometheus server ready" 180 \
  "kubectl wait --for=condition=Available deployment/prometheus-server -n monitoring --timeout=5s"
echo

# ── Step 3: Build and deploy kube-rightsize ──────────────────────────────────
banner "Step 3: Build and deploy kube-rightsize"

cd "$REPO_ROOT"
info "Building operator image..."
run make docker-build IMG="$IMG" 2>/dev/null

info "Loading image into cluster..."
k3d image import "$IMG" -c "$CLUSTER_NAME"

info "Installing via Helm..."
helm install kube-rightsize ./charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace \
  --set image.repository=kube-rightsize \
  --set image.tag=demo \
  --set image.pullPolicy=Never \
  --set webhooks.enabled=true \
  --set metrics.enabled=true \
  --wait --timeout 2m >/dev/null

wait_for "operator ready" 60 \
  "kubectl wait --for=condition=Available deployment/kube-rightsize-controller-manager -n kube-rightsize-system --timeout=5s"
run kubectl get pods -n kube-rightsize-system
echo

# ── Step 4: Deploy an over-provisioned workload ─────────────────────────────
banner "Step 4: Deploy an over-provisioned workload"

info "Creating demo workload: 3 replicas, 500m CPU / 512Mi memory requested"
info "(Actual usage will be ~100m CPU / ~80Mi memory -- 80% over-provisioned)"
run kubectl apply -f hack/demo-workload.yaml

wait_for "all pods running" 120 \
  "kubectl wait --for=condition=Ready pod -l app=web-api -n demo-app --timeout=5s"
run kubectl get pods -n demo-app
echo

info "Current resource allocation:"
run kubectl get pods -n demo-app -o custom-columns=\
'NAME:.metadata.name,CPU_REQ:.spec.containers[0].resources.requests.cpu,MEM_REQ:.spec.containers[0].resources.requests.memory'
echo

# ── Step 5: Create a RightSizePolicy in Recommend mode ──────────────────────
banner "Step 5: Create a RightSizePolicy (Recommend mode)"

info "Applying a policy with fast demo settings..."
info "(minimumDataPoints: 3, historyWindow: 15m, reconcileInterval: 30s)"

cat <<'EOF' | kubectl apply -f -
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: web-api
  namespace: demo-app
spec:
  targetRef:
    kind: Deployment
    name: web-api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  cpu:
    percentile: 95
    overhead: "20"
    bounds:
      min: "50m"
      max: "2000m"
  memory:
    percentile: 99
    overhead: "30"
    bounds:
      min: "64Mi"
      max: "4Gi"
  confidence:
    minimumDataPoints: 3
  historyWindow: 15m
  updateStrategy:
    mode: Recommend
    reconcileInterval: 30s
EOF
echo

# ── Step 6: Wait for recommendations ────────────────────────────────────────
banner "Step 6: Waiting for recommendations"

info "The operator is querying Prometheus for CPU and memory usage data."
info "With minimumDataPoints: 3 and 15m history window, this takes ~2-3 minutes."
echo

ATTEMPTS=0
MAX_ATTEMPTS=30
while [ "$ATTEMPTS" -lt "$MAX_ATTEMPTS" ]; do
  RECS=$(kubectl get rightsizepolicies web-api -n demo-app -o jsonpath='{.status.recommendations}' 2>/dev/null || echo "")
  if [ -n "$RECS" ] && [ "$RECS" != "null" ] && [ "$RECS" != "[]" ]; then
    break
  fi
  ATTEMPTS=$((ATTEMPTS + 1))
  PHASE=$(kubectl get rightsizepolicies web-api -n demo-app -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
  POINTS=$(kubectl get rightsizepolicies web-api -n demo-app -o jsonpath='{.status.dataPointsCollected}' 2>/dev/null || echo "0")
  echo -ne "\r  Phase: $PHASE | Data points: $POINTS | Attempt $ATTEMPTS/$MAX_ATTEMPTS   "
  sleep 10
done
echo
echo

if [ "$ATTEMPTS" -ge "$MAX_ATTEMPTS" ]; then
  warn "Recommendations did not appear within the timeout."
  warn "This is expected if Prometheus has not yet scraped enough data."
  info "Check status with: kubectl get rightsizepolicies web-api -n demo-app -o yaml"
else
  info "Recommendations are ready!"
  echo
  run kubectl get rightsizepolicies -n demo-app
  echo
  # Show recommendations via the kubectl plugin if available, otherwise via jsonpath.
  if command -v kubectl-rightsize >/dev/null 2>&1; then
    run kubectl rightsize recommendations -n demo-app
    echo
    run kubectl rightsize savings -n demo-app
  else
    info "Recommendation details:"
    kubectl get rightsizepolicies web-api -n demo-app \
      -o jsonpath='{range .status.recommendations[0].containers[*]}  {.containerName}: CPU {.cpu.recommendation} (from {.cpu.current}), Memory {.memory.recommendation} (from {.memory.current}){"\n"}{end}'
    echo
  fi
fi

# ── Step 7: Promote to Auto mode ────────────────────────────────────────────
banner "Step 7: Promote to Auto mode (in-place resize)"

info "Switching from Recommend to Auto mode..."
run kubectl patch rightsizepolicies web-api -n demo-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto","autoRevert":true}}}'
echo

info "Waiting for the operator to resize pods in-place..."
sleep 15

info "Current resource allocation after resize:"
run kubectl get pods -n demo-app -o custom-columns=\
'NAME:.metadata.name,CPU_REQ:.spec.containers[0].resources.requests.cpu,MEM_REQ:.spec.containers[0].resources.requests.memory'
echo

info "Policy status:"
run kubectl get rightsizepolicies -n demo-app
echo

# ── Done ─────────────────────────────────────────────────────────────────────
banner "Demo Complete"

info "kube-rightsize resized all 3 pods in-place without a single restart."
info "No YAML edits. No PRs. No pod restarts."
echo
info "What to explore next:"
info "  kubectl get rightsizepolicies web-api -n demo-app -o yaml  # full status"
info "  kubectl describe pod -n demo-app -l app=web-api            # resize events"
if command -v kubectl-rightsize >/dev/null 2>&1; then
  info "  kubectl rightsize savings -n demo-app                     # dollar savings"
fi
echo