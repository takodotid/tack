{{- define "tack.labels" -}}
app.kubernetes.io/name: tack
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}
