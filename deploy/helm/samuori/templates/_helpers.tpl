{{- define "samuori.fullname" -}}
{{- printf "%s-samuori" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "samuori.labels" -}}
app.kubernetes.io/name: samuori
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "samuori.selector" -}}
app.kubernetes.io/name: samuori
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "samuori.require" -}}
{{- $v := index . 0 -}}{{- $what := index . 1 -}}
{{- if not $v }}{{ fail (printf "%s is required" $what) }}{{ end -}}
{{- end -}}
