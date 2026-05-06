{{/*
Return the chart name, truncated and trimmed to DNS-1123 limits.
*/}}
{{- define "incidentary-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Return the fully qualified release name.
*/}}
{{- define "incidentary-operator.fullname" -}}
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

{{/*
Chart.yaml name-version label.
*/}}
{{- define "incidentary-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard Kubernetes recommended labels.
*/}}
{{- define "incidentary-operator.labels" -}}
helm.sh/chart: {{ include "incidentary-operator.chart" . }}
{{ include "incidentary-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: incidentary
{{- end -}}

{{/*
Selector labels — stable across upgrades.
*/}}
{{- define "incidentary-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "incidentary-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Service account name.
*/}}
{{- define "incidentary-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "incidentary-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
API-key Secret name. Prefers an existing secret when set.
*/}}
{{- define "incidentary-operator.apiKeySecretName" -}}
{{- if .Values.existingSecretName -}}
{{- .Values.existingSecretName -}}
{{- else -}}
{{- printf "%s-apikey" (include "incidentary-operator.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
API-key Secret key. Defaults to "token".
*/}}
{{- define "incidentary-operator.apiKeySecretKey" -}}
{{- default "token" .Values.existingSecretKey -}}
{{- end -}}
