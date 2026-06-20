# Installs the DataWerx Mesh agent from the signed OCI Helm chart. The chart
# carries the DaemonSet, RBAC, CRDs, and the clusterset.local DNS Service; this
# module only parameterizes the install. Peer topology (MeshPeer resources) is
# authored separately, per the open-core seam.
resource "helm_release" "datawerx_mesh" {
  name             = var.release_name
  namespace        = var.namespace
  create_namespace = var.create_namespace

  repository = "oci://ghcr.io/datawerx/datawerx-mesh/charts"
  chart      = "datawerx-mesh"
  version    = var.chart_version

  # Stamp this cluster's mesh identity.
  set {
    name  = "clusterID"
    value = var.cluster_id
  }

  # Reference a pre-created Secret for the node WireGuard key when given;
  # otherwise the agent falls back to an ephemeral key (dev only).
  dynamic "set" {
    for_each = var.wireguard_private_key_secret != "" ? [var.wireguard_private_key_secret] : []
    content {
      name  = "wireguard.privateKey.existingSecret"
      value = set.value
    }
  }

  # Pass through any additional values the caller supplies.
  dynamic "set" {
    for_each = var.extra_values
    content {
      name  = set.key
      value = set.value
    }
  }
}
