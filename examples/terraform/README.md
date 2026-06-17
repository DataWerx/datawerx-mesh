# Terraform module — install DataWerx Mesh

A small, self-contained module that installs the DataWerx Mesh agent into an
existing Kubernetes cluster from the signed OCI Helm chart. It is a starting
point: copy it into your infrastructure repo and wire the providers to your own
cluster credentials.

## What it does

- Creates the `datawerx-system` namespace (optional).
- Installs the `datawerx-mesh` chart from
  `oci://ghcr.io/datawerx/datawerx/charts` at a pinned version.
- Passes the cluster ID and, optionally, the WireGuard private-key Secret and any
  extra Helm values through to the chart.

It deliberately does **not** manage `MeshPeer` resources — peer topology is
GitOps-authored (free tier) or synced from the control plane (premium), per the
open-core seam. Use it to get the agent running; author peers separately.

## Usage

```hcl
module "datawerx_mesh" {
  source = "github.com/DataWerx/datawerx-mesh//examples/terraform"

  cluster_id    = "us-east"
  chart_version = "0.1.0"

  # Optional: reference a pre-created Secret holding the node WireGuard key.
  wireguard_private_key_secret = "datawerx-wg-key"

  # Optional: any additional chart values.
  extra_values = {
    "metrics.serviceMonitor.enabled" = "true"
  }
}
```

Configure the `helm` and `kubernetes` providers against your cluster before
applying — for example from an EKS/GKE/AKS data source or a kubeconfig:

```hcl
provider "helm" {
  kubernetes {
    config_path = "~/.kube/config"
  }
}

provider "kubernetes" {
  config_path = "~/.kube/config"
}
```

## Inputs

| Name | Description | Default |
|------|-------------|---------|
| `cluster_id` | Stable mesh ID for this cluster. | _(required)_ |
| `chart_version` | DataWerx Mesh chart version to install. | `0.1.0` |
| `namespace` | Namespace to install into. | `datawerx-system` |
| `create_namespace` | Create the namespace. | `true` |
| `release_name` | Helm release name. | `datawerx-mesh` |
| `wireguard_private_key_secret` | Name of an existing Secret with the node key; empty uses ephemeral keys. | `""` |
| `extra_values` | Map of additional chart values (`set` blocks). | `{}` |

## Outputs

| Name | Description |
|------|-------------|
| `release_name` | The installed Helm release name. |
| `namespace` | The namespace the agent runs in. |
| `chart_version` | The chart version that was installed. |
