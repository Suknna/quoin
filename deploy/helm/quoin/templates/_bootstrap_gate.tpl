{{/* Workloads may be rendered by a real install/upgrade only after the helper
has atomically completed the secret and first-administrator bootstrap. helm
lint/template has neither operation mode, so it remains a pure render check. */}}
{{- define "quoin.requireBootstrapComplete" -}}
{{- if or .Release.IsInstall .Release.IsUpgrade -}}
{{- $marker := lookup "v1" "ConfigMap" .Release.Namespace (printf "%s-bootstrap-complete" .Release.Name) -}}
{{- if or (not $marker) (ne (default "" (index $marker.data "state")) "complete") -}}
{{- fail "quoin workloads require helper-owned bootstrap-complete ConfigMap" -}}
{{- end -}}
{{- end -}}
{{- end -}}
