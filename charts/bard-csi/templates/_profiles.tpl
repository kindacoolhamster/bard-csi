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
{{- else if eq $type "iscsi" -}}
image: ghcr.io/kindacoolhamster/bard-plugin-iscsi
socket: iscsi.sock
configMap: bard-iscsi-config
configMount: /etc/bard-iscsi
chapMount: /etc/bard-iscsi-chap
chapSecret: bard-iscsi-chap
# targetd JSON-RPC credentials (management: targetd instances only) are read
# ONLY by controller-plane RPCs (CreateVolume/.../ControllerPublish -- the
# node plane never builds a tdManager), so unlike chapMount this is wired
# controller-side only below, not into both planes.
targetdMount: /etc/bard-iscsi-targetd
targetdSecret: bard-iscsi-targetd
controller: { privileged: true, hostDev: true, hostNetwork: true, hostPID: false, kubeletDir: false }
node:
  privileged: true
  hostDev: true
  hostNetwork: true
  hostPID: false
  kubeletDir: true
  env:
    - name: NODE_ID
      fieldRef: { fieldPath: spec.nodeName }
# Control plane: lvcreate carves the LUN block device and targetcli drives the
# host kernel's LIO configfs target -- hostDev above covers /dev, these cover the
# rest (lvm2's lock/config dirs, configfs, and the D-Bus targetcli 2.1.5x
# unconditionally enumerates tcmu-runner over -- see CLAUDE.md).
controllerVolumes:
  - name: lvm-run
    mountPath: /run/lvm
    hostPath: { path: /run/lvm }
  - name: lvm-etc
    mountPath: /etc/lvm
    hostPath: { path: /etc/lvm }
  - name: configfs
    mountPath: /sys/kernel/config
    hostPath: { path: /sys/kernel/config }
  - name: dbus
    mountPath: /run/dbus
    readOnly: true
    hostPath: { path: /run/dbus }
nodeVolumes:
  # Widest hostPath in the chart: iscsiadm runs chrooted here (--iscsiadm-chroot
  # below) because iscsiadm+iscsid are a version-matched pair with distro-specific
  # DB paths -- a container iscsiadm driving the host's iscsid writes CHAP/node
  # records the daemon never looks at, and login hangs in negotiation (CLAUDE.md).
  - name: host-root
    mountPath: /host
    hostPath: { path: / }
  # Session state must survive plugin pod restarts -- NodeUnstage reads it to log
  # the session out; without persistence a mid-lifetime restart leaks the session.
  - name: iscsi-state
    mountPath: /var/lib/bard/iscsi
    hostPath: { path: /var/lib/bard/iscsi, type: DirectoryOrCreate }
nodeArgs:
  - --node-id=$(NODE_ID)
  - --iscsiadm-chroot=/host
  - --state-dir=/var/lib/bard/iscsi
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
{{- $baseArgs := list (printf "--config=%s/config.yaml" $prof.configMount) -}}
{{- $cfgVol := dict "name" "config" "mountPath" $prof.configMount "readOnly" true "configMap" (dict "name" $prof.configMap) -}}
{{- $baseVols := list $cfgVol -}}
{{- /* keys (gated on a keysMount -- some plugins, e.g. iSCSI, have no --key-dir flag; a
     forced --key-dir would crash-loop a Go flag.ExitOnError binary that lacks it) */ -}}
{{- if $prof.keysMount -}}
{{-   $baseArgs = append $baseArgs (printf "--key-dir=%s" $prof.keysMount) -}}
{{-   $baseVols = append $baseVols (dict "name" "keys" "mountPath" $prof.keysMount "readOnly" true "secret" (dict "name" $keysSecret)) -}}
{{- end -}}
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
{{- /* chap (gated on chapMount+chapSecret profile fields -- optional across BOTH planes,
     e.g. iSCSI's per-instance CHAP credentials) */ -}}
{{- $chapArgs := list -}}
{{- $chapVols := list -}}
{{- if and $prof.chapMount $prof.chapSecret -}}
{{-   $chapArgs = list (printf "--chap-dir=%s" $prof.chapMount) -}}
{{-   $chapVols = list (dict "name" "chap" "mountPath" $prof.chapMount "readOnly" true "secret" (dict "name" $prof.chapSecret "optional" true)) -}}
{{- end -}}
{{- /* targetd (gated on targetdMount+targetdSecret profile fields -- CONTROLLER PLANE
     ONLY, unlike chap/kms above: only controller-side RPCs ever read these creds) */ -}}
{{- $targetdArgs := list -}}
{{- $targetdVols := list -}}
{{- if and $prof.targetdMount $prof.targetdSecret -}}
{{-   $targetdArgs = list (printf "--targetd-dir=%s" $prof.targetdMount) -}}
{{-   $targetdVols = list (dict "name" "targetd" "mountPath" $prof.targetdMount "readOnly" true "secret" (dict "name" $prof.targetdSecret "optional" true)) -}}
{{- end -}}
{{- /* controller plane: base (+ keys) (+ kms) (+ chap) (+ targetd) (+ profile extras);
     host flags come from $prof.controller (privileged/hostDev/... -- merged below). */ -}}
{{- $cArgs := concat (concat $baseArgs $kmsArgs) $chapArgs -}}
{{- $cArgs = concat $cArgs $targetdArgs -}}
{{- $cArgs = concat $cArgs ($prof.controllerArgs | default list) -}}
{{- $cVols := concat (concat $baseVols $kmsVols) $chapVols -}}
{{- $cVols = concat $cVols $targetdVols -}}
{{- $cVols = concat $cVols ($prof.controllerVolumes | default list) -}}
{{- $controller := merge (dict "enabled" true "args" $cArgs "volumes" $cVols) $prof.controller -}}
{{- /* node plane: base (+ encryption, always optional) (+ kms) (+ chap) (+ profile
     extras) + host flags */ -}}
{{- $nArgs := $baseArgs -}}
{{- $nVols := $baseVols -}}
{{- if $prof.encryptionMount -}}
{{-   $encSecret := (default (dict) $plugin.encryption).masterKeySecret | default $prof.encryptionSecret -}}
{{-   $nArgs = append $nArgs (printf "--encryption-key-dir=%s" $prof.encryptionMount) -}}
{{-   $nVols = append $nVols (dict "name" "encryption-keys" "mountPath" $prof.encryptionMount "readOnly" true "secret" (dict "name" $encSecret "optional" true)) -}}
{{- end -}}
{{- $nArgs = concat $nArgs $kmsArgs -}}
{{- $nVols = concat $nVols $kmsVols -}}
{{- $nArgs = concat $nArgs $chapArgs -}}
{{- $nVols = concat $nVols $chapVols -}}
{{- $nArgs = concat $nArgs ($prof.nodeArgs | default list) -}}
{{- $nVols = concat $nVols ($prof.nodeVolumes | default list) -}}
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
    {{- if eq $type "ceph-rbd" }}
    monitors: {{ toJson $inst.monitors }}
    pool: {{ $inst.pool }}
    userID: {{ $inst.user }}
    {{- with $inst.mounter }}
    mounter: {{ . }}
    {{- end }}
    {{- with $inst.radosNamespace }}
    radosNamespace: {{ . }}
    {{- end }}
    {{- if $inst.readAffinity }}
    readAffinity: true
    {{- end }}
    {{- with $inst.clusterName }}
    clusterName: {{ . }}
    {{- end }}
    {{- else if eq $type "cephfs" }}
    monitors: {{ toJson $inst.monitors }}
    fsName: {{ $inst.fsName }}
    userID: {{ $inst.user }}
    {{- with $inst.mounter }}
    mounter: {{ . }}
    {{- end }}
    {{- with $inst.subvolumeGroup }}
    subvolumeGroup: {{ . }}
    {{- end }}
    {{- with $inst.nfsCluster }}
    nfsCluster: {{ . }}
    {{- end }}
    {{- with $inst.nfsServer }}
    nfsServer: {{ . }}
    {{- end }}
    {{- else if eq $type "iscsi" }}
    {{- /* vg is required for a local (targetcli-managed) instance and absent
         for a targetd one (its storage pool is targetdPool, remote) -- unlike
         the unconditional local fields below, this MUST be gated or a
         targetd instance's omitted vg renders the literal "<no value>". */}}
    {{- with $inst.vg }}
    vg: {{ . }}
    {{- end }}
    portal: {{ $inst.portal }}
    {{- with $inst.portals }}
    portals: {{ toJson . }}
    {{- end }}
    {{- with $inst.iqnBase }}
    iqnBase: {{ . }}
    {{- end }}
    {{- with $inst.thinPool }}
    thinPool: {{ . }}
    {{- end }}
    {{- if $inst.chapAuth }}
    chapAuth: true
    {{- end }}
    {{- with $inst.management }}
    management: {{ . }}
    {{- end }}
    {{- with $inst.targetdEndpoint }}
    targetdEndpoint: {{ . }}
    {{- end }}
    {{- with $inst.targetdPool }}
    targetdPool: {{ . }}
    {{- end }}
    {{- with $inst.targetIqn }}
    targetIqn: {{ . }}
    {{- end }}
    {{- end }}
{{- end }}
{{- end -}}
