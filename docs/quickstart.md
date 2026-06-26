# Quickstart

Link **two clusters** and call a Service across them by name. Uses
[kind](https://kind.sigs.k8s.io) so it runs on one laptop; the steps are
identical on real clusters.

**Prereqs:** `Docker`, `kind`, `kubectl`, `helm`, `wg` (wireguard-tools), and the WireGuard kernel module (`sudo modprobe wireguard`).

## Fastest path to see it work (scripted)

```sh
hack/e2e/kind-up.sh                 # two kind clusters + agent + reciprocal peering
dwx mesh verify --context kind-dwx-a  # → Mesh peers: 1 connected
hack/demo/quickstart.sh             # export an echo Service in A, call it by name from B → hi-from-a
hack/e2e/kind-down.sh               # clean up
```

These scripts create a working two-cluster mesh with a service called across it. The rest of this doc does the same thing by hand so you understand each piece.  These steps map 1:1 onto real clusters.

## 1. Two clusters

Give each its own pod/service CIDRs (or enable [remap](how-it-works.md#overlapping-cidrs) if they must overlap):

```sh
cat <<EOF | kind create cluster --name dwx-a --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking: { podSubnet: "10.244.0.0/16", serviceSubnet: "10.96.0.0/16" }
EOF
cat <<EOF | kind create cluster --name dwx-b --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking: { podSubnet: "10.245.0.0/16", serviceSubnet: "10.97.0.0/16" }
EOF
```

## 2. Build & load the agent image

```sh
docker build -t datawerx/mesh-agent:dev .
kind load docker-image datawerx/mesh-agent:dev --name dwx-a
kind load docker-image datawerx/mesh-agent:dev --name dwx-b
```

## 3. Install the agent (Helm)

```sh
PRIV_A=$(wg genkey); PUB_A=$(echo "$PRIV_A" | wg pubkey)
PRIV_B=$(wg genkey); PUB_B=$(echo "$PRIV_B" | wg pubkey)

install() { # ctx clusterID priv localCIDRs
  kubectl --context "$1" create namespace datawerx-system --dry-run=client -o yaml | kubectl --context "$1" apply -f -
  kubectl --context "$1" -n datawerx-system create secret generic dwx-wg \
    --from-literal=private-key="$3" --dry-run=client -o yaml | kubectl --context "$1" apply -f -
  helm --kube-context "$1" upgrade --install dwx ./charts/datawerx-mesh -n datawerx-system \
    --set image.tag=dev --set image.pullPolicy=Never \
    --set clusterID="$2" --set-string localCIDRs="$4" \
    --set wireguard.privateKey.existingSecret=dwx-wg
}
install kind-dwx-a cluster-a "$PRIV_A" "10.244.0.0/16\,10.96.0.0/16"
install kind-dwx-b cluster-b "$PRIV_B" "10.245.0.0/16\,10.97.0.0/16"
```

## 4. Make the clusters Mesh Peers

Each cluster gets a `MeshPeer` describing the *other* including its key, endpoint, CIDRs. On kind the endpoint is the other control-plane container's IP on the shared
docker network:

```sh
IP_A=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' dwx-a-control-plane)
IP_B=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' dwx-b-control-plane)

peer() { # ctx name pub endpoint pod svc
  kubectl --context "$1" apply -f - <<EOF
apiVersion: networking.datawerx.io/v1alpha1
kind: MeshPeer
metadata: { name: $2 }
spec:
  clusterID: $2
  publicKey: "$3"
  endpoint: "$4"
  podCIDRs: ["$5"]
  serviceCIDRs: ["$6"]
EOF
}
peer kind-dwx-a cluster-b "$PUB_B" "$IP_B:51820" 10.245.0.0/16 10.97.0.0/16
peer kind-dwx-b cluster-a "$PUB_A" "$IP_A:51820" 10.244.0.0/16 10.96.0.0/16

dwx mesh verify --context kind-dwx-a   # → Mesh peers: 1 connected
```

## 5. Export a service and call it across the mesh

```sh
# A headless 'echo' Service in cluster A, exported via the standard MCS API
kubectl --context kind-dwx-a create namespace demo
# http-echo:1.0 is distroless; its entrypoint is the binary, so name it first
# (`/http-echo …`) — the args after `--` replace the entrypoint, they don't append.
kubectl --context kind-dwx-a -n demo create deployment echo --image=hashicorp/http-echo:1.0 -- /http-echo -listen=:8080 -text=hi-from-a
kubectl --context kind-dwx-a -n demo expose deployment echo --port=8080 --cluster-ip=None
kubectl --context kind-dwx-a apply -f - <<EOF
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata: { name: echo, namespace: demo }
EOF

# Free tier: your GitOps pipeline mirrors EndpointExports between clusters.
# Here we copy by hand, point CoreDNS at the responder, and curl by name.
kubectl --context kind-dwx-b create namespace demo
kubectl --context kind-dwx-a -n demo get endpointexports -o yaml | \
  kubectl --context kind-dwx-b -n demo apply -f -
hack/e2e/patch-coredns.sh kind-dwx-b

kubectl --context kind-dwx-b -n demo run probe --rm -it \
  --image=curlimages/curl:8.7.1 --restart=Never -- \
  curl -s http://echo.demo.svc.clusterset.local:8080
# -> hi-from-a
```

The Service in cluster A, reached by name from cluster B, over an encrypted
tunnel — no LoadBalancer, no public IP.

## 6. Ask an AI about your mesh (optional)

The same state you just verified is available to any AI agent through a
**read-only** MCP server. With `dwx mcp` on your `PATH` (built from
`./cmd/dwx`, or via [install](install.md)), point Claude Desktop or Claude
Code at the cluster — add to your MCP host config (`claude_desktop_config.json`
or `.mcp.json`):

```json
{
  "mcpServers": {
    "datawerx-mesh": { "command": "dwx mcp", "args": ["--context", "kind-dwx-a"] }
  }
}
```

Restart the host, then ask *"is the DataWerx mesh healthy, and what's it importing?"*

Prefer the terminal? The agent reads exactly what these print:

```sh
dwx mesh diagnose --context kind-dwx-a          # grounded "obvious cause" findings
dwx mesh graph --context kind-dwx-a --format mermaid   # paste into any Markdown to see the topology
```

→ **[Ask an AI about your mesh](ai-agents.md)** for the full tool list and the design.

## Next

- **[How it works](how-it-works.md)** — what just happened, end to end.
- **[Ask an AI about your mesh](ai-agents.md)** — point Claude or any agent at the live mesh state.
- **[Bring your own overlay](byo-overlay.md)** — do this on your existing Tailscale/NetBird mesh.
- **[Cross-cluster services](cross-cluster-services.md)** — headless vs. ClusterSetIP, and production DNS.
- **[Configuration](configuration.md)** — production install, metrics, every setting.
