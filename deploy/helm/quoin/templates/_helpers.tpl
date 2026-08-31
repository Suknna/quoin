{{/* Deterministic object names derived only from the Helm release. */}}
{{- define "quoin.labels" -}}
app.kubernetes.io/name: quoin
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "quoin.matchLabels" -}}
matchLabels:
  app.kubernetes.io/name: quoin
  app.kubernetes.io/instance: {{ .root.Release.Name }}
  app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "quoin.selector" -}}
app.kubernetes.io/name: quoin
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* The runtime TLS certificate SANs are frozen to [quoin, localhost]
(internal/quoin/bootstrap); the runtime endpoint must therefore use the
in-namespace service name "quoin". */}}
{{- define "quoin.runtimeEndpoint" -}}https://quoin:8443{{- end -}}

{{- define "quoin.secretName" -}}{{ .Release.Name }}-secrets{{- end -}}

{{/* minimumShmBytes: mechanical projection of lintelShmSize (Mi|Gi|Ti). */}}
{{- define "quoin.shmBytes" -}}
{{- $size := .Values.input.lintelShmSize -}}
{{- $count := int (regexFind "^[1-9][0-9]*" $size) -}}
{{- if hasSuffix "Ti" $size -}}
{{- mul $count 1099511627776 -}}
{{- else if hasSuffix "Gi" $size -}}
{{- mul $count 1073741824 -}}
{{- else -}}
{{- mul $count 1048576 -}}
{{- end -}}
{{- end -}}

{{- define "quoin.resources" -}}
{{- with . -}}
resources:
{{ toYaml . | indent 2 }}
{{- end -}}
{{- end -}}
