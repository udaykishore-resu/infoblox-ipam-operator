#!/usr/bin/env bash
# scripts/kind-demo.sh — end-to-end demo on a local kind cluster: builds the
# operator and mock-infoblox images, loads them into kind, installs the CRD
# and RBAC, deploys both, applies a sample IPSpaceClaim, and shows it get
# reconciled to Bound with a real allocated CIDR — all without touching a
# real Infoblox account.
#
# Requires: docker, kind, kubectl. Not run inside the sandboxed environment
# this repo was scaffolded in (no docker/kind available there) — run this on
# your own machine.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-infoblox-ipam-demo}"
NAMESPACE="payments"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
need docker
need kind
need kubectl

echo "▶ building images"
docker build -t infoblox-ipam-operator:demo -f Dockerfile --target operator .
docker build -t mock-infoblox:demo -f Dockerfile --target mock-infoblox .

if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  echo "▶ creating kind cluster '${CLUSTER_NAME}'"
  kind create cluster --name "${CLUSTER_NAME}"
else
  echo "▶ reusing existing kind cluster '${CLUSTER_NAME}'"
fi

echo "▶ loading images into kind"
kind load docker-image infoblox-ipam-operator:demo --name "${CLUSTER_NAME}"
kind load docker-image mock-infoblox:demo --name "${CLUSTER_NAME}"

echo "▶ installing CRD + RBAC"
kubectl apply -f config/crd/ipspaceclaims.yaml
kubectl apply -f config/rbac/role.yaml

echo "▶ deploying mock-infoblox in-cluster"
kubectl create namespace infoblox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-infoblox
  namespace: infoblox-system
spec:
  replicas: 1
  selector:
    matchLabels: {app: mock-infoblox}
  template:
    metadata:
      labels: {app: mock-infoblox}
    spec:
      containers:
        - name: mock-infoblox
          image: mock-infoblox:demo
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 9090}]
---
apiVersion: v1
kind: Service
metadata:
  name: mock-infoblox
  namespace: infoblox-system
spec:
  selector: {app: mock-infoblox}
  ports: [{port: 9090, targetPort: 9090}]
EOF

echo "▶ deploying infoblox-ipam-operator"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: infoblox-credentials
  namespace: infoblox-system
stringData:
  INFOBLOX_API_TOKEN: demo-token
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: infoblox-ipam-operator
  namespace: infoblox-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: infoblox-ipam-operator-binding
subjects:
  - kind: ServiceAccount
    name: infoblox-ipam-operator
    namespace: infoblox-system
roleRef:
  kind: ClusterRole
  name: infoblox-ipam-operator-role
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: infoblox-ipam-operator
  namespace: infoblox-system
spec:
  replicas: 1
  selector:
    matchLabels: {app: infoblox-ipam-operator}
  template:
    metadata:
      labels: {app: infoblox-ipam-operator}
    spec:
      serviceAccountName: infoblox-ipam-operator
      containers:
        - name: operator
          image: infoblox-ipam-operator:demo
          imagePullPolicy: IfNotPresent
          args:
            - --infoblox-base-url=http://mock-infoblox.infoblox-system.svc:9090
            - --leader-elect=false
          envFrom:
            - secretRef: {name: infoblox-credentials}
EOF

echo "▶ waiting for operator rollout"
kubectl -n infoblox-system rollout status deployment/infoblox-ipam-operator --timeout=90s

kubectl create namespace network-infra --dry-run=client -o yaml | kubectl apply -f -
echo "▶ applying sample IPSpaceClaim"
kubectl apply -f config/samples/ipspaceclaim_sample.yaml

echo "▶ waiting for it to bind..."
for i in $(seq 1 20); do
  PHASE=$(kubectl -n "${NAMESPACE}" get ipspaceclaim checkout-service-block -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [ "$PHASE" = "Bound" ]; then break; fi
  sleep 2
done

echo
echo "▶ result:"
kubectl get ipspaceclaims -A -o wide

echo
echo "Tear down with: kind delete cluster --name ${CLUSTER_NAME}"
