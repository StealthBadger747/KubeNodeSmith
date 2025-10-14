# KubeNodeSmith

KubeNodeSmith is my home-lab friendly autoscaler. It watches Kubernetes for pods that can’t get a seat, asks Proxmox for a new worker, waits for the node to come online, labels it, and later tears it back down when it’s just burning watts. No cloud provider API, no managed magic—just the Kubernetes API, your infrastructure, and some glue.

> 🛠️ **Still a prototype.** Expect sharp edges, `panic` calls in a few spots, and plenty of TODOs. Check `TODO.md` for the roadmap.

---

## What’s in the repo?

- `cmd/nodesmith` – the main event. Polls the cluster, talks to providers, labels nodes.
- `cmd/boot-ipxe` – an older helper for one-off VM bootstrapping. Handy during experiments.
- `internal/config` – YAML loader with validation and duration helpers.
- `internal/kube.go` – all the Kubernetes querying, labelling, and resource math.
- `internal/provider` – provider interface + the current Proxmox implementation.
- `example-configs` – reference configs and notes to copy from.
- `manifests` – deployment YAML (plain or Argo CD) plus a couple “stress the scheduler” workloads.
- `nodesmith/` – a pre-built binary drop if you just want to poke around quickly.

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

Using Nix? Drop into the dev shell and run it inline:

```bash
nix develop
go run ./cmd/nodesmith --config ./example-configs/scaler-config.yaml
```

The binary will:
- read kubeconfig from `~/.kube/config` (honours `--kubeconfig`),
- look up the Proxmox secret in your cluster,
- start polling immediately (default 30s cadence).


Pro tip: when running from your workstation the provider still uses the live Proxmox API. Point it at a lab cluster or set up a fake provider entry if you want to practice without creating VMs.

---

## Deploying to Kubernetes

1. Create the `kubenodesmith` namespace and load your Proxmox secret.
2. Apply the manifests from `manifests/app/`:
   ```bash
   kubectl apply -f manifests/app/
   ```
3. The deployment mounts the ConfigMap at `/config` and sets `SCALER_CONFIG_PATH=/config/scaler-config.yaml` for you.
4. Health checks live at `http://<pod>:8080/healthz`.
5. If you prefer GitOps, sync `manifests/app/argocd-application.yaml` into Argo CD.

---

## Configuration cheat sheet

Everything lives in one YAML file (see `example-configs/scaler-config.yaml`).

```yaml
schemaVersion: v1alpha1
pollingInterval: 30s

providers:
  proxmox-production:
    type: proxmox
    credentialsRef:
      name: proxmox-api-secret
      namespace: kubenodesmith
    options:
      endpoint: https://10.0.4.30:8006/api2/json
      nodeWhitelist: [alfaromeo, porsche]
      vmIDRange:
        lower: 1250
        upper: 1300
      managedNodeTag: kubenodesmith-managed
      vmMemOverheadMiB: 2048
      networkInterfaces:
        - name: net0
          model: virtio
          bridge: vmbr0
          vlanTag: 20
          macPrefix: "02:00:00"
      vmOptions:
        - { name: sockets, value: 1 }
        - { name: cpu, value: host }

nodePools:
  - name: proxmox-small
    providerRef: proxmox-production
    limits:
      minNodes: 0
      maxNodes: 5
      cpuCores: 0        # 0 == “no cap”
      memoryMiB: 30720   # pool-wide ceiling
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
```

Key things to remember:

- `schemaVersion` is locked to `v1alpha1` for now.
- `pollingInterval` is parsed with Go’s duration syntax (`30s`, `2m`, …).
- Providers are decoded into strongly typed structs—unknown options are ignored but preserved.
- Node pools define min/max counts plus aggregate CPU/memory ceilings to avoid runaways.
- Labels in `machineTemplate` are stamped onto the node once it joins; the scaler automatically adds the pool label (`topology.kubenodesmith.io/pool: <nodePoolName>`), so you don’t have to include it.

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

## Development tips

- `go fmt ./...` and `go test ./...` before committing.
- Avoid adding new `panic` paths—bubble errors up instead (see TODO list).
- When adding config knobs, update `internal/config` and drop an example into `example-configs/`.
- The Proxmox provider supports `PROXMOX_SKIP_TLS_VERIFY=true` if your lab cluster has self-signed certs.

---

## Contributing & roadmap

I keep the future wish-list in `TODO.md`: CRDs, better scheduling heuristics, more providers, observability, all the good stuff. If you open a PR, note how you tested it and what assumptions you made about your infrastructure.

---

## License

No explicit license yet, so treat it as “all rights reserved” until one is published.
