{{- define "kubepkg.labels" -}}
app.kubernetes.io/name: kubepkg
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "kubepkg.selector" -}}
app.kubernetes.io/name: kubepkg
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
