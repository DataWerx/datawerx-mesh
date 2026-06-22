# Design 0013 — Edge device connector (device-initiated mesh access)

- Status: **Proposed.**
- Milestone: **M5+ (Intelligence & adoption / edge reach).**
- Packages (proposed): `pkg/edge` (pure), a thin terminator reusing `pkg/wg`,
  an `EdgeDevice` CRD in `networking.datawerx.io`, surfaced by `dwxctl edge`.
- Builds on: the remote-access gateway role (`pkg/gateway`, `pkg/nat`
  `BuildGatewayMasqRules`, `controllers.GatewayReconciler`), the join bundle
  pattern (`pkg/bootstrap`, design 0006), and the premium injection seam
  (`pkg/agent.Options.RegisterPremium`).

## Summary

Let a **single edge device that is not a Kubernetes node** — an IoT box, a
factory gateway, a VM, a developer laptop — reach mesh services **by name**
(`*.clusterset.local`) over a tunnel the **device dials outbound**. The mesh
never initiates a connection to the device; the device is free to sit behind
NAT/CGNAT with no inbound ports. This is the "like a VPN, better than
`kubectl port-forward`" access path.

The hard requirement that shapes the whole design: **it must work identically
whether the cluster runs the native WireGuard data plane
(`DataWerx_DATAPLANE=wireguard`) or a bring-your-own overlay
(`DataWerx_DATAPLANE=routed`).** Today neither existing mechanism satisfies that
on its own (see §1).

The core idea is one sentence:

> **Decouple the device's access transport from the mesh's internal transport.**

The device always brings its **own** WireGuard tunnel, terminated on a dedicated
**edge-ingress** endpoint owned by this connector — *separate from* the node-to-
node mesh data plane. From that terminator the existing gateway role forwards and
masquerades the device into the mesh over whatever routes the node already has
(the native `dwx-mesh0` device, or BYO host routes over your overlay). Because
the device tunnel never touches `dwx-mesh0`, the device-side setup and the
security model are byte-for-byte identical in both modes.

## 1. Why the existing pieces are not enough

Three things already exist and each gets us part of the way. None is sufficient
alone, and the gap is exactly "works regardless of data plane".

1. **Roaming `MeshPeer` over native WireGuard.** `MeshPeer.spec.endpoint` is
   optional; an empty endpoint is a *roaming* peer whose address the kernel
   learns from the inbound handshake (`pkg/apis/networking/v1alpha1/meshpeer_types.go`),
   and the agent applies a 25 s persistent-keepalive for NAT traversal
   (`pkg/wg/wireguard.go`). A device could be registered this way and dial in.
   **But** this only exists in native mode: in `routed` mode DataWerx creates no
   WireGuard device at all (`pkg/routed`, `docs/byo-overlay.md`), so there is
   nothing for the device to handshake with. It also conflates a *device* with a
   *cluster peer* — wrong identity model, wrong blast radius, wrong CIDR
   semantics.

2. **The remote-access gateway role** (`DataWerx_ROLE=gateway`). This is already
   data-plane-agnostic *for the onward leg*: it enables IP forwarding
   (`pkg/gateway/forward.go`), masquerades client→mesh traffic
   (`pkg/nat.BuildGatewayMasqRules`, chain `DWX-GW-MASQ`), and publishes an
   access-profile ConfigMap (`pkg/gateway/profile.go`,
   `controllers.GatewayReconciler`) — all over the node's existing routes
   regardless of data plane. **But** it deliberately *does not own a WireGuard
   device or terminate any tunnel* (`pkg/gateway/profile.go` package doc); it
   assumes the device is already on a shared overlay. In a pure-native install
   there is no device-facing terminator.

3. **The join bundle** (`pkg/bootstrap`, design 0006). The right *shape* for a
   secret-free, validated, single-token enrollment — but it is cluster↔cluster
   and authors `MeshPeer`s. A device is not a cluster.

**The missing component is a data-plane-independent edge-ingress terminator.**
This design adds exactly that, and wires it to the gateway role (2) for the
onward leg and borrows the token discipline of (3) for enrollment.

## 2. Architecture

```
edge device (behind NAT)                          gateway node (a mesh node, any data plane)
────────────────────────                          ───────────────────────────────────────────
wg-quick: [Peer] = gateway:51821                   edge-ingress terminator (pkg/edge + pkg/wg)
  AllowedIPs = clusterset VIP CIDR + mesh CIDRs      own WireGuard device `dwx-edge0`, listen :51821
  PersistentKeepalive = 25  ───── outbound UDP ────► peer = device pubkey, AllowedIP = device /32
  DNS = gateway clusterset responder                          │ (device source learned from handshake)
                                                              ▼
                                                   IP forward + DWX-GW-MASQ  (gateway role, §1.2)
                                                              │  -s <edge CIDR> -d <mesh CIDR> MASQUERADE
                                                              ▼
                                       mesh reached via the node's existing routes
                          ┌────────────────────────────────────┴───────────────────────────────┐
                          ▼                                                                       ▼
                native: dwx-mesh0 (WireGuard)                                  routed/BYO: host routes over your overlay
```

Two legs, deliberately independent:

- **Device → gateway (the access tunnel).** A plain WireGuard tunnel on a
  *dedicated* device `dwx-edge0` with its own listen port (default `51821`, to
  not collide with the mesh's `51820`). The device initiates; the terminator
  records the device as a roaming peer with an `AllowedIP` of just the device's
  assigned `/32` (`/128` for v6). Keepalive holds the NAT pinhole open. This leg
  is owned entirely by `pkg/edge` and is **byte-identical in native and routed
  mode** because it never references `dwx-mesh0`.

- **Gateway → mesh (the onward leg).** Reuses the gateway role unchanged: IP
  forwarding + `DWX-GW-MASQ` scoped to the **edge CIDR** as the client source
  range, then the node's normal mesh routes. ClusterSetIP VIPs DNAT to real
  backends in `DWX-CLUSTERSET` exactly as they do for in-cluster traffic.

### Services by name (the priority use case)

The device gets `*.clusterset.local` resolution by pointing its stub resolver at
the gateway's existing clusterset DNS responder (the same `DataWerx_GATEWAY_DNS_ADDR`
the access profile already advertises, `pkg/gateway/profile.go` `DNSConfig`). The
device routes the ClusterSetIP VIP range through `dwx-edge0`; a name resolves to a
VIP, the VIP is routed into the gateway, DNAT'd, masqueraded, and load-balanced to
a real backend in any exporting cluster. Raw pod/service IP reach falls out of the
same route set for free.

### Pure logic, thin shells (house style)

- `pkg/edge` (PURE, exhaustively unit-tested, no kernel/k8s):
  - `PlanDevicePeer(EdgeDevice) -> wg PeerConfig` (pubkey + device `/32` AllowedIP).
  - `AllocateDeviceIP(edgeCIDR, sorted device keys) -> IP` — a deterministic
    function of the CIDR and the full sorted device set (hash + linear probe),
    mirroring the broker-less `dns.AllocateClusterSetIPs`. No central allocator;
    every gateway computes the same assignment, and an operator may also pin an
    explicit `spec.address`.
  - `BuildDeviceProfile(...) -> wg-quick config + AccessProfile` — the device-side
    artifact, reusing `gateway.AccessProfile` extended with the edge endpoint and
    the assigned address.
  - Enrollment token encode/decode/validate, reusing the `dwxmesh.v1.<base64url>`
    shape and `topology.IsDangerousCIDR` screening from `pkg/bootstrap`.
- A thin **edge terminator manager** that brings up `dwx-edge0` and upserts/removes
  device peers — a second consumer of `pkg/wg` (the crypto/netlink is already there;
  no new kernel code), separate instance from the mesh device.
- A thin **`EdgeDeviceReconciler`** that programs the terminator and re-drives the
  gateway masquerade/profile for the edge CIDR. Declarative full-state sync like
  every other reconciler; `NotFound` tears the peer down (key-indexed, as
  `MeshPeerReconciler` does).

## 3. The `EdgeDevice` CRD (config surface)

Cluster-scoped, `networking.datawerx.io/v1alpha1`, short name `ed`. It is the
**tier-agnostic integration point**, exactly like `MeshPeer`: authored by the
free `dwxctl edge` path **or** materialized by the premium control plane /
admin website (§5). The reconciler never branches on who wrote it.

```yaml
apiVersion: networking.datawerx.io/v1alpha1
kind: EdgeDevice
metadata:
  name: press-line-7            # deterministic from deviceID
spec:
  deviceID: press-line-7        # stable, human-facing identity
  publicKey: <device WG pubkey> # Curve25519; shareable; private key never leaves device
  address: ""                   # optional /32 pin; empty => deterministic allocation from edge CIDR
  allowedServices:              # optional scoping (clusterset name globs); default: all imported
    - payments.prod
    - "telemetry.*"
  allowedCIDRs: []              # optional extra raw destinations; screened by IsDangerousCIDR
  identityPreserving: false     # NoNAT: pods see the device's real tunnel IP (premium return-route)
  expiresAt: null               # optional RFC3339 TTL; reconciler tears the peer down past it
status:
  phase: Connected              # Pending | Connected | Error  (mirror MeshPeerPhase)
  address: 100.71.0.5/32        # the assigned tunnel address
  lastHandshakeTime: 0          # int64 epoch, like MeshPeerStatus
  message: ""
  observedGeneration: 0
```

Gateway/edge env + Helm surface (additive to the existing gateway role):

| Env | Helm | Meaning |
|---|---|---|
| `DataWerx_EDGE_ENABLE` | `edge.enabled` | Turn on the edge-ingress terminator on this gateway node. |
| `DataWerx_EDGE_CIDR` | `edge.cidr` | Tunnel address pool for devices (must not overlap any mesh range). Becomes the gateway masquerade source scope. |
| `DataWerx_EDGE_LISTEN_PORT` | `edge.listenPort` | UDP listen port for `dwx-edge0` (default `51821`). |
| `DataWerx_EDGE_ENDPOINT` | `edge.endpoint` | The advertised public `host:port` devices dial (NodePort/LB/elastic IP). |

The edge terminator is opt-in and only meaningful with `DataWerx_ROLE=gateway`
(it depends on the gateway's forwarding + masquerade + DNS advertisement). The
agent validates `DataWerx_EDGE_CIDR` is non-dangerous and non-overlapping with
`localCIDRs`/peers, and refuses otherwise (fail-closed, matching the remap-off
refuse behavior).

## 4. Enrollment flows

**Free — `dwxctl edge` (the GitOps/manual flow, automated; mirrors `dwxctl join`):**

- `dwxctl edge enroll --device-id press-line-7 [--generate]` — authors an
  `EdgeDevice` (admin authenticated by their own kubeconfig RBAC) and prints the
  device-side artifact: a ready `wg-quick` config (or `AccessProfile` JSON). With
  `--generate` the private key is created **device-side** and only the public key
  is sent up; the private key prints to **stderr** with a "store this on the
  device" note — the same discipline as `dwxctl join --generate`.
- `--dry-run` prints the `EdgeDevice` object for `kubectl apply -f -`. Re-enrolling
  the same device is an idempotent upsert (deterministic name via
  `topology.SanitizeName`).
- Label `app.kubernetes.io/managed-by: dwxctl-edge` for audit/cleanup, mirroring
  `bootstrap.ManagedByJoin`.

**Premium — zero-touch device enrollment (admin website + control plane):**

This is where the connector lights up the seams that already exist by name:

- **`coordinator` (RFC 8628 device-authorization flow)** — the premium operator
  component already referenced in `pkg/agent` design 0004 §8. A device with no
  kubeconfig runs the OAuth 2.0 *device code* grant: it shows a short code, the
  operator approves it in the admin website (SSO/RBAC-gated), and the control
  plane mints the `EdgeDevice` CRD and returns the device its config. This is the
  textbook onboarding flow for headless edge hardware and is exactly what RFC 8628
  is for.
- **Control-plane materialization** — the admin website / SaaS authors
  `EdgeDevice` CRDs fleet-wide over `EnterpriseControlPlaneClient` (and/or the
  `pkg/syncer` path that already mirrors topology into `MeshPeer`s). Free and
  premium converge on the same CRD the reconciler programs.
- **`nonat` (no-NAT return routing)** — the existing premium per-node return-route
  component is what makes `identityPreserving: true` work: with it, the gateway
  skips the masquerade (`GatewayReconciler.NoNAT`) so pods and `MeshNetworkPolicy`
  see the device's real tunnel IP, and replies are steered back to the gateway.
  Without it, the OSS default MASQUERADE path keeps working with zero extra
  components.

All premium pieces inject through `pkg/agent.Options.RegisterPremium`; the
open-core build wires none of them and remains fully functional with the manual
`dwxctl edge` flow.

## 5. Security model

Identity, authorization, and blast radius are the crux for an inbound-from-
anywhere edge surface.

- **Cryptographic identity = WireGuard public key.** Curve25519, per device. The
  private key is generated on the device and **never** appears in an enrollment
  token, CRD, or profile — the same secret-free discipline `pkg/bootstrap`
  enforces for cluster bundles.
- **Authorization is the WireGuard `AllowedIPs` ACL, cryptographically enforced.**
  The terminator gives each device peer an `AllowedIP` of exactly its assigned
  `/32`, so a device cannot spoof another device's source address; and the device's
  reachable set is the masquerade `-d` destinations plus the routes in its profile.
  `allowedServices`/`allowedCIDRs` narrow this further (compiled into the profile
  and, in `nonat` mode, into device-scoped `MeshNetworkPolicy`).
- **Isolation from the mesh control plane.** The terminator is a *separate*
  device (`dwx-edge0`) on a *separate* port with a *separate*, non-overlapping
  CIDR. A compromised device key can never be mistaken for a cluster peer on
  `dwx-mesh0`, cannot author CRDs, and is bounded to the edge CIDR's masquerade
  scope. The edge CIDR is validated against `topology.IsDangerousCIDR` and against
  local/peer ranges at startup.
- **Least privilege onward.** `DWX-GW-MASQ` is scoped `-s <edge CIDR>` only, so it
  never pre-empts the source-preserving exemption for the node's own pod traffic
  (`DWX-MESH-NOMASQ`), exactly as the gateway masquerade is scoped today.
- **Revocation & rotation.** Delete the `EdgeDevice` (or let `expiresAt` lapse) →
  the reconciler removes the peer and the device is cut off on the next reconcile;
  no handshake, no route. Key rotation tears down the stale key before programming
  the new one, mirroring `MeshPeerReconciler`. Premium enrollment issues
  short-lived grants via the `coordinator`.
- **Auditability.** Free enrollments carry the `managed-by: dwxctl-edge` label;
  premium enrollments are SSO-attributed and audit-logged in the hosted plane
  (the same free-read / paid-governed-act seam as the rest of design 0004 §2).
- **Exposure surface.** Only one inbound UDP port on the gateway node (the edge
  listener) — into the *cluster*, never into the device. This is strictly less
  exposure than a `kubectl port-forward` left running, and unlike port-forward it
  is declarative, revocable, scoped, and survives pod restarts.

## 6. Open-core boundary (binding — design 0004 §2)

| Layer | Free (Apache-2.0) | Premium (hosted / private operator) |
|---|---|---|
| Data plane | `dwx-edge0` terminator, `DWX-GW-MASQ` for the edge CIDR (reused) | `nonat` identity-preserving return routing |
| Contract | `EdgeDevice` CRD + device `AccessProfile`/wg-quick artifact | — (consumes the same CRD) |
| Enrollment | `dwxctl edge enroll` (kubeconfig-RBAC, token, `--generate`) | RFC 8628 `coordinator` device-code flow, SSO/RBAC, TTL |
| Fleet | n/a | admin-website fleet management; control-plane materializes `EdgeDevice`s via `EnterpriseControlPlaneClient`/`pkg/syncer` |

The mesh must run fully without any premium piece (`COMMITMENT.md`): the manual
`dwxctl edge` path is a complete, free, data-plane-agnostic edge connector. The
reconcile loop and data plane **never branch on tier**.

## 7. Testing layers (house convention)

- **unit (every push, hermetic):** `pkg/edge` pure logic — deterministic device-IP
  allocation (order-independent, collision-probed), `PlanDevicePeer`, profile/
  wg-quick rendering, token round-trip and rejection of foreign/dangerous inputs;
  `EdgeDeviceReconciler` against a fake client + fake terminator/gateway data plane.
- **integration (`integration`, envtest):** reconciler vs. a real API server —
  enroll → `Connected`, delete → peer torn down, `expiresAt` lapse.
- **dataplane (`dataplane`, netns + root):** a real `dwx-edge0` device + a real
  WireGuard "device" peer in a second netns dialing in through NAT, asserting a
  handshake and that masqueraded traffic reaches a mesh CIDR — run once with
  `DataWerx_DATAPLANE=wireguard` and once with `routed` to prove data-plane
  independence is real, not assumed.
- **e2e (`e2e`, kind):** extend the gateway e2e so an out-of-cluster WireGuard
  client resolves and calls a `*.clusterset.local` service across the mesh.

## 8. Scope / non-goals

- **Not a cluster peer.** Devices never join `dwx-mesh0`, never export Services,
  and never appear in the cluster topology graph as peers — they are clients.
- **Mesh→device initiation** stays out of scope by design (the device is a
  client; the mesh never dials it).
- **Public-endpoint discovery** for the gateway (reading a LoadBalancer/NodePort
  address) is left to the operator passing `DataWerx_EDGE_ENDPOINT`, mirroring
  `dwxctl join`'s `--endpoint`; auto-discovery can follow.
- **Device-side agent.** The device runs stock WireGuard (`wg-quick`) — no
  DataWerx binary required. A thin optional helper (renew/rotate) is future work.
- **Per-device observability** (handshake age → status/metrics) reuses the
  existing prober/metrics surfaces and the snapshot contract (design 0005); a
  full edge SLO view is premium fleet scope.

## 9. Map of seams this plugs into (grounding for the implementing session)

- **Gateway role (reuse as-is):** `pkg/gateway` (`forward.go`, `profile.go`),
  `pkg/nat.BuildGatewayMasqRules`/`GatewayMasqChain`,
  `controllers.GatewayReconciler` (`NoNAT`, `ClientCIDRs`, `publishProfile`).
- **WireGuard data plane (reuse for the terminator):** `pkg/wg` — a second
  manager instance for `dwx-edge0`; `ConfigurePeer`/`RemovePeer`,
  persistent-keepalive, roaming-endpoint learning already exist.
- **Enrollment token discipline:** `pkg/bootstrap` (`dwxmesh.v1` token,
  `Validate`, `topology.IsDangerousCIDR`, `topology.SanitizeName`,
  `ManagedByLabel`).
- **Broker-less allocation precedent:** `pkg/dns.AllocateClusterSetIPs` (hash +
  deterministic probe) — the template for `edge.AllocateDeviceIP`.
- **CRDs (tier-agnostic integration point):** new `EdgeDevice` in
  `networking.datawerx.io` (hand-written deepcopy, no codegen, like `MeshPeer`).
- **Premium injection:** `pkg/agent.Options.RegisterPremium` →
  `coordinator` (RFC 8628 device auth) and `nonat` (no-NAT return routing), per
  design 0004 §8.
- **Operator CLI:** `cmd/dwxctl` — new `edge` subcommand alongside `verify`,
  `join`, `snapshot`.

---

*Provenance: extends design 0006 (zero-friction join) and the remote-access
gateway role to a non-Kubernetes device, under the open-core seam discipline of
design 0004. The durable thesis is §1's principle — decouple the device access
transport from the mesh transport — which is what makes the connector identical
across the native and BYO-overlay data planes.*
