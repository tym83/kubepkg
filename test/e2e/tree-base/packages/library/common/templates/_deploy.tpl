{{- define "common.deployment" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  labels: {app: {{ .Release.Name }}}
  annotations: {e2e.kubepkg.dev/domain: {{ dig "platform" "domain" "none" .Values.AsMap | quote }}}
spec:
  replicas: {{ .Values.replicas }}
  selector: {matchLabels: {app: {{ .Release.Name }}}}
  template:
    metadata: {labels: {app: {{ .Release.Name }}}}
    spec:
      containers:
        - name: main
          image: {{ .Values.image }}
          resources: {requests: {cpu: 5m, memory: 8Mi}}
{{- end -}}
