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
{{- /* Every enabled metrics port must be distinct. Under a hostNetwork profile
     (iscsi sets it on BOTH planes) the controller pod and a node pod share the
     host netns on the controller's node, so a duplicate port means the second
     listener never binds -- and the core only warning-logs that failure, so the
     driver keeps serving CSI while a scrape target is silently dead. Fail the
     render instead of shipping a half-observable install. */ -}}
{{- if .Values.metrics.enabled -}}
{{- $m := .Values.metrics -}}
{{- $ports := dict (printf "%d" (int $m.port)) "metrics.port" -}}
{{- $add := dict -}}
{{- $_ := set $add "metrics.nodePort" (int $m.nodePort) -}}
{{- if $m.sidecars.enabled -}}
{{-   if .Values.sidecars.provisioner.enabled }}{{ $_ := set $add "metrics.sidecars.provisionerPort" (int $m.sidecars.provisionerPort) }}{{ end -}}
{{-   if .Values.sidecars.snapshotter.enabled }}{{ $_ := set $add "metrics.sidecars.snapshotterPort" (int $m.sidecars.snapshotterPort) }}{{ end -}}
{{-   if .Values.sidecars.resizer.enabled }}{{ $_ := set $add "metrics.sidecars.resizerPort" (int $m.sidecars.resizerPort) }}{{ end -}}
{{-   if .Values.attach.enabled }}{{ $_ := set $add "metrics.sidecars.attacherPort" (int $m.sidecars.attacherPort) }}{{ end -}}
{{-   if .Values.sidecars.healthMonitor.enabled }}{{ $_ := set $add "metrics.sidecars.healthMonitorPort" (int $m.sidecars.healthMonitorPort) }}{{ end -}}
{{- end -}}
{{- range $key, $port := $add -}}
{{-   $s := printf "%d" $port -}}
{{-   if hasKey $ports $s -}}
{{-     fail (printf "metrics port %d is used by both %s and %s -- every enabled metrics port must be unique, because a hostNetwork profile puts the controller and node listeners in the same host network namespace. Change one of them." $port (index $ports $s) $key) -}}
{{-   end -}}
{{-   $_ := set $ports $s $key -}}
{{- end -}}
{{- end -}}
{{- end -}}
