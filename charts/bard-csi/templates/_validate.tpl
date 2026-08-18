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
     a different host -- silently not see the LIO target at all. The controller
     and node computations below match their respective pod-level flag loops. */ -}}
{{- $controllerHostNetwork := false -}}
{{- $nodeHostNetwork := false -}}
{{- range $name, $plugin := .Values.plugins -}}
{{-   if $plugin.enabled -}}
{{-     $p := fromYaml (include "bard-csi.normalize" (dict "name" $name "plugin" $plugin "root" $)) -}}
{{-     if and $p.controller $p.controller.enabled $p.controller.hostNetwork -}}
{{-       $controllerHostNetwork = true -}}
{{-     end -}}
{{-     if and $p.node $p.node.enabled $p.node.hostNetwork -}}
{{-       $nodeHostNetwork = true -}}
{{-     end -}}
{{-   end -}}
{{- end -}}
{{- if and $controllerHostNetwork (gt (.Values.controller.replicas | int) 1) -}}
{{- fail (printf "a hostNetwork controller plugin is single-replica only (it binds host ports and is pinned to one node via controller.nodeSelector) -- set controller.replicas: 1 (got controller.replicas=%d)." (.Values.controller.replicas | int)) -}}
{{- end -}}
{{- $sharedHostNetwork := and $controllerHostNetwork $nodeHostNetwork -}}
{{- /* Every enabled metrics listener must be globally unique. Fixed listeners
     in the same Pod are always a conflict; fixed listeners in the other plane
     conflict only when a hostNetwork profile puts both Pods in one namespace.
     A duplicate otherwise only warning-logs at bind time, leaving CSI serving
     while observability or csi-addons is silently dead. */ -}}
{{- if .Values.metrics.enabled -}}
{{- $m := .Values.metrics -}}
{{- $metrics := dict "metrics.port" (int $m.port) "metrics.nodePort" (int $m.nodePort) -}}
{{- $controllerMetrics := dict "metrics.port" (int $m.port) -}}
{{- $nodeMetrics := dict "metrics.nodePort" (int $m.nodePort) -}}
{{- if $m.sidecars.enabled -}}
{{-   if .Values.sidecars.provisioner.enabled }}{{ $_ := set $metrics "metrics.sidecars.provisionerPort" (int $m.sidecars.provisionerPort) }}{{ $_ := set $controllerMetrics "metrics.sidecars.provisionerPort" (int $m.sidecars.provisionerPort) }}{{ end -}}
{{-   if .Values.sidecars.snapshotter.enabled }}{{ $_ := set $metrics "metrics.sidecars.snapshotterPort" (int $m.sidecars.snapshotterPort) }}{{ $_ := set $controllerMetrics "metrics.sidecars.snapshotterPort" (int $m.sidecars.snapshotterPort) }}{{ end -}}
{{-   if .Values.sidecars.resizer.enabled }}{{ $_ := set $metrics "metrics.sidecars.resizerPort" (int $m.sidecars.resizerPort) }}{{ $_ := set $controllerMetrics "metrics.sidecars.resizerPort" (int $m.sidecars.resizerPort) }}{{ end -}}
{{-   if .Values.attach.enabled }}{{ $_ := set $metrics "metrics.sidecars.attacherPort" (int $m.sidecars.attacherPort) }}{{ $_ := set $controllerMetrics "metrics.sidecars.attacherPort" (int $m.sidecars.attacherPort) }}{{ end -}}
{{-   if .Values.sidecars.healthMonitor.enabled }}{{ $_ := set $metrics "metrics.sidecars.healthMonitorPort" (int $m.sidecars.healthMonitorPort) }}{{ $_ := set $controllerMetrics "metrics.sidecars.healthMonitorPort" (int $m.sidecars.healthMonitorPort) }}{{ end -}}
{{- end -}}

{{- /* Keep every metrics listener globally unique even when the two Pods have
     separate network namespaces. */ -}}
{{- $seenMetrics := dict -}}
{{- range $key, $port := $metrics -}}
{{-   $s := printf "%d" $port -}}
{{-   if hasKey $seenMetrics $s -}}
{{-     fail (printf "metrics listener %s uses port %d, already used by %s; every enabled metrics listener must use a unique port. Change %s." $key $port (index $seenMetrics $s) $key) -}}
{{-   end -}}
{{-   $_ := set $seenMetrics $s $key -}}
{{- end -}}

{{- $controllerFixed := dict -}}
{{- $nodeFixed := dict -}}
{{- if .Values.sidecars.livenessProbe.enabled -}}
{{- $_ := set $controllerFixed "9808" "sidecars.livenessProbe health port" -}}
{{- end -}}
{{- if .Values.sidecars.csiAddons.enabled -}}
{{- $_ := set $controllerFixed "9071" "sidecars.csiAddons controller port" -}}
{{- $_ := set $nodeFixed "9070" "sidecars.csiAddons node controller port" -}}
{{- end -}}

{{- range $key, $port := $controllerMetrics -}}
{{-   $s := printf "%d" $port -}}
{{-   if hasKey $controllerFixed $s -}}
{{-     fail (printf "metrics listener %s uses port %d, already used by %s; change %s to a free port." $key $port (index $controllerFixed $s) $key) -}}
{{-   end -}}
{{- end -}}
{{- range $key, $port := $nodeMetrics -}}
{{-   $s := printf "%d" $port -}}
{{-   if hasKey $nodeFixed $s -}}
{{-     fail (printf "metrics listener %s uses port %d, already used by %s; change %s to a free port." $key $port (index $nodeFixed $s) $key) -}}
{{-   end -}}
{{- end -}}
{{- if $sharedHostNetwork -}}
{{-   range $key, $port := $controllerMetrics -}}
{{-     $s := printf "%d" $port -}}
{{-     if hasKey $nodeFixed $s -}}
{{-       fail (printf "metrics listener %s uses port %d, already used by %s across hostNetwork planes; change %s to a free port." $key $port (index $nodeFixed $s) $key) -}}
{{-     end -}}
{{-   end -}}
{{-   range $key, $port := $nodeMetrics -}}
{{-     $s := printf "%d" $port -}}
{{-     if hasKey $controllerFixed $s -}}
{{-       fail (printf "metrics listener %s uses port %d, already used by %s across hostNetwork planes; change %s to a free port." $key $port (index $controllerFixed $s) $key) -}}
{{-     end -}}
{{-   end -}}
{{- end -}}
{{- end -}}
{{- end -}}
