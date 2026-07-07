{{/*
Backend profiles + normalization. This is the abstraction layer: a backend expert
configures a first-party plugin with `instances` (mons/pool/fsName/user/zone) and
the chart fills in all the Bard-internal plumbing (image, socket, --config/--key-dir
args, volume mounts, host flags). A plugin WITHOUT `instances` is passed through
unchanged -- the advanced "bring your own plugin" override path (explicit
socket/args/volumes still work exactly as before).
*/}}

{{/*
bard-csi.profile: the static per-backend plumbing, keyed by backend type (the
plugins map key). Returns YAML; callers `fromYaml` it. configMount holds config.yaml;
keysMount is --key-dir. encryptionMount/kmsMount/vaultMount present only for backends
that support them. node.* are the host flags that plane needs.
*/}}
{{- define "bard-csi.profile" -}}
{{- $type := . -}}
{{- if eq $type "ceph-rbd" -}}
image: ghcr.io/kindacoolhamster/bard-plugin-ceph-rbd
socket: ceph-rbd.sock
configMap: bard-ceph-config
configMount: /etc/bard-ceph
keysMount: /etc/bard-ceph-keys
keysSecret: bard-ceph-keys
encryptionMount: /etc/bard-ceph-encryption
encryptionSecret: bard-ceph-encryption
kmsMount: /etc/bard-ceph-kms
vaultMount: /etc/bard-ceph-vault
controller: { privileged: false, hostDev: false, hostNetwork: false, hostPID: false, kubeletDir: false }
node:       { privileged: true,  hostDev: true,  hostNetwork: true,  hostPID: true,  kubeletDir: true }
{{- else if eq $type "cephfs" -}}
image: ghcr.io/kindacoolhamster/bard-plugin-cephfs
socket: cephfs.sock
configMap: bard-cephfs-config
configMount: /etc/bard-cephfs
keysMount: /etc/bard-cephfs-keys
keysSecret: bard-cephfs-keys
encryptionMount: /etc/bard-cephfs-encryption
encryptionSecret: bard-cephfs-encryption
kmsMount: /etc/bard-cephfs-kms
vaultMount: /etc/bard-cephfs-vault
controller: { privileged: false, hostDev: false, hostNetwork: false, hostPID: false, kubeletDir: false }
node:       { privileged: true,  hostDev: true,  hostNetwork: true,  hostPID: false, kubeletDir: true }
{{- end -}}
{{- end -}}

{{/*
bard-csi.usesProfile: "true" when a plugin should be profile-driven -- it has a
non-empty `instances` map AND a known backend type. Empty otherwise.
Call: (dict "name" $type "plugin" $plugin).
*/}}
{{- define "bard-csi.usesProfile" -}}
{{- $p := .plugin -}}
{{- $prof := include "bard-csi.profile" .name -}}
{{- if and $prof $p.instances (gt (len (keys ($p.instances | default dict))) 0) -}}
true
{{- end -}}
{{- end -}}

{{/*
bard-csi.normalize: a raw plugin (values) -> the effective plugin in the low-level
shape (socket/image/controller{enabled,args,volumes}/node{enabled,host flags,args,
volumes}). Profile-driven when `instances` is set; otherwise echoes the plugin
unchanged (override path). Returns YAML; callers `fromYaml` it.
Call: (dict "name" $type "plugin" $plugin "root" $).
*/}}
{{- define "bard-csi.normalize" -}}
{{- $type := .name -}}
{{- $plugin := .plugin -}}
{{- if not (include "bard-csi.usesProfile" (dict "name" $type "plugin" $plugin)) -}}
{{- toYaml $plugin -}}
{{- else -}}
{{- $prof := fromYaml (include "bard-csi.profile" $type) -}}
{{- $img := $plugin.image | default dict -}}
{{- $keysSecret := $plugin.keysSecret | default $prof.keysSecret -}}
{{- $kms := $plugin.kms -}}
{{- /* base args + volumes both planes share */ -}}
{{- $baseArgs := list (printf "--config=%s/config.yaml" $prof.configMount) (printf "--key-dir=%s" $prof.keysMount) -}}
{{- $cfgVol := dict "name" "config" "mountPath" $prof.configMount "readOnly" true "configMap" (dict "name" $prof.configMap) -}}
{{- $keysVol := dict "name" "keys" "mountPath" $prof.keysMount "readOnly" true "secret" (dict "name" $keysSecret) -}}
{{- $baseVols := list $cfgVol $keysVol -}}
{{- /* kms (gated on a kms block -- this is why no kms => no --kms-config => no crashloop) */ -}}
{{- $kmsArgs := list -}}
{{- $kmsVols := list -}}
{{- if and $kms $prof.kmsMount -}}
{{-   $kmsArgs = list (printf "--kms-config=%s/config.yaml" $prof.kmsMount) -}}
{{-   $kmsVols = list (dict "name" "kms" "mountPath" $prof.kmsMount "readOnly" true "configMap" (dict "name" $kms.configMap "optional" true)) -}}
{{-   if $kms.vaultTokenSecret -}}
{{-     $kmsVols = append $kmsVols (dict "name" "vault-token" "mountPath" $prof.vaultMount "readOnly" true "secret" (dict "name" $kms.vaultTokenSecret "optional" true)) -}}
{{-   end -}}
{{- end -}}
{{- /* controller plane: config + keys (+ kms). No privilege/host. */ -}}
{{- $cArgs := concat $baseArgs $kmsArgs -}}
{{- $cVols := concat $baseVols $kmsVols -}}
{{- $controller := dict "enabled" true "args" $cArgs "volumes" $cVols -}}
{{- /* node plane: base (+ encryption, always optional) (+ kms) + host flags */ -}}
{{- $nArgs := $baseArgs -}}
{{- $nVols := $baseVols -}}
{{- if $prof.encryptionMount -}}
{{-   $encSecret := (default (dict) $plugin.encryption).masterKeySecret | default $prof.encryptionSecret -}}
{{-   $nArgs = append $nArgs (printf "--encryption-key-dir=%s" $prof.encryptionMount) -}}
{{-   $nVols = append $nVols (dict "name" "encryption-keys" "mountPath" $prof.encryptionMount "readOnly" true "secret" (dict "name" $encSecret "optional" true)) -}}
{{- end -}}
{{- $nArgs = concat $nArgs $kmsArgs -}}
{{- $nVols = concat $nVols $kmsVols -}}
{{- $node := merge (dict "enabled" true "args" $nArgs "volumes" $nVols) $prof.node -}}
{{- $out := dict "enabled" true "socket" $prof.socket "image" (dict "repository" ($img.repository | default $prof.image) "tag" ($img.tag | default "") "pullPolicy" ($img.pullPolicy | default "IfNotPresent")) "controller" $controller "node" $node -}}
{{- toYaml $out -}}
{{- end -}}
{{- end -}}

{{/*
bard-csi.instanceConfig: render a plugin's config.yaml `instances:` block from the
high-level `instances` map, mapping backend-native fields to the plugin's schema
(user -> userID, etc.). Call: (dict "name" $type "instances" $instances).
*/}}
{{- define "bard-csi.instanceConfig" -}}
{{- $type := .name -}}
instances:
{{- range $id, $inst := .instances }}
  {{ $id }}:
    monitors: {{ toJson $inst.monitors }}
    {{- if eq $type "ceph-rbd" }}
    pool: {{ $inst.pool }}
    {{- else if eq $type "cephfs" }}
    fsName: {{ $inst.fsName }}
    {{- end }}
    userID: {{ $inst.user }}
    {{- with $inst.mounter }}
    mounter: {{ . }}
    {{- end }}
    {{- if eq $type "ceph-rbd" }}
    {{- with $inst.radosNamespace }}
    radosNamespace: {{ . }}
    {{- end }}
    {{- if $inst.readAffinity }}
    readAffinity: true
    {{- end }}
    {{- with $inst.clusterName }}
    clusterName: {{ . }}
    {{- end }}
    {{- end }}
    {{- if eq $type "cephfs" }}
    {{- with $inst.subvolumeGroup }}
    subvolumeGroup: {{ . }}
    {{- end }}
    {{- with $inst.nfsCluster }}
    nfsCluster: {{ . }}
    {{- end }}
    {{- with $inst.nfsServer }}
    nfsServer: {{ . }}
    {{- end }}
    {{- end }}
{{- end }}
{{- end -}}
