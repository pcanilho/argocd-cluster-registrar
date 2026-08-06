---
{{- define "app.commonAnnotations" -}}
{{- with .Values.commonAnnotations | default dict }}
{{- toYaml . | nindent 4 }}
{{- end }}
{{- end }}

{{- define "app.commonLabels" -}}
{{- with .Values.commonLabels }}
{{- toYaml . | nindent 4 }}
{{- end }}
{{- end }}

{{- define "app.labels" -}}
{{- include "app.commonLabels" . }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "app.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "app.fullname" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end }}

{{- define "app.serviceAccountName" -}}
{{ .Release.Name }}-{{ .Chart.Name }}-sa
{{- end }}
