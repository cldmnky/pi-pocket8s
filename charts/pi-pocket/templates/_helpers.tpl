{{- define "pocket.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "pocket.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else if contains (include "pocket.name" .) .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "pocket.name" .) | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- define "pocket.labels" -}}
app.kubernetes.io/name: {{ include "pocket.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}
{{- define "pocket.runtimeSecret" -}}
{{- default (printf "%s-runtime" (include "pocket.fullname" .)) .Values.runtimeSecret.existingSecret -}}
{{- end -}}
{{- define "pocket.tokenSecret" -}}
{{- default (printf "%s-portal-token" (include "pocket.fullname" .)) .Values.portal.tokenSecret -}}
{{- end -}}
# RuntimeDefault seccomp blocks nested user namespaces (unshare/clone3 return
# ENOSYS), which rootless Podman needs. Unconfined is required for the
# nested-container profile (verified live); restricted-v3 only allows
# RuntimeDefault, so the hardened profile keeps it and loses nested Podman.
{{- define "pocket.seccomp" -}}
{{- if .Values.podSeccomp -}}
{{- .Values.podSeccomp -}}
{{- else if eq .Values.openshift.pocketSCC "nested-container" -}}
Unconfined
{{- else -}}
RuntimeDefault
{{- end -}}
{{- end -}}
{{- define "pocket.claim" -}}
{{- default (printf "%s-workspace" (include "pocket.fullname" .)) .Values.persistence.existingClaim -}}
{{- end -}}
