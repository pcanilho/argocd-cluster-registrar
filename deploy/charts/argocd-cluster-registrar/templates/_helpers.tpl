{{/*
Indentation is applied at the CALL SITE, never inside these helpers.

The helpers used to end in `nindent 4` while every caller also piped them
through `| indent 4`, so any non-empty commonLabels/commonAnnotations rendered
at 8 spaces next to literal keys at 4. That is invalid YAML, and it broke the
chart for anyone who set commonLabels at all:

  [ERROR] templates/deployment.yaml: unable to parse YAML: did not find expected key

It went unnoticed because both values default to empty, so the helpers emitted
nothing and the double indent had nothing to misplace.
*/}}
{{/*
Note the bare `{{ toYaml . }}` rather than `{{- toYaml . }}`: it keeps the
newline that `{{- with }}` would otherwise swallow. Without it the block is
appended directly onto the caller's last line, producing
`app.kubernetes.io/managed-by: Helmteam: platform`.
*/}}
{{- define "app.commonAnnotations" -}}
{{- with .Values.commonAnnotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "app.commonLabels" -}}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "app.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- include "app.commonLabels" . }}
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
