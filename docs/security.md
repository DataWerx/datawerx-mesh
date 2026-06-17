# Threat model

Herein we describe the trust boundaries, assets, and threats for DataWerx
Mesh, and the operator responsibilities that go with running a privileged node
agent that programs the kernel data plane. For vulnerability reporting and the
hardening checklist, see [`SECURITY.md`](../SECURITY.md).

## Trust boundaries

```
   GitOps / control plane          Kubernetes API server            kernel data plane
  (authors MeshPeers, SSO) ─────►  (MeshPeer/MCS objects, RBAC) ──► (WireGuard, routes,
        TRUST: high                      TRUST: enforced by RBAC       iptables) per node
                                                                       TRUST: agent is root
   remote cluster peer  ◄══ encrypted WireGuard (cryptokey routing) ══►  this cluster
        TRUST: cryptographic (peer public key); CIDR claims are NOT authenticated
```

Key point: **a `MeshPeer` is authorized network access.** Whoever can create or
modify a `MeshPeer` can program a WireGuard peer and steer the claimed CIDRs into
this cluster's nodes. The CIDR *claims* in a `MeshPeer` are not cryptographically
attested — they are trusted because the object passed API-server RBAC. Treat
`MeshPeer` write access as equivalent to "connect a cable into our network."

## Assets

- **Node WireGuard private key** — identity of the node in the mesh.
- **The node routing table / iptables** — the agent mutates these with `NET_ADMIN`.
- **Cross-cluster traffic** — confidential/integrity-protected by WireGuard.
- **Premium SSO token** — bearer credential to the managed control plane (premium tier).

## Threats and mitigations

| # | Threat | Mitigation |
|---|--------|------------|
| T1 | **Traffic hijack via a malicious/over-broad `MeshPeer`** — a peer advertises `0.0.0.0/0` or a victim CIDR to capture traffic. | The planner **refuses to route dangerous prefixes** (default route, loopback, link-local, multicast) regardless of `DataWerx_LOCAL_CIDRS` (`topology.IsDangerousCIDR`), and **refuses overlapping local CIDRs** (reported as `Error`, never routed). Operators must still **restrict `MeshPeer` RBAC**. |
| T2 | **CIDR collision hijack** — a remote claims a CIDR your cluster uses locally. | Overlap is detected and the range is withheld (`Phase=Error`); only the premium 1:1 NAT remap path routes overlaps, into a virtual range. |
| T3 | **Node key compromise** | Key is a projected Secret (not in image), never logged (truncated via `shortKey`); rotate by replacing the Secret and the peer's advertised public key. |
| T4 | **Data-plane disruption** — a bug corrupts node networking. | All rules live in dedicated `DWX-*` chains, full-state synced (idempotent, self-healing); the agent only ever deletes routes it installed (per-peer bookkeeping). |
| T5 | **DNS spoofing via a forged `ServiceImport`** | The `clusterset.local` responder answers only from MCS objects, which are RBAC-controlled; it never proxies arbitrary names. |
| T6 | **Privilege blast radius** — the agent is root/`NET_ADMIN`. | Distroless nonroot image, no shell; tighten `privileged` to `NET_ADMIN`+`SYS_MODULE` in production. |
| T7 | **Premium coordinator abuse** — token theft, device-code flooding. | Access/refresh tokens expire and are purged; device codes are single-use; in-flight device requests are capped. The default `DevAuthenticator` is **insecure and for testing only** — production must use a real OIDC IdP. |
| T8 | **MITM on the wire** | WireGuard cryptokey routing: a peer can only send/receive within its programmed AllowedIPs, authenticated by its public key. |

## Operator responsibilities

- **Restrict `MeshPeer` (and MCS object) write RBAC** to the GitOps pipeline /
  trusted operators only. This is the primary control for T1/T2.
- Provide a **real WireGuard private key** via Secret; rotate periodically.
- Tighten the pod `securityContext` from `privileged` to the capability set.
- For the premium coordinator, **replace `DevAuthenticator` with a real OIDC IdP**
  before exposing it.
- Monitor `dwx_meshpeers{phase!="Connected"}` and the NAT/remap error counters.

## Currently out of scope

- Cryptographic attestation of CIDR claims (a `MeshPeer`'s advertised ranges are
  trusted via RBAC, not signed). A signed-topology control plane is a premium
  direction.
- Multi-tenant isolation within a single cluster: `MeshPeer` is cluster-scoped.
