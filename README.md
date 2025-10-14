# KubeNodeSmith

KubeNodeSmith is an experimental node autoscaler tailored for bare‑metal and virtualized home lab clusters. It watches for unschedulable pods, provisions fresh worker nodes through infrastructure providers (currently Proxmox), waits for them to register with Kubernetes, and labels the nodes so workloads can land safely. When capacity sits idle, it cordons and tears nodes back down.

- Pulls scheduling signals directly from the Kubernetes API (no cloud provider integration required).
- Talks to infrastructure through a small provider interface so other backends (Redfish, vSphere, etc.) can be added over time.
- Applies guardrails per node pool (min/max nodes, aggregate CPU and memory ceilings).
- Exposes an `/healthz` endpoint for simple liveness probes.

> **Project status:** early prototype. The codebase still carries sharp edges (`panic` paths, limited error handling, single-threaded loop). See `TODO.md` for the active roadmap.

## Repository Tour

- `cmd/nodesmith` – main autoscaler binary. Evaluates node pools, provisions or deprovisions nodes, and labels new workers.
- `cmd/boot-ipxe` – legacy/experimental helper for direct VM provisioning. Useful for local hacking, not part of the primary loop.
- `internal/` – reusable packages:
  - `internal/config` decodes the YAML config file (with schema enforcement and duration helpers).
  - `internal/kube.go` houses Kubernetes interactions (pod/node queries, resource accounting, labeling).
  - `internal/provider` defines the provider interface and the Proxmox implementation.
- `example-configs/` – copy-ready sample scaler configuration and documentation.
- `manifests/` – Kubernetes deployment manifests (standalone YAML and Argo CD application) plus workload fixtures for scale testing.
- `nodesmith/` – pre-built binary snapshot kept around for quick smoke tests.

## Prerequisites

- Go `1.24.6` (see `go.mod`).
- Access to a Kubernetes cluster (kubeconfig on disk or in-cluster credentials when running as a pod).
- Provider credentials:
  - **Proxmox:** API token with rights to create/delete VMs and inspect cluster/node resources.
  - Secrets must expose `PROXMOX_TOKEN_ID`, `PROXMOX_SECRET`, and (optionally) `PROXMOX_SKIP_TLS_VERIFY`.
- For cluster deploys, create the `kubenodesmith` namespace and prepare the provider secrets ahead of time.

## Build & Run Locally

```bash
go build ./cmd/nodesmith
./nodesmith --config ./example-configs/scaler-config.yaml
```

The binary reads kubeconfig from `~/.kube/config` by default. Override with `--kubeconfig` if needed.

### Using Nix

The repo includes a `flake.nix` for reproducible builds:

```bash
nix develop
go run ./cmd/nodesmith --config ./example-configs/scaler-config.yaml
```

## Kubernetes Deployment

1. Apply RBAC, service account, deployment, and ConfigMap from `manifests/app/kubenodesmith.yaml` and `manifests/app/scaler-config-configmap.yaml`.
2. Mount the scaler configuration into the pod and pass its path via `SCALER_CONFIG_PATH` (the deployment already handles this).
3. Point your GitOps stack at `manifests/app` or import `manifests/app/argocd-application.yaml`.

Probe the pod at `http://<pod-ip>:8080/healthz` to track liveness/readiness.

## Configuration Guide

Configuration is a single YAML document. The loader enforces known fields and validates references. Start from `example-configs/scaler-config.yaml`.

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
      endpoint: https://<cluster>:8006/api2/json
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
        - name: sockets
          value: 1
        - name: cpu
          value: host
        # ...

nodePools:
  - name: proxmox-small
    providerRef: proxmox-production
    limits:
      minNodes: 0
      maxNodes: 5
      cpuCores: 0        # 0 means “unbounded”
      memoryMiB: 30720   # aggregate memory ceiling for the pool
    machineTemplate:
      kubeNodeNamePrefix: zagato-worker-auto
      architecture: amd64
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

Key ideas:

- `schemaVersion` must be `v1alpha1`.
- `pollingInterval` defines how often the scaler evaluates workloads.
- `providers` declare backend connectivity. The Proxmox provider expects `options` describing API endpoints, allowable hypervisor nodes, VMID ranges, and bootstrap VM options (network, disks, cloud-init settings, etc.).
- `nodePools` map pending pods to providers. Pool limits guard against runaway scale-outs by imposing node count and aggregate resource ceilings.
- `MachineTemplate.labels` are applied to the Kubernetes node after it becomes ready. The pool label defaults to `topology.kubenodesmith.io/pool`.

Any provider block with `type: proxmox` is unpacked into strongly typed options by `internal/provider/proxmox`. Additional keys in `options` are ignored unless decoded, which lets you experiment without code changes.

## Runtime Behaviour

1. **Loop cadence:** every `pollingInterval` seconds, the scaler lists unschedulable pods across the cluster.
2. **Scale up:** if pods are pending due to resource shortages, it calculates their requests, checks pool limits, and asks the provider to create a machine sized for that demand.
3. **Post-provision:** it waits (up to five minutes) for the node to register, then stamps any labels configured for the node pool.
4. **Scale down:** when no pods are pending, it looks for pool nodes that are idle (no evictable pods), cordons them, and calls the provider to delete the backing machine.

All Kubernetes interactions live in `internal/kube.go` and rely on client-go. Provider calls flow through `internal/provider.Provider`, making the Proxmox integration replaceable.

## Testing Scale Decisions

Use the sample workloads in `manifests/testcase-deployments/` to force scheduling pressure. For example:

```bash
kubectl apply -f manifests/testcase-deployments/echoserver.yaml
```

Watch the scaler logs to confirm a new node is requested, then verify the pod lands on the freshly provisioned machine.

## Development Notes

- Format and lint Go code with the usual toolchain (`go fmt`, `go vet`, `staticcheck` if available).
- Run `go test ./...` before sending changes (no tests exist yet, but the command completes quickly and will catch compilation issues).
- Keep provider logic free of `panic` paths where possible—error handling improvements are tracked in `TODO.md`.
- When introducing new configuration keys, update `internal/config` so the loader validates and documents them, and add an example to `example-configs/`.

## Roadmap & Contributions

The rough roadmap lives in `TODO.md` and covers work such as CRD-based configuration, richer scheduling heuristics, additional provider support, and better observability. Issues and pull requests are welcome; please call out any assumptions about your cluster or provider when contributing.

## License

No explicit license has been published yet. Until one is added, treat the project as “all rights reserved” and obtain permission before using it in production.
