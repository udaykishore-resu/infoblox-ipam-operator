#!/usr/bin/env bash
# demo/demo.sh — a self-contained, no-cluster-required walkthrough of the
# operator's core lifecycle, run against the in-memory mock-infoblox server.
# This is the fastest way to see the request/response shapes and the drift
# scenario without standing up a real Kubernetes cluster or Infoblox account.
#
# For the full in-cluster demo (kubectl apply -> real IPSpaceClaim CRD ->
# controller reconciling against this same mock server), see scripts/kind-demo.sh.
set -euo pipefail

PORT="${MOCK_INFOBLOX_PORT:-9090}"
BASE="http://localhost:${PORT}"
TOKEN="demo-token"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
need curl
need jq

echo "▶ building mock-infoblox"
go build -o /tmp/mock-infoblox ./cmd/mock-infoblox

echo "▶ starting mock-infoblox on :${PORT}"
MOCK_INFOBLOX_ADDR=":${PORT}" /tmp/mock-infoblox &
MOCK_PID=$!
trap 'kill $MOCK_PID 2>/dev/null || true' EXIT
sleep 1

hr() { printf '\n%s\n' "----------------------------------------------------------------"; }

hr
echo "1) Allocate a /28 from IP space 'prod-eks-us-east-1' (next-available)"
echo "   — this is exactly the call the operator makes when a new"
echo "     IPSpaceClaim with cidrSize: 28 is reconciled for the first time."
RESP=$(curl -s -X POST "${BASE}/api/ddi/v1/ipam/address_block" \
  -H "Authorization: Token ${TOKEN}" -H "Content-Type: application/json" \
  -d '{"space":"prod-eks-us-east-1","cidr":28,"next_available":true,"tags":{"team":"payments-platform","k8s-namespace":"payments","k8s-name":"checkout-service-block"},"comment":"managed by infoblox-ipam-operator: payments/checkout-service-block"}')
echo "$RESP" | jq .
REF=$(echo "$RESP" | jq -r .id)

hr
echo "2) Fetch it back by ref (this is the periodic drift-check poll)"
curl -s "${BASE}/api/ddi/v1/${REF}" -H "Authorization: Token ${TOKEN}" | jq .

hr
echo "3) Simulate a network engineer deleting the allocation directly in"
echo "   the Infoblox portal — outside of Kubernetes entirely."
SHORT="${REF#ipam/address_block/}"
curl -s -o /dev/null -w "   -> DELETE /admin/blocks/${SHORT}: HTTP %{http_code}\n" \
  -X DELETE "${BASE}/admin/blocks/${SHORT}"

hr
echo "4) Operator's next drift-check GET now 404s — this is exactly what"
echo "   flips IPSpaceClaim.status.phase to 'Drifted' with condition"
echo "   DriftDetected=True, reason=AllocationMissing."
curl -s -o /dev/null -w "   -> GET  /api/ddi/v1/${REF}: HTTP %{http_code}\n" \
  "${BASE}/api/ddi/v1/${REF}"

hr
echo "5) Allocate + release, showing the finalizer-driven cleanup path is"
echo "   idempotent (safe to retry if the first release attempt fails)."
RESP2=$(curl -s -X POST "${BASE}/api/ddi/v1/ipam/address_block" \
  -H "Authorization: Token ${TOKEN}" -H "Content-Type: application/json" \
  -d '{"space":"prod-eks-us-east-1","cidr":28,"next_available":true,"comment":"release-demo"}')
REF2=$(echo "$RESP2" | jq -r .id)
echo "   allocated: $(echo "$RESP2" | jq -c '{id,address,cidr}')"
curl -s -o /dev/null -w "   -> DELETE ${REF2} (1st release): HTTP %{http_code}\n" \
  -X DELETE "${BASE}/api/ddi/v1/${REF2}" -H "Authorization: Token ${TOKEN}"
curl -s -o /dev/null -w "   -> DELETE ${REF2} (2nd release, idempotent): HTTP %{http_code} (404 is expected+handled, not an error)\n" \
  -X DELETE "${BASE}/api/ddi/v1/${REF2}" -H "Authorization: Token ${TOKEN}"

hr
echo "Done. mock-infoblox will stop now (trap on exit)."
