{{/* Chart name. */}}
{{- define "datawerx-mesh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully-qualified app name. */}}
{{- define "datawerx-mesh.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "datawerx-mesh.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "datawerx-mesh.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Selector labels. */}}
{{- define "datawerx-mesh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "datawerx-mesh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name. */}}
{{- define "datawerx-mesh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "datawerx-mesh.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Name of the DNS Service. */}}
{{- define "datawerx-mesh.dnsServiceName" -}}
{{- default (printf "%s-dns" (include "datawerx-mesh.fullname" .)) .Values.dnsService.name -}}
{{- end -}}

{{/* Name of the chart-managed WireGuard key Secret. */}}
{{- define "datawerx-mesh.wgSecretName" -}}
{{- printf "%s-wg" (include "datawerx-mesh.fullname" .) -}}
{{- end -}}
