{{- define "kubepkg.labels" -}}
app.kubernetes.io/name: kubepkg
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "kubepkg.selector" -}}
app.kubernetes.io/name: kubepkg
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- /* kubepkg.seconds turns a duration such as 168h or 30m into seconds. */ -}}
{{- define "kubepkg.seconds" -}}
{{- $d := toString . -}}
{{- if hasSuffix "h" $d -}}{{ mul (trimSuffix "h" $d | int) 3600 }}
{{- else if hasSuffix "m" $d -}}{{ mul (trimSuffix "m" $d | int) 60 }}
{{- else -}}{{ trimSuffix "s" $d | int }}
{{- end -}}
{{- end -}}
