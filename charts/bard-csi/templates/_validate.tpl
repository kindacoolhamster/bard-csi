{{/*
bard-csi.validate: chart-wide render-time guards. Included once (from
controller.yaml) so a structurally broken config fails the render loudly instead
of deploying a controller/node pair that will hang or crash at runtime.
Call: (include "bard-csi.validate" .) -- pass the ROOT context.
*/}}
{{- define "bard-csi.validate" -}}
{{- $iscsi := .Values.plugins.iscsi | default dict -}}
{{- if and $iscsi.enabled (gt (len (keys ($iscsi.instances | default dict))) 0) (not .Values.attach.enabled) -}}
{{- fail "plugins.iscsi requires attach.enabled=true (iSCSI is an attach-style backend). NOTE CSIDriver.attachRequired is immutable: on an existing install, kubectl delete csidriver csi.bard.io before upgrading. See charts/bard-csi/README.md." -}}
{{- end -}}
{{- /* A hostNetwork controller plugin (e.g. iscsi's profile) binds host ports
     (targetcli/LIO portals) and is pinned to one node via controller.nodeSelector,
     so a 2nd replica would either fail to schedule (same host, same ports) or -- on
     a different host -- silently not see the LIO target at all. Same $hostNetwork
     computation as controller.yaml's pod-level flag loop. */ -}}
{{- $hostNetwork := false -}}
{{- range $name, $plugin := .Values.plugins -}}
{{-   if $plugin.enabled -}}
{{-     $p := fromYaml (include "bard-csi.normalize" (dict "name" $name "plugin" $plugin "root" $)) -}}
{{-     if and $p.controller $p.controller.enabled $p.controller.hostNetwork -}}
{{-       $hostNetwork = true -}}
{{-     end -}}
{{-   end -}}
{{- end -}}
{{- if and $hostNetwork (gt (.Values.controller.replicas | int) 1) -}}
{{- fail (printf "a hostNetwork controller plugin is single-replica only (it binds host ports and is pinned to one node via controller.nodeSelector) -- set controller.replicas: 1 (got controller.replicas=%d)." (.Values.controller.replicas | int)) -}}
{{- end -}}
{{- end -}}
