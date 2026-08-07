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

{{/*
Lease name for leader election.

Derived from labelPrefix and managedBy, NOT the release name: ownership is
established purely by those two labels, so two releases differing only in name
contend for the same cluster Secrets and must therefore contend for the same
lease. Deriving from managedBy alone would over-serialise instead -- two releases
using different prefixes never collide and must not block each other.

Hashed because labelPrefix contains a "/", which is illegal in an object name,
and managedBy is a label value, which permits characters a name does not.
*/}}
{{- define "app.leaderElectionID" -}}
{{- printf "acr-%s" (printf "%s|%s" .Values.labelPrefix .Values.managedBy | sha256sum | trunc 16) -}}
{{- end }}
