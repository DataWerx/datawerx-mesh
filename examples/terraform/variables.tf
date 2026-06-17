variable "cluster_id" {
  description = "Stable, globally unique mesh ID for this cluster."
  type        = string
}

variable "chart_version" {
  description = "DataWerx Mesh Helm chart version to install."
  type        = string
  default     = "0.1.0"
}

variable "namespace" {
  description = "Namespace the agent is installed into."
  type        = string
  default     = "datawerx-system"
}

variable "create_namespace" {
  description = "Whether to create the namespace as part of the release."
  type        = bool
  default     = true
}

variable "release_name" {
  description = "Helm release name."
  type        = string
  default     = "datawerx-mesh"
}

variable "wireguard_private_key_secret" {
  description = "Name of an existing Secret holding the node WireGuard private key. Empty uses ephemeral, per-process keys (not recommended for production)."
  type        = string
  default     = ""
}

variable "extra_values" {
  description = "Additional chart values, applied as individual Helm set entries."
  type        = map(string)
  default     = {}
}
