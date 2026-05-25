{{/*
Expand the name of the chart.
*/}}
{{- define "attune.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "attune.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "attune.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "attune.labels" -}}
helm.sh/chart: {{ include "attune.chart" . }}
{{ include "attune.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "attune.selectorLabels" -}}
app.kubernetes.io/name: {{ include "attune.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "attune.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "attune.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Cluster size preset defaults. Returns nothing if clusterSize is empty.
Explicit values in values.yaml always override these presets.
*/}}

{{- define "attune.replicaCount" -}}
{{- if and .Values.clusterSize (or (eq .Values.clusterSize "large") (eq .Values.clusterSize "xlarge")) }}
{{- if eq (int .Values.replicaCount) 1 }}2{{- else }}{{ .Values.replicaCount }}{{- end }}
{{- else }}
{{- .Values.replicaCount }}
{{- end }}
{{- end }}

{{/*
Resolve prometheusQPS: if clusterSize is set and prometheusQPS is at its
default (10), use the preset value. Otherwise use the explicit value.
*/}}
{{- define "attune.prometheusQPS" -}}
{{- if and .Values.clusterSize (eq (.Values.prometheusQPS | toString) "10") }}
  {{- if eq .Values.clusterSize "small" }}10
  {{- else if eq .Values.clusterSize "medium" }}20
  {{- else if eq .Values.clusterSize "large" }}40
  {{- else if eq .Values.clusterSize "xlarge" }}80
  {{- else }}{{ .Values.prometheusQPS }}
  {{- end }}
{{- else }}{{ .Values.prometheusQPS }}
{{- end }}
{{- end }}

{{/*
Resolve prometheusBurst: if clusterSize is set and prometheusBurst is at its
default (20), use the preset value. Otherwise use the explicit value.
*/}}
{{- define "attune.prometheusBurst" -}}
{{- if and .Values.clusterSize (eq (int .Values.prometheusBurst) 20) }}
  {{- if eq .Values.clusterSize "small" }}20
  {{- else if eq .Values.clusterSize "medium" }}40
  {{- else if eq .Values.clusterSize "large" }}80
  {{- else if eq .Values.clusterSize "xlarge" }}160
  {{- else }}{{ .Values.prometheusBurst }}
  {{- end }}
{{- else }}{{ .Values.prometheusBurst }}
{{- end }}
{{- end }}

{{/*
Resolve maxConcurrentReconciles: if clusterSize is set and
maxConcurrentReconciles is empty (default), use the preset value.
*/}}
{{- define "attune.maxConcurrentReconciles" -}}
{{- if and .Values.clusterSize (not .Values.maxConcurrentReconciles) }}
  {{- if eq .Values.clusterSize "small" }}1
  {{- else if eq .Values.clusterSize "medium" }}2
  {{- else if eq .Values.clusterSize "large" }}4
  {{- else if eq .Values.clusterSize "xlarge" }}8
  {{- else }}{{ .Values.maxConcurrentReconciles }}
  {{- end }}
{{- else }}{{ .Values.maxConcurrentReconciles }}
{{- end }}
{{- end }}

{{- define "attune.resources" -}}
{{- if .Values.resources }}
{{- toYaml .Values.resources }}
{{- else }}
  {{- $size := .Values.clusterSize | default "small" }}
  {{- if eq $size "small" }}
limits:
  cpu: 500m
  memory: 256Mi
requests:
  cpu: 100m
  memory: 128Mi
  {{- else if eq $size "medium" }}
limits:
  cpu: 1000m
  memory: 512Mi
requests:
  cpu: 250m
  memory: 256Mi
  {{- else if eq $size "large" }}
limits:
  cpu: 2000m
  memory: 2Gi
requests:
  cpu: 500m
  memory: 512Mi
  {{- else if eq $size "xlarge" }}
limits:
  cpu: 4000m
  memory: 4Gi
requests:
  cpu: 1000m
  memory: 1Gi
  {{- end }}
{{- end }}
{{- end }}
