terraform {
  required_version = ">= 1.5"

  required_providers {
    # Pinned below helm 3.0: the module uses the `set { }` block form of
    # helm_release, which the v3 provider replaced with a `set = [...]` attribute.
    helm = {
      source  = "hashicorp/helm"
      version = ">= 2.12, < 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.25, < 3.0"
    }
  }
}
