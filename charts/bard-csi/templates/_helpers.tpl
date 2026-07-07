{{/* Standard name/label helpers. */}}
{{- define "bard-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bard-csi.fullname" -}}
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

{{- define "bard-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "bard-csi.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: bard-csi
{{- end -}}

{{/* The Bard core image. */}}
{{- define "bard-csi.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
Plugin container. Call with a dict: { name, plugin, plane ("controller"|"node"), root }.
--socket is injected from .socket; plugins/dev/kubelet mounts are chart-managed and
referenced by flag; per-plugin config/secret volumes are namespaced "<name>-<vol>".
*/}}
{{- define "bard-csi.pluginContainer" -}}
{{- $name := .name -}}
{{- $plugin := .plugin -}}
{{- $plane := .plane -}}
{{- $root := .root -}}
{{- $cfg := index $plugin $plane -}}
- name: {{ $name }}-plugin
  image: "{{ $plugin.image.repository }}:{{ $plugin.image.tag | default $root.Chart.AppVersion }}"
  imagePullPolicy: {{ $plugin.image.pullPolicy | default "IfNotPresent" }}
  {{- if and (eq $plane "node") $cfg.privileged }}
  securityContext:
    privileged: true
  {{- end }}
  args:
    - "--socket=/var/lib/bard/plugins/{{ $plugin.socket }}"
    {{- range $cfg.args }}
    - {{ . | quote }}
    {{- end }}
  volumeMounts:
    - name: plugins
      mountPath: /var/lib/bard/plugins
    {{- if and (eq $plane "node") $cfg.hostDev }}
    - name: dev-dir
      mountPath: /dev
    {{- end }}
    {{- if and (eq $plane "node") $cfg.kubeletDir }}
    - name: kubelet-dir
      mountPath: {{ $root.Values.node.kubeletDir }}
      mountPropagation: Bidirectional
    {{- end }}
    {{- range $cfg.volumes }}
    - name: {{ $name }}-{{ .name }}
      mountPath: {{ .mountPath }}
      {{- if .readOnly }}
      readOnly: true
      {{- end }}
      {{- if .mountPropagation }}
      mountPropagation: {{ .mountPropagation }}
      {{- end }}
    {{- end }}
  {{- with $cfg.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

{{/*
Pod-level volumes a plugin contributes for a plane (its config/secret references).
Call with: { name, plugin, plane }. Names are "<plugin>-<vol>" to avoid collisions.
*/}}
{{- define "bard-csi.pluginVolumes" -}}
{{- $name := .name -}}
{{- $plugin := .plugin -}}
{{- $cfg := index $plugin .plane -}}
{{- range $cfg.volumes }}
- name: {{ $name }}-{{ .name }}
  {{- if .configMap }}
  configMap:
    name: {{ .configMap.name }}
    {{- if .configMap.optional }}
    optional: true
    {{- end }}
  {{- else if .secret }}
  secret:
    secretName: {{ .secret.name }}
    {{- if .secret.optional }}
    optional: true
    {{- end }}
  {{- else if .hostPath }}
  hostPath:
    path: {{ .hostPath.path }}
    {{- with .hostPath.type }}
    type: {{ . }}
    {{- end }}
  {{- else if .emptyDir }}
  emptyDir: {}
  {{- end }}
{{- end }}
{{- end -}}
