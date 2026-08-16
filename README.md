# infoblox-ipam-operator

A Kubernetes operator that manages IP address allocation declaratively
against **Infoblox Universal DDI**, using a native CRD (`IPSpaceClaim`) and
a continuous reconciliation loop instead of a one-shot `terraform apply`.

Fully demoable with zero external dependencies — a built-in mock of the
Infoblox DDI v1 API means you can run the entire allocate → drift-detect →
release lifecycle locally, in CI, or on a `kind` cluster, without a real
Infoblox account.

```
make demo        # 60-second, no-cluster, curl-based lifecycle walkthrough
make kind-demo    # full in-cluster demo: real CRD, real controller, real reconcile loop
```

---

## 1. The existing system

Infoblox's core product is **DDI** — DNS, DHCP, and IP Address Management —
historically delivered through **NIOS**, an on-prem "Grid" appliance model.
Over the last several years Infoblox has moved this to **BloxOne / Universal
DDI**, a SaaS-managed control plane fronted by a modern REST API
(`csp.infoblox.com/api/ddi/v1/...`) that can allocate IP spaces, address
blocks, and DNS records across hybrid and multi-cloud environments, and is
explicitly marketed with Kubernetes and containerized deployment as
first-class scenarios.

The tooling that exists today to integrate this with Kubernetes and
infrastructure-as-code is:

| Tool | What it does | What it doesn't do |
|---|---|---|
| **`terraform-provider-infoblox`** (infobloxopen) | Manages NIOS host records, IPAM allocations, and DNS as static Terraform resources | No concept of a running cluster's *dynamic* IP demand. Every claim needs a `plan`/`apply` cycle. No reconciliation after apply — if something drifts, Terraform doesn't notice until the next manual run. |
| **Community `external-dns-webhook` for Infoblox** | Syncs Kubernetes Service/Ingress hostnames to Infoblox DNS records | DNS only — no IPAM. Built against the older WAPI surface, not the newer Universal DDI v1 API. |
| **Infoblox WAPI / DDI v1 REST API** (used directly) | Full CRUD on IP spaces, address blocks, DNS, DHCP | It's just an API — nothing reconciles Kubernetes state against it, detects drift, or exposes it as `kubectl`-native objects. |

In practice, teams running Infoblox alongside EKS/GKE bridge this gap by
hand: pre-provisioning address blocks via Terraform ahead of time, then
manually tracking which namespace/team/service consumed which block in a
spreadsheet or wiki page — with no automated reconciliation, and no signal
when someone changes or deletes an allocation directly in the Infoblox
portal.

## 2. The gap

Specifically, there is **no CRD-based, `controller-runtime`-native IPAM
operator for Infoblox** — nothing playing the role that
[Whereabouts](https://github.com/k8snetworkplumbingwg/whereabouts) or
Calico's IPAM plugin play for other IPAM backends, but backed by Infoblox
Universal DDI as the system of record. Concretely, that means:

- **No declarative K8s object** for "give me an address block from this
  Infoblox IP space." Today that's a Terraform resource block, a manual API
  call, or a portal click — never a `kubectl apply`.
- **No drift detection.** If a network engineer edits or deletes an
  allocation directly in the Infoblox UI, nothing in the Kubernetes-facing
  tooling notices. The cluster's belief about its own IP allocations quietly
  goes stale.
- **No safe, automatic release.** Static Terraform state means "delete the
  resource" is a manual, deliberate act — there's no lifecycle tied to the
  Kubernetes objects that actually consume the address, and no guard
  against leaking allocations if something is deleted out of order.

## 3. The solution: `IPSpaceClaim`

This operator introduces one CRD, `IPSpaceClaim`, and a controller that
keeps it in sync with Infoblox Universal DDI on an ongoing basis — not just
at creation time.

```
+-----------------------+        watch           +----------------------------+
|  kubectl apply -f     | ----------------------> |  IPSpaceClaim Controller  |
|  IPSpaceClaim (CRD)   |                         |  (controller-runtime)     |
+-----------------------+                         +--------------+-------------+
                                                                  |
                                      allocate / get / release    | REST (DDI v1)
                                                                  v
                                                    +----------------------------+
                                                    |  Infoblox Universal DDI   |
                                                    |  /api/ddi/v1/ipam/...     |
                                                    +----------------------------+
```

**Lifecycle:**

1. A team declares an `IPSpaceClaim` — either "give me the next available
   `/28` from this IP space" (`cidrSize: 28`) or "pin this specific CIDR I
   already own" (`fixedCIDR: 10.44.12.0/28`, for migrating a pre-existing
   static allocation under operator management).
2. The controller calls Infoblox's DDI v1 API to allocate it, tagging the
   allocation with extensible attributes — owning namespace, claim name,
   team, cost center — so it's traceable from the Infoblox portal back to
   the exact Kubernetes object that requested it.
3. The allocated CIDR and the Infoblox resource ref are written back to
   `.status`. `kubectl get ipspaceclaims` shows the real, live CIDR — not
   just intent.
4. On a bounded interval (default 5 minutes, see `driftRecheckEvery` in
   `internal/controller`), the controller re-fetches the allocation from
   Infoblox and compares it to what's in `.status`. If someone changed or
   deleted it out-of-band, the claim transitions to `Phase: Drifted` with a
   `DriftDetected` condition — visible to `kubectl` and alertable by any
   standard condition-watching tooling.
5. On CRD deletion, a **finalizer** guarantees the Infoblox-side allocation
   is released (or retained, per `reclaimPolicy: Release | Retain` —
   deliberately mirroring the `PersistentVolume` reclaim-policy pattern K8s
   users already know) before Kubernetes is allowed to finish deleting the
   object. This closes the "leaked allocation" failure mode pure Terraform
   workflows are exposed to.

### Example

```yaml
apiVersion: infoblox.udaykishore.dev/v1alpha1
kind: IPSpaceClaim
metadata:
  name: checkout-service-block
  namespace: payments
spec:
  ipSpaceName: prod-eks-us-east-1
  cidrSize: 28
  reclaimPolicy: Release
  tags:
    team: payments-platform
    cost-center: "CC-4471"
```

```
$ kubectl get ipspaceclaims -n payments
NAME                       IPSPACE               CIDR              PHASE
checkout-service-block     prod-eks-us-east-1     10.44.12.0/28     Bound
```

## 4. Design choices, and why

- **Level-triggered reconciliation, not event-triggered scripting.** A
  webhook that only reacts to Kubernetes-side events (like the existing
  `external-dns-webhook`) has no way to notice state that changed for
  reasons *outside* Kubernetes. This operator polls Infoblox specifically to
  catch that class of problem — the same reason Kubernetes controllers in
  general are level-triggered rather than purely event-driven.
- **Finalizers for allocation safety.** IP address space is a scarce,
  shared, auditable resource. The finalizer pattern guarantees
  release-before-delete, the same way K8s guarantees a `PersistentVolume`
  isn't force-deleted while still bound.
- **Extensible attributes as the audit trail.** Every allocation carries the
  owning namespace/claim/team at creation time, so Infoblox portal records
  and `IPSpaceClaim` objects stay cross-referenceable without a separate
  CMDB or spreadsheet.
- **A scoped, hand-written Infoblox client, not a generated SDK.** The
  operator only needs allocate / get / release against one resource type. A
  minimal, fully unit-testable client (zero third-party dependencies — see
  `internal/infoblox`) keeps the surface area, and the audit burden, small.
- **Service-account token via environment variable, never a CLI flag.**
  Keeps the credential out of process listings and Deployment specs;
  designed to be mounted from a Kubernetes `Secret`.
- **A real mock server, not mocked-out unit tests only.** `cmd/mock-infoblox`
  implements the actual DDI v1 request/response shapes in-memory, so the
  entire system — including the controller's reconcile loop — can be
  exercised end-to-end in CI or on a laptop with no network access and no
  Infoblox account. See [section 6](#6-running-the-demo) below.

## 5. What's intentionally out of scope (v1)

- **CNI-chain integration** — a Whereabouts-style plugin so pods themselves
  pull addresses through this path at pod-creation time. This changes the
  trust boundary (kubelet-level access to every pod's network setup) enough
  to warrant its own design pass rather than bolting it onto the claim-level
  controller.
- **Multi-cluster / hub-spoke coordination** across multiple Infoblox Grids
  or Universal DDI accounts.
- **Rate-limit-aware batching** for very large fleets. The current
  drift-check interval is a flat 5 minutes per claim; at scale (thousands of
  claims) this wants jitter and backpressure rather than a fixed interval
  per object.
- **Validating/mutating admission webhooks** for the CRD itself (e.g.
  rejecting a claim that mixes `cidrSize` and `fixedCIDR`) — currently
  enforced in the controller at reconcile time, not at admission time.

## 6. Running the demo

Two levels, depending on how much you want to stand up.

### Level 1 — no cluster required (60 seconds)

Runs the real client code (`internal/infoblox`) against the real mock server
(`cmd/mock-infoblox`) over HTTP, showing the exact request/response shapes
and the drift scenario:

```bash
make demo
```

This will: build `mock-infoblox`, start it on `:9090`, allocate a `/28`,
fetch it back, simulate an out-of-band portal deletion, show the next fetch
404ing (the drift signal), then demonstrate idempotent release. See
`demo/demo.sh` for the exact sequence — every step in that script has been
run and verified during development.

### Level 2 — full in-cluster demo (kind)

Requires `docker`, `kind`, and `kubectl` locally:

```bash
make kind-demo
```

This builds both container images, spins up a `kind` cluster, installs the
`IPSpaceClaim` CRD and RBAC, deploys `mock-infoblox` and the operator
in-cluster (operator pointed at the mock via `--infoblox-base-url`), applies
`config/samples/ipspaceclaim_sample.yaml`, and polls until you see:

```
NAME                       IPSPACE               CIDR              PHASE
checkout-service-block     prod-eks-us-east-1     10.44.12.0/28     Bound
```

Tear down with `make kind-down`.

### Unit tests

```bash
make test
```

`internal/infoblox` has zero third-party dependencies and its tests run
against `net/http/httptest` — no network, no mocking framework, no
Kubernetes cluster required. `internal/controller` tests (using
`controller-runtime`'s fake client / envtest) require the `k8s.io` /
`sigs.k8s.io` modules resolved via `go mod tidy` in an environment with
normal internet egress.

## 7. Production readiness checklist

What's already here, and what a real rollout would add on top:

**Included:**
- Multi-stage, distroless, non-root container image (`Dockerfile`)
- Leader election for HA operator replicas (`--leader-elect`, on by default)
- `/healthz` and `/readyz` probes wired to the manager
- Finalizer-guarded delete path (no orphaned allocations)
- Idempotent release (safe to retry on failure)
- Structured status/conditions (`kubectl get` and `kubectl wait` friendly)
- CI (`.github/workflows/ci.yml`): `gofmt` check, `go vet`, `go test -race`,
  Docker image build on every PR
- Credential handling via env var / K8s `Secret`, not CLI flags

**Would add before a real production rollout:**
- Prometheus metrics for allocation latency, drift-detection failures, and
  Infoblox API error rates (controller-runtime exposes a metrics endpoint
  already; this project doesn't yet add custom metrics on top of it)
- Retry/backoff with jitter on Infoblox API 429/5xx responses, rather than
  the current flat 30-second requeue
- Admission webhook validation for the CRD spec
- `envtest`-based integration tests for the controller package (blocked in
  the sandbox this project was built in by network egress restrictions on
  `k8s.io` module fetches — not a code issue, just an environment
  limitation; works normally with standard internet access)
- Helm chart / Kustomize overlays for install, instead of the raw
  `kubectl apply` shown in `scripts/kind-demo.sh`

## 8. Status

Portfolio / demonstration project, built to address a specific, verifiable
gap in Infoblox's Kubernetes tooling ecosystem — every claim above about
what exists and what doesn't is checkable directly against the
`infobloxopen` GitHub org and the public DDI v1 API docs. Not affiliated
with or endorsed by Infoblox.

## Author

**Udaykishore Resu** — Principal Software Engineer, platform architecture &
distributed systems (Golang, Kubernetes, multi-cloud AWS/GCP).
[github.com/udaykishore-resu](https://github.com/udaykishore-resu) ·
[medium.com/@udaykishoreresu](https://medium.com/@udaykishoreresu)
