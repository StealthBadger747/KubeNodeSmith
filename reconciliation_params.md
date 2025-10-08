# Autoscaler Control Plane Checklist

## Versions
- [ ] OS version on nodes matches latest (requested vs actual)
- [ ] K3s version on nodes matches control plane (requested vs actual)

## Node State / Labels
- [ ] Node labels present (`autoscaler.parawell.cloud/managed=true`, `nodegroup=...`)
- [ ] Node taints applied/cleared correctly during scale-up/scale-down
- [ ] Node template label (`template=<version>`) matches current template (drift detection)

## Scheduling / Capacity
- [ ] Pods marked Pending only due to real resource shortages (not control-plane taints)
- [ ] Unschedulable resource totals computed correctly (CPU, Memory)
- [ ] Worker node allocatable CPU/memory reported as expected
- [ ] Node’s actual allocated CPU/memory matches accounting (requested vs actual)

## Scale-Up Flow
- [ ] New node joins cluster and reports `NodeReady=True`
- [ ] New node picks up expected labels and taints on join
- [ ] Pending pods land on new node after taints cleared

## Scale-Down Flow
- [ ] Target node cordoned before removal
- [ ] Node empty of non-DaemonSet, non-mirror pods before deletion
- [ ] VolumeAttachments detached before VM deletion
- [ ] Node object deleted from API server
- [ ] VM deleted from Proxmox (tag verified)

## Drift Detection
- [ ] Nodes running with outdated template labels detected as “drifted”
- [ ] Drifted nodes flagged for replacement
