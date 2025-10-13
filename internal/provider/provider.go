package provider

import "context"

// MachineSpec captures the minimum compute characteristics a backend must honor
// when bringing a new worker online. Providers can interpret additional fields
// via Tags.
type MachineSpec struct {
	// NamePrefix becomes part of the eventual Kubernetes node name so the
	// autoscaler can correlate machines it created.
	NamePrefix string
	// CPUCores represents whole virtual cores requested for the machine.
	CPUCores int64
	// MemoryMiB is the desired memory size in mebibytes available to the node.
	MemoryMiB int64
	// DiskGiB optionally communicates the boot disk size expectation.
	DiskGiB int64
	// Tags allow callers to persist backend specific metadata (e.g. proxmox VM
	// tags) that help identify managed machines.
	Tags map[string]string
}

// Machine represents a node that the backend provider manages on behalf of the
// autoscaler.
type Machine struct {
	// ProviderID is the unique identifier understood by the backend API.
	ProviderID string
	// KubeNodeName is the name the node will register with inside Kubernetes.
	KubeNodeName string
	// Tags echo provider-side labels to support idempotency or filtering.
	Tags map[string]string
}

// Provider defines the lifecycle operations required by the autoscaler to work
// with different infrastructure backends (Proxmox, Redfish, bare metal, etc.).
type Provider interface {
	// ProvisionMachine must create a machine that will eventually join the
	// cluster. It should return immediately once the backend has accepted the
	// request and provide enough metadata for follow-up coordination.
	ProvisionMachine(ctx context.Context, spec MachineSpec) (*Machine, error)

	// DeprovisionMachine tears down a machine previously created by
	// ProvisionMachine. Implementations should be idempotent.
	DeprovisionMachine(ctx context.Context, machine Machine) error

	// ListMachines returns the set of machines currently managed by this
	// provider. The result should only include machines that match the prefixes
	// and tags supplied during provisioning so the autoscaler can detect drift.
	ListMachines(ctx context.Context, namePrefix string) ([]Machine, error)
}
