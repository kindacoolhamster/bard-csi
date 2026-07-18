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
{{- end -}}
