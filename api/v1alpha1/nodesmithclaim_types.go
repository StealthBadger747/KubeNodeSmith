package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NodeSmithClaimSpec defines the desired state of NodeSmithClaim
type NodeSmithClaimSpec struct {
	// +kubebuilder:validation:MinLength=1
	PoolRef        string                      `json:"poolRef"`
	Requirements   *NodeSmithClaimRequirements `json:"requirements,omitempty"`
	IdempotencyKey string                      `json:"idempotencyKey,omitempty"`
	// TerminationGracePeriod bounds how long NodeSmith waits for pods to gracefully exit
	// before forcing termination on the backing node.
	// +optional
	TerminationGracePeriod *metav1.Duration `json:"terminationGracePeriod,omitempty"`
}

// NodeSmithClaimRequirements specifies the resource requirements for a claim.
// +kubebuilder:validation:XValidation:rule="!has(self.cpuCores) || self.cpuCores > 0",message="cpuCores must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.memoryMiB) || self.memoryMiB > 0",message="memoryMiB must be positive"
type NodeSmithClaimRequirements struct {
	CPUCores  int64 `json:"cpuCores,omitempty"`
	MemoryMiB int64 `json:"memoryMiB,omitempty"`
}

// Condition types for NodeSmithClaim lifecycle tracking.
// Following Kubernetes conventions, conditions provide detailed state information
// without requiring a single "phase" field.
const (
	// ConditionTypeLaunched indicates whether the machine has been created by the provider.
	ConditionTypeLaunched = "Launched"

	// ConditionTypeRegistered indicates whether the node has registered with the cluster.
	ConditionTypeRegistered = "Registered"

	// ConditionTypeInitialized indicates whether the node is fully initialized and ready.
	ConditionTypeInitialized = "Initialized"

	// ConditionTypeReady is the overall readiness condition, typically dependent on
	// Launched, Registered, and Initialized all being True.
	ConditionTypeReady = "Ready"

	// ConditionTypeFailed indicates an unrecoverable error occurred.
	// When this condition is True, the controller will stop reconciling the claim.
	ConditionTypeFailed = "Failed"
)

// Finalizer for NodeSmithClaim to ensure proper cleanup of cloud resources.
const FinalizerNodeSmithClaim = "kubenodesmith.io/nodesmithclaim"

// NodeSmithClaimStatus defines the observed state of NodeSmithClaim.
type NodeSmithClaimStatus struct {
	// conditions represent the current state of the NodeSmithClaim resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types for NodeSmithClaim lifecycle:
	// - "Launched": the machine has been created by the provider
	// - "Registered": the node has registered with the cluster
	// - "Initialized": the node is fully initialized and ready
	// - "Ready": overall readiness (depends on other conditions)
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// providerID is the unique identifier for the machine from the cloud provider.
	// This is set immediately after the machine is created and is critical for
	// idempotency and durability across controller restarts.
	// Format examples: "proxmox://<cluster>/vms/<id>", "redfish://<ip>/Systems/<id>"
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// nodeName is the name of the Kubernetes Node object that corresponds to this claim.
	// This is set once the node successfully registers with the cluster.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// providerRef identifies the NodeSmithProvider used to provision this claim.
	// This field is informational and helps track which provider configuration was used.
	// +optional
	ProviderRef string `json:"providerRef,omitempty"`

	// launchAttempts tracks how many times we've attempted to launch and register
	// a machine for this claim. After exceeding the max retry count, the claim
	// is marked as Failed and reconciliation stops.
	// +optional
	LaunchAttempts int `json:"launchAttempts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NodeSmithClaim is the Schema for the nodesmithclaims API
type NodeSmithClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of NodeSmithClaim
	// +required
	Spec NodeSmithClaimSpec `json:"spec"`

	// status defines the observed state of NodeSmithClaim
	// +optional
	Status NodeSmithClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeSmithClaimList contains a list of NodeSmithClaim
type NodeSmithClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeSmithClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeSmithClaim{}, &NodeSmithClaimList{})
}
