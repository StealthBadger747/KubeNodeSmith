# Autoscaler Configuration

This directory contains example configuration files for the KubeNodeSmith autoscaler. The
`scaler-config.yaml` document models how the controller should talk to infrastructure providers and
describes the node pools it may manage. Right now the file is consumed via ConfigMap; the layout is
kept close to a future CRD so the migration path stays clear.

## Document Layout

```yaml
schemaVersion: v1alpha1
pollingInterval: 30s
providers: {}
nodePools: []
```

- `schemaVersion` lets the loader enforce compatible configuration shapes (`v1alpha1` for now).
- `pollingInterval` controls how frequently the autoscaler wakes up to evaluate scale up/down
  opportunities.
- `providers` is a map keyed by a friendly provider name.
- `nodePools` is an array of the environments this autoscaler instance is allowed to scale.

## Providers Block

Each provider entry declares the settings the autoscaler needs to talk to a backend API (Proxmox,
Redfish, bare metal, etc.). Example:

```yaml
providers:
  proxmox-production:
    type: proxmox
    credentialsRef:
      name: proxmox-api-secret
      namespace: kubenodesmith
    options:
      endpoint: https://100.64.0.25:8006/api2/json
      nodeWhitelist:
        - alfaromeo
        - porsche
      vmIDRange:
        lower: 1250
        upper: 1300
      vmMemOverheadMiB: 2046
      vmTags:
        - zagato-k3s-auto
      networkInterfaces:
        - name: net0
          model: virtio
          bridge: vmbr0
          vlanTag: 20
          macPrefix: "02:00:00"
      vmOptions:
        - name: sockets
          value: 1
        - name: cpu
          value: host
        - name: boot
          value: order=net0
        - name: ostype
          value: l26
        - name: ipconfig0
          value: ip=dhcp
        - name: ide2
          value: local-zfs:cloudinit
        - name: scsi0
          value: local-zfs:16
        - name: cicustom
          value: meta=snippets:snippets/zagato-shared-cloud-init-meta-data.yaml
```

Key fields:

- `type` – selects the implementation (for now `proxmox`, `redfish`, or any future backend).
- `credentialsRef` points at the Kubernetes Secret the controller should read; it still works when the
  config comes from a ConfigMap because the autoscaler code will fetch the actual Secret at runtime.
- `options` is a provider-scoped map. Each provider can document and decode the keys it cares about
  (e.g. Proxmox uses `endpoint`, `nodeWhitelist`, VM identity ranges, memory overhead, tag lists, and bootstrap hints).
- Common Proxmox keys include:
  - `nodeWhitelist` – allowable nodes when scheduling new VMs.
  - `vmIDRange.lower`/`vmIDRange.upper` – inclusive VMID bounds.
  - `vmMemOverheadMiB` – memory overhead to add during sizing.
  - `vmTags` – list of tags to stamp onto created VMs.
  - `networkInterfaces` – structured so the controller can synthesize MAC addresses while you pick bridge/VLAN.
  - `vmOptions` – raw `name`/`value` pairs passed straight to Proxmox for anything else (including `tags=...`; the controller merges these with `vmTags` and pool tags).

## Node Pools Block

A node pool combines scaling guardrails, the desired machine shape, and a pointer to which provider
should handle the work.

```yaml
nodePools:
  - name: proxmox-small
    providerRef: proxmox-production
    limits:
      minNodes: 0
      maxNodes: 5
    machineTemplate:
      kubeNodeNamePrefix: zagato-worker-auto
      cpuCores: 4
      memoryMiB: 6144
      architecture: amd64
      tags:
        provider-tag: zagato-k3s-auto
      labels:
        node-role.kubernetes.io/worker: ""
        topology.kubenodesmith.io/pool: proxmox-small
    scaleUp:
      batchSize: 1
      stabilizationWindow: 2m
    scaleDown:
      maxConcurrent: 1
      drainTimeout: 10m
```

Details:

- `providerRef` must match one of the keys declared in the `providers` map.
- `limits` places hard bounds on how many machines this pool can manage.
- `machineTemplate` maps onto the `provider.MachineSpec` structure:
  - `kubeNodeNamePrefix` keeps the Kubernetes node names stable.
  - `cpuCores`, `memoryMiB`, `architecture` define the node shape.
  - `tags` carry provider metadata.
  - `labels` are Kubernetes node labels to apply post-bootstrap.
- `scaleUp` / `scaleDown` expose pacing knobs you can tune later.

## Future CRD Migration

When you are ready to promote this config into a CRD, the same schema can be nested under
`spec` and versioned using a domain you control (e.g. `autoscaler.yourcompany.com`). Owning the domain
is recommended but not required; any globally unique DNS suffix works for CRD API groups. Until then
this ConfigMap-based file keeps things lightweight while letting the interface solidify.

These files are not yet consumed by the controller—they serve as design targets for the eventual
implementation. Adjust them freely.
