output "release_name" {
  description = "The installed Helm release name."
  value       = helm_release.datawerx_mesh.name
}

output "namespace" {
  description = "The namespace the agent runs in."
  value       = helm_release.datawerx_mesh.namespace
}

output "chart_version" {
  description = "The chart version that was installed."
  value       = helm_release.datawerx_mesh.version
}
