# Design 0013 — Edge device connector (device-initiated mesh access)

- Status: **Accepted (initial implementation landed).**
- Packages: `pkg/edge` (pure, OSS), the `EdgeDevice` CRD in
  `networking.datawerx.io` (OSS contract), and the **premium** connector
  `datawerx-premium/edge` (managed terminator + reconciler + injection).
- Builds on: the remote-access gateway role (`pkg/gateway`, `pkg/nat`
  `BuildGatewayMasqRules`, `controllers.GatewayReconciler`), the join bundle
  pattern (`pkg/bootstrap`, design 0006), and the premium injection seam
  (`pkg/agent.Options.RegisterPremium`).

## Summary

Let a single edge device **that is not a Kubernetes node** reach mesh services **by name**
(`*.clusterset.local`) over a tunnel the **device dials outbound**. The mesh
never initiates a connection to the device; the device is free to sit behind
NAT/CGNAT with no inbound ports. This is the "like a VPN, better than
`kubectl port-forward`" access path.

The hard requirement that shapes the whole design: it must work identically
whether the cluster runs the native WireGuard data plane
(`DataWerx_DATAPLANE=wireguard`) or a bring-your-own overlay
(`DataWerx_DATAPLANE=routed`).

The goal here is to 

> **Decouple the device's access transport from the mesh's internal transport.**

The device always brings its **own** WireGuard tunnel, terminated on a dedicated
**edge-ingress** endpoint owned by this connector — *separate from* the node-to-
node mesh data plane. From that terminator the existing gateway role forwards and
masquerades the device into the mesh over whatever routes the node already has
(the native `dwx-mesh0` device, or BYO host routes over your overlay). Because
the device tunnel never touches `dwx-mesh0`, the device-side setup and the
security model are byte-for-byte identical in both modes.

## 0. Tiering (the framing decision)

The **managed edge connector** is a premium feature. It
adds is squarely org-scale, additive work:

- A **managed terminator** (DataWerx provides the `dwx-edge0` WireGuard
  concentrator, so you don't have to stand up and operate your own overlay),
- fleet-wide deterministic addressing with no broker,
- **zero-touch** fleet enrollment (the RFC 8628 `coordinator` device-auth flow),
- **SSO/RBAC/audit + a fleet UI** (the admin website),
- **`nonat` identity-preserving return routing**.

Two things stay **free**:

1. The **`EdgeDevice` CRD** and the pure **`pkg/edge`** planner stay in the open
   core as the **tier-agnostic contract** — exactly how `MeshPeer` is the
   integration point whether it was authored by free GitOps or the premium
   syncer, and how `GatewayReconciler.NoNAT` lives in OSS while the function that
   makes it work (`nonat`) is premium. The terminator, reconciler, and
   enrollment register **only** via `pkg/agent.Options.RegisterPremium`; the
   open-core build wires none of them.
2. The **free BYO-overlay + gateway path** remains a complete, no-cost DIY edge
   connector: put the device on a shared overlay (Tailscale/NetBird/…) and run
   the gateway role (`DataWerx_ROLE=gateway`). The *capability* of edge reach is
   free; the **managed/automated/governed** connector is premium.

The reconcile loop and data plane **never branch on tier.**

## 1. Why the existing architecture is not enough on its own

1. **Roaming `MeshPeer` over native WireGuard.** A roaming peer (empty
   `spec.endpoint`) can dial in, but only in native mode — `routed` mode creates
   no WireGuard device for the device to handshake with — and it conflates a
   *device* with a *cluster peer* (wrong identity model, blast radius, CIDR
   semantics).
2. **The remote-access gateway role.** Already data-plane-agnostic for the
   onward leg (forward + `DWX-GW-MASQ` + access-profile ConfigMap), but it
   deliberately **does not own a WireGuard device or terminate any tunnel** — it
   assumes the device is already on a shared overlay.
3. **The join bundle** (`pkg/bootstrap`). The right *shape* for secret-free,
   validated, single-token enrollment — but cluster↔cluster, and a device is not
   a cluster.

**The missing component is a data-plane-independent edge-ingress terminator.**
This design adds exactly that (premium), wires it to the gateway role (2) for the
onward leg, and borrows the token discipline of (3) for the device artifact.

## 2. Architecture

```
edge device (behind NAT)                          gateway node (a mesh node, any data plane)
────────────────────────                          ───────────────────────────────────────────
wg-quick: [Peer] = gateway:51821                   edge-ingress terminator (premium edge + pkg/wg)
  AllowedIPs = clusterset VIP CIDR + mesh CIDRs      own WireGuard device `dwx-edge0`, listen :51821
  PersistentKeepalive = 25  ───── outbound UDP ────► peer = device pubkey, AllowedIP = device /32
  DNS = gateway clusterset responder                          │ (device source learned from handshake)
                                                              ▼
                                                   IP forward + DWX-GW-MASQ  (gateway role, §1.2)
                                                              │  -s <edge CIDR> -d <mesh CIDR> MASQUERADE
                                                              ▼
                                       mesh reached via the node's existing routes
                          ┌────────────────────────────────────┴───────────────────────────────┐
                          ▼                                                                    ▼
                native: dwx-mesh0 (WireGuard)                                  routed/BYO: host routes over your overlay
```

Two paths, deliberately independent:

- **Device → gateway (the access tunnel).** A plain WireGuard tunnel on a
  dedicated device `dwx-edge0` with its own listen port (default `51821`). The
  device initiates; the terminator records it as a **roaming** peer with an
  `AllowedIP` of just the device's assigned `/32` (`/128` for v6). Keepalive
  holds the NAT pinhole open. This leg is **byte-identical in native and routed
  mode** because it never references `dwx-mesh0`.
- **Gateway → mesh (the onward leg).** Reuses the gateway role unchanged: IP
  forwarding + `DWX-GW-MASQ` scoped to the **edge CIDR** as the client source
  range. The edge CIDR is supplied to the gateway role as a client CIDR
  (`DataWerx_GATEWAY_CLIENT_CIDRS`), so there is exactly one masquerade owner
  (`nat.Manager`); the edge connector never programs a second one. ClusterSetIP
  VIPs DNAT to real backends in `DWX-CLUSTERSET` exactly as for in-cluster
  traffic.

### Services by name

The device points its stub resolver at the gateway's existing clusterset DNS
responder (`DataWerx_GATEWAY_DNS_ADDR`, surfaced in the device profile) and
routes the ClusterSetIP VIP range through `dwx-edge0`: a name resolves to a VIP,
the VIP is routed into the gateway, DNAT'd, masqueraded, and load-balanced to a
real backend in any exporting cluster. Raw pod/service IP reach falls out of the
same route set for free.

### Pure logic, thin shells

- **`pkg/edge` (Pure, exhaustively unit-tested, no kernel/k8s):**
  - `PlanDevicePeer(publicKey, address) -> DevicePeer` (pubkey + device `/32`
    AllowedIP — exactly the device's own host route, nothing wider).
  - `AllocateDeviceIPs(edgeCIDR, []DeviceClaim) -> map[key]IP` — a deterministic
    function of the CIDR and the full claim set (hash + linear probe, explicit
    pins honored and reserved), mirroring `dns.AllocateClusterSetIPs`. No central
    allocator; every terminator computes the same assignment.
  - `BuildDeviceProfile(...) -> DeviceProfile` + `WireGuardQuickConfig(priv)` —
    the device-side artifact, reusing `gateway.AccessProfile` extended with the
    edge endpoint and assigned address; the private key is supplied separately
    and never stored in the profile/CRD/token.
  - `dwxedge.v1.<base64url>` enrollment-token `Encode`/`Decode`/`Validate`,
    reusing the bootstrap token shape and `topology.IsDangerousCIDR` screening.
  - `ValidateEdgeCIDR(cidr, reserved)` — fail-closed startup screen (non-
    dangerous, has host bits, disjoint from local/peer ranges).
- **Premium edge terminator** (`datawerx-premium/edge`): a second `pkg/wg`
  manager instance for `dwx-edge0` (the crypto/netlink already exists; no new
  kernel code), brought up synchronously before any peer is programmed.
- **Premium `EdgeDeviceReconciler`**: declarative full-state sync mirroring
  `MeshPeerReconciler` — key-indexed `NotFound` teardown, finalizer, change-only
  status patch, key rotation, and `expiresAt` lapse → peer torn down.

## 3. The `EdgeDevice` CRD

Cluster-scoped, `networking.datawerx.io/v1alpha1`, short name `ed`. It is the
**tier-agnostic integration point**, exactly like `MeshPeer`: it ships in the
open core (types + hand-written deepcopy + `config/crd` + chart `crds/`), and is
authored by the premium `dwxctl edge`/control-plane path. The reconciler never
branches on who wrote it.

```yaml
apiVersion: networking.datawerx.io/v1alpha1
kind: EdgeDevice
metadata:
  name: press-line-7
spec:
  deviceID: press-line-7        # stable, human-facing identity
  publicKey: <device WG pubkey> # Curve25519; shareable; private key never leaves device
  address: ""                   # optional /32 pin; empty => deterministic allocation
  allowedServices: []           # optional clusterset name globs (default: all imported)
  allowedCIDRs: []              # optional extra raw destinations; screened by IsDangerousCIDR
  identityPreserving: false     # NoNAT: pods see the device's real tunnel IP (premium nonat)
  expiresAt: null               # optional RFC3339 TTL; reconciler tears the peer down past it
status:
  phase: Connected              # Pending | Connected | Error (mirror MeshPeerPhase)
  address: 100.71.0.5/32        # the assigned tunnel address
  lastHandshakeTime: 0          # int64 epoch, like MeshPeerStatus
  message: ""
  observedGeneration: 0
```

Premium env and Helm surface - additive to the gateway role; the open-core agent
ignores these:

| Env | Helm | Meaning |
|---|---|---|
| `DataWerx_EDGE_ENABLE` | `edge.enabled` | Turn on the edge-ingress terminator on this gateway node. |
| `DataWerx_EDGE_CIDR` | `edge.cidr` | Tunnel address pool (must not overlap any mesh range). Also the gateway masquerade source scope (set it as a `DataWerx_GATEWAY_CLIENT_CIDRS` entry). |
| `DataWerx_EDGE_LISTEN_PORT` | `edge.listenPort` | UDP listen port for `dwx-edge0` (default `51821`). |
| `DataWerx_EDGE_ENDPOINT` | `edge.endpoint` | Advertised public `host:port` devices dial. |
| `DataWerx_EDGE_INTERFACE` | `edge.interface` | Terminator device name (default `dwx-edge0`). |
| `DataWerx_EDGE_PRIVATE_KEY` | (secret) | Terminator WG private key; ephemeral if unset (devices break on restart). |

The connector is opt-in and only meaningful with `DataWerx_ROLE=gateway`. The
agent validates `DataWerx_EDGE_CIDR` is non-dangerous and non-overlapping with
`localCIDRs`/peers and refuses otherwise (fail-closed).

## 4. Enrollment flows

**Free — `dwxctl edge` (open-core CLI, admin-authenticated, mirrors `dwxctl join`):**
the `edge` subcommand ships in the open-core `cmd/dwxctl` (it only authors the
free `EdgeDevice` contract via the pure `pkg/edge` planner — no premium code).
`enroll` authors an `EdgeDevice` (the admin's own kubeconfig RBAC); `profile`
renders the device-side `wg-quick` config / `dwxedge.v1` token; `list` reports
device status. With `--generate` the private key is created **device-side** and
printed to **stderr**; only the public key is sent up. `--dry-run` prints the
`EdgeDevice` for `kubectl apply -f -`. Re-enrolling is an idempotent upsert
(deterministic name via `topology.SanitizeName`). Label
`app.kubernetes.io/managed-by: dwxctl-edge`. The device only carries traffic once
a terminator is running — the premium managed connector, or the free BYO-overlay
+ gateway path.

**Premium** — zero-touch device enrollment (admin website/control plane):

- **`coordinator` (RFC 8628 device-authorization flow):** a headless device runs
  the OAuth 2.0 device-code grant; the operator approves it in the SSO/RBAC-gated
  admin website; the control plane mints the `EdgeDevice` and returns the device
  its profile.
- **Control-plane materialization:** the SaaS authors `EdgeDevice` CRDs fleet-
  wide over `EnterpriseControlPlaneClient` / the `pkg/syncer` path. Free and
  premium converge on the same CRD the reconciler programs.
- **`nonat` (no-NAT return routing):** makes `identityPreserving: true` work —
  the gateway skips the masquerade so pods/`MeshNetworkPolicy` see the device's
  real tunnel IP, and the per-node ensurer steers replies back. Without it the
  default MASQUERADE path keeps working with zero extra components.

## 5. Security model

- **Cryptographic identity = WireGuard public key.** Per device; the private key
  is generated on the device and never appears in a token, CRD, or profile.
- **Authorization is the WireGuard `AllowedIPs` ACL, cryptographically
  enforced.** Each device peer's `AllowedIP` is exactly its assigned `/32`, so a
  device cannot spoof another's source; its reachable set is the masquerade `-d`
  destinations plus the profile routes (`allowedServices`/`allowedCIDRs` narrow
  it further).
- **Isolation from the mesh control plane.** A *separate* device (`dwx-edge0`) on
  a *separate* port with a *separate*, non-overlapping CIDR. A compromised device
  key can never be mistaken for a cluster peer on `dwx-mesh0`, cannot author
  CRDs, and is bounded to the edge CIDR's masquerade scope. The edge CIDR is
  validated against `topology.IsDangerousCIDR` and local/peer ranges at startup.
- **Least privilege onward.** `DWX-GW-MASQ` is scoped `-s <edge CIDR>` only, so
  it never pre-empts the source-preserving `DWX-MESH-NOMASQ` exemption.
- **Revocation & rotation.** Delete the `EdgeDevice` (or let `expiresAt` lapse) →
  the reconciler removes the peer on the next reconcile; no handshake, no route.
  Key rotation tears the stale key down before programming the new one. Premium
  enrollment issues short-lived grants via the `coordinator`.
- **Auditability.** Free/`dwxctl edge` enrollments carry the `managed-by` label;
  premium enrollments are SSO-attributed and audit-logged.
- **Exposure surface.** One inbound UDP port on the gateway node — into the
  *cluster*, never into the device — strictly less than a `kubectl port-forward`
  left running, and unlike port-forward it is declarative, revocable, scoped, and
  survives pod restarts.

## 6. Open-core boundary

| Layer | Free (Apache-2.0) | Premium (hosted / private operator) |
|---|---|---|
| Contract | `EdgeDevice` CRD + pure `pkg/edge` planner + device `AccessProfile`/wg-quick artifact | — (consumes the same contract) |
| Data plane | (free edge reach = BYO overlay + gateway role; `DWX-GW-MASQ` reused) | managed `dwx-edge0` terminator + `EdgeDeviceReconciler`; `nonat` identity-preserving routing |
| Enrollment | `dwxctl edge enroll`/`profile`/`list` (open-core CLI, kubeconfig-RBAC, `--generate`) | RFC 8628 `coordinator` device-code flow (mints `EdgeDevice` + returns profile); SSO/RBAC/TTL |
| Fleet | — | admin-website fleet management; control-plane materializes `EdgeDevice`s |

The mesh runs fully without any premium piece (`COMMITMENT.md`): BYO overlay +
gateway is a complete, free, data-plane-agnostic edge connector. The reconcile
loop and data plane **never branch on tier**.

## 7. Testing layers

- **unit (every push, hermetic):** `pkg/edge` pure logic — deterministic device-
  IP allocation (order-independent, collision-probed, pins honored),
  `PlanDevicePeer`, profile/wg-quick rendering, token round-trip and rejection of
  foreign/dangerous inputs, `ValidateEdgeCIDR`, plus fuzzers for the token codec
  and allocator. The premium `EdgeDeviceReconciler` against a fake client + fake
  terminator plane (enroll → Connected, delete → torn down, `expiresAt` lapse,
  bad key → Error, deterministic distinct addresses).
- **integration (`integration`, envtest):** reconciler vs. a real API server.
- **dataplane (`dataplane`, netns + root):** a real `dwx-edge0` + a real
  WireGuard "device" peer in a second netns dialing in through NAT — run once
  with `DataWerx_DATAPLANE=wireguard` and once with `routed` to prove data-plane
  independence is real, not assumed.
- **e2e (`e2e`, kind):** extend the gateway e2e so an out-of-cluster WireGuard
  client resolves and calls a `*.clusterset.local` service across the mesh.

## 8. Scope

- **Not a cluster peer.** Devices never join `dwx-mesh0`, never export Services,
  never appear in the topology graph as peers — they are clients.
- **Mesh→device initiation** stays out of scope (the mesh never dials the
  device).
- **Public-endpoint discovery** for the gateway is left to the operator passing
  `DataWerx_EDGE_ENDPOINT`, mirroring `dwxctl join`'s `--endpoint`.
- **Device-side agent.** The device runs stock `wg-quick` — no DataWerx binary.
- **Per-device observability** reuses the existing prober/metrics/snapshot
  surfaces; a full edge SLO view is premium fleet scope.

## 9. Map of seams this plugs into

- **Gateway role (reuse as-is):** `pkg/gateway`, `pkg/nat.BuildGatewayMasqRules`/
  `GatewayMasqChain`, `controllers.GatewayReconciler` (`NoNAT`, `ClientCIDRs`).
- **WireGuard data plane (reuse for the terminator):** `pkg/wg` — a second
  manager instance for `dwx-edge0`; roaming-endpoint learning + persistent-
  keepalive already exist.
- **Enrollment token discipline:** `pkg/bootstrap` token shape,
  `topology.IsDangerousCIDR`/`SanitizeName`, `ManagedByLabel`.
- **Broker-less allocation precedent:** `pkg/dns.AllocateClusterSetIPs` →
  `edge.AllocateDeviceIPs`.
- **CRDs (tier-agnostic integration point):** `EdgeDevice` in
  `networking.datawerx.io` (hand-written deepcopy, no codegen, like `MeshPeer`).
- **Premium injection:** `pkg/agent.Options.RegisterPremium` → `edge.RegisterEdge`
  (terminator + reconciler), composed alongside `coordinator` (RFC 8628) and
  `nonat`, per design 0004 §8.
- **Operator CLI:** `cmd/dwxctl` — `edge` subcommand (premium) alongside
  `verify`, `join`, `snapshot`.

---

*Provenance: extends design 0006 zero-friction join and the remote-access
gateway role to a non-Kubernetes device, under the open-core seam discipline of
design 0004. The durable thesis is §1's principle — decouple the device access
transport from the mesh transport — which is what makes the connector identical
across the native and BYO-overlay data planes. The tiering (§0) makes the managed
connector premium while keeping the CRD + pure planner free as the contract.*
