{{/* Chart name, optionally overridden. */}}
{{- define "kohen.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully-qualified app name. */}}
{{- define "kohen.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kohen.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kohen.labels" -}}
helm.sh/chart: {{ include "kohen.chart" . }}
{{ include "kohen.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kohen
{{- end -}}

{{- define "kohen.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kohen.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kohen.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kohen.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* RBAC policy rules shared by ClusterRole (cluster scope) and Role (namespaced scope). */}}
{{- define "kohen.rbacRules" -}}
- apiGroups: ["kohen.dev"]
  resources: ["configsyncs"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["kohen.dev"]
  resources: ["configsyncs/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["kohen.dev"]
  resources: ["configsyncs/finalizers"]
  verbs: ["update"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "watch", "patch"]
# ExternalSecret apply-if-present + await/resolve (Phase 2, SPEC §8.2/§8.3).
# Kohen applies (owns + prunes) recognized ExternalSecret manifests found in
# git and reads them to gate readiness; it never reads the backing Secret
# material via this role beyond the secrets get/list/watch above.
- apiGroups: ["external-secrets.io"]
  resources: ["externalsecrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
{{- end -}}

{{/* Leader-election needs lease access in the operator's own namespace. */}}
{{- define "kohen.leaderElectionRules" -}}
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
{{- end -}}

{{/* Validate scope. */}}
{{- define "kohen.scope" -}}
{{- if not (or (eq .Values.scope "cluster") (eq .Values.scope "namespaced")) -}}
{{- fail (printf "scope must be 'cluster' or 'namespaced', got %q" .Values.scope) -}}
{{- end -}}
{{- .Values.scope -}}
{{- end -}}
