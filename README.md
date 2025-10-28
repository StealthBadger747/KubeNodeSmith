# KubeNodeSmith

KubeNodeSmith is my home-lab friendly autoscaler. It watches Kubernetes for pods that can’t get a seat, asks Proxmox for a new worker, waits for the node to come online, labels it, and later tears it back down when it’s just burning watts. No cloud provider API, no managed magic—just the Kubernetes API, your infrastructure, and some glue.

> 🛠️ **Still a prototype.** Expect sharp edges, `panic` calls in a few spots, and plenty of TODOs. Check `TODO.md` for the roadmap.

---

## What’s in the repo?

- `cmd/nodesmith` – the main event. Polls the cluster, talks to providers, labels nodes.
- `apis` – Go types for the Kubernetes CRDs.
- `internal/config` – CRD loader with validation and duration helpers.
- `internal/kube.go` – all the Kubernetes querying, labelling, and resource math.
- `internal/provider` – provider interface + the current Proxmox implementation.
- `manifests` – CRDs, deployment YAML (plain or Argo CD), sample autoscaler resources, and “stress the scheduler” workloads.

---

## Prerequisites

- A base image that works over netboot that will join the cluster by itself on boot
  - I'm working on publishing an example repo with a NixOS image that is built by nix. It will also include the dhcp and netboot configuration server that is needed and a guide to integrate it into UniFi.
- For Proxmox:
  - API token with rights to create/delete VMs and inspect cluster resources.
  - Kubernetes secret with keys `PROXMOX_TOKEN_ID`, `PROXMOX_SECRET`, and optionally `PROXMOX_SKIP_TLS_VERIFY`.
  - Secret normally lives in the `kubenodesmith` namespace.

---

## Running it locally

Using Nix? Drop into the dev shell and run it inline once the CRDs and resources are in place:

```bash
nix develop
kubectl apply -f manifests/crds/
kubectl apply -f manifests/app/nodesmith-resources.yaml   # adjust for your lab before applying
go run ./cmd/nodesmith --controller-name kubenodesmith --controller-namespace kubenodesmith
```

The binary will:
- read kubeconfig from `~/.kube/config` (honours `--kubeconfig`),
- pull controller/pool/provider configuration from the NodeSmith CRDs,
- look up the Proxmox secret in your cluster,
- start polling immediately (default 30s cadence).


Pro tip: when running from your workstation the provider still uses the live Proxmox API. Point it at a lab cluster or set up a fake provider entry if you want to practice without creating VMs.

---

## Deploying to Kubernetes

1. Create the `kubenodesmith` namespace (or pick your own) and load the Proxmox secret there.
2. Install the CRDs:
   ```bash
   kubectl apply -f manifests/crds/
   ```
3. Tweak `manifests/app/nodesmith-resources.yaml` for your environment, then apply it:
   ```bash
   kubectl apply -f manifests/app/nodesmith-resources.yaml
   ```
4. Deploy the controller workload and RBAC:
   ```bash
   kubectl apply -f manifests/app/kubenodesmith.yaml
   ```
   (or sync `manifests/app/argocd-application.yaml` if you prefer GitOps.)
5. Health checks live at `http://<pod>:8080/healthz`.

---

## Configuration cheat sheet

The autoscaler now reads Kubernetes-native CRDs under `kubenodesmith.parawell.cloud/v1alpha1`.
`manifests/app/nodesmith-resources.yaml` shows the full picture:

```yaml
---
apiVersion: kubenodesmith.parawell.cloud/v1alpha1
kind: NodeSmithProvider
metadata:
  name: proxmox-production
  namespace: kubenodesmith
spec:
  type: proxmox
  credentialsSecretRef:
    name: proxmox-api-secret
    namespace: kubenodesmith
  proxmox:
    endpoint: https://10.0.4.30:8006/api2/json
    nodeWhitelist: [alfaromeo, porsche]
    vmIDRange: { lower: 1250, upper: 1300 }
    managedNodeTag: kubenodesmith-managed
    vmMemOverheadMiB: 2048
    networkInterfaces:
      - { name: net0, model: virtio, bridge: vmbr0, vlanTag: 20, macPrefix: "02:00:00" }
    vmOptions:
      - { name: sockets, value: 1 }
      - { name: cpu, value: host }
      - { name: boot, value: order=net0 }
      - { name: ostype, value: l26 }
      - { name: ipconfig0, value: ip=dhcp }
      - { name: ide2, value: local-zfs:cloudinit }
      - { name: scsi0, value: local-zfs:16,serial=CONTAINERD01,discard=on,ssd=1 }
      - { name: cicustom, value: meta=snippets:snippets/zagato-shared-cloud-init-meta-data.yaml }
---
apiVersion: kubenodesmith.parawell.cloud/v1alpha1
kind: NodeSmithPool
metadata:
  name: proxmox-small
  namespace: kubenodesmith
spec:
  providerRef: proxmox-production
  limits:
    minNodes: 0
    maxNodes: 5
    cpuCores: 0
    memoryMiB: 30720
  machineTemplate:
    kubeNodeNamePrefix: zagato-worker-auto
    architecture: amd64
    labels:
      node-role.kubernetes.io/worker: ""
  scaleUp:
    batchSize: 1
    stabilizationWindow: 2m
  scaleDown:
    maxConcurrent: 1
    drainTimeout: 10m
---
apiVersion: kubenodesmith.parawell.cloud/v1alpha1
kind: NodeSmithController
metadata:
  name: kubenodesmith
  namespace: kubenodesmith
spec:
  pollingInterval: 30s
  pools: [proxmox-small]
```

Key things to remember:

- All three resources live in the same namespace by default; the controller watches the pool names listed in its spec.
- Durations use the usual Go syntax (`30s`, `2m`, …).
- The controller reads provider credentials from the referenced secret at reconcile time—no more ConfigMap wiring.
- Node pools still define min/max counts and aggregate CPU/memory ceilings; the controller adds the pool label (`topology.kubenodesmith.io/pool: <name>`) automatically.

---

## What happens during a loop?

1. **Poll:** list pending pods with the “Unschedulable” condition.
2. **Scale up:** if there’s at least one, figure out the pod’s resource requests, check the pool limits, and ask the provider for a VM big enough to host it.
3. **Wait:** watch Kubernetes until the node registers (timeout 5 min), then add any configured labels.
4. **Scale down:** when there are no unschedulable pods, look for idle nodes in the pool (no evictable pods). Cordons them, calls the provider to delete the VM, and repeats.

All Kubernetes interactions live in `internal/kube.go`; provider calls go through the `internal/provider` interface so additional backends can plug in later.

---

## Kicking the tires

- Deploy one of the “heavy” sample workloads:
  ```bash
  kubectl apply -f manifests/testcase-deployments/echoserver.yaml
  ```
- Watch the scaler logs for a new node request and make sure the pod lands on it.
- Delete the workload and confirm the node is eventually cordoned and removed.

---

## Contributing & roadmap

I keep the future wish-list in `TODO.md`: better scheduling heuristics, more providers, observability, all the good stuff. If you open a PR, note how you tested it and what assumptions you made about your infrastructure.

---

## License

Released under the [MIT License](LICENSE).
