package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NodeSmithPoolSpec defines the desired state of NodeSmithPool
type NodeSmithPoolSpec struct {
	ProviderRef string `json:"providerRef"`
	// +kubebuilder:default=topology.kubenodesmith.io/pool
	PoolLabelKey    string           `json:"poolLabelKey,omitempty"`
	Limits          NodePoolLimits   `json:"limits"`
	MachineTemplate MachineTemplate  `json:"machineTemplate"`
	ScaleUp         *ScaleUpPolicy   `json:"scaleUp,omitempty"`
	ScaleDown       *ScaleDownPolicy `json:"scaleDown,omitempty"`
	// +optional
	RegistrationTimeout *metav1.Duration `json:"registrationTimeout,omitempty"`
}

// NodePoolLimits constrains pool size and aggregate resources.
// +kubebuilder:validation:XValidation:rule="self.minNodes >= 0",message="minNodes must be non-negative"
// +kubebuilder:validation:XValidation:rule="self.maxNodes == 0 || self.maxNodes >= self.minNodes",message="maxNodes must be zero (unlimited) or >= minNodes"
// +kubebuilder:validation:XValidation:rule="self.cpuCores == 0 || self.cpuCores > 0",message="cpuCores must be zero (unlimited) or positive"
// +kubebuilder:validation:XValidation:rule="self.memoryMiB == 0 || self.memoryMiB > 0",message="memoryMiB must be zero (unlimited) or positive"
type NodePoolLimits struct {
	MinNodes  int   `json:"minNodes"`
	MaxNodes  int   `json:"maxNodes"`
	CPUCores  int64 `json:"cpuCores"`
	MemoryMiB int64 `json:"memoryMiB"`
}

// MachineTemplate captures metadata for nodes provisioned into the pool.
type MachineTemplate struct {
	Labels map[string]string `json:"labels,omitempty"`
}

// ScaleUpPolicy controls expansion behaviour.
type ScaleUpPolicy struct {
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`
}

// ScaleDownPolicy controls contraction behaviour.
type ScaleDownPolicy struct {
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`
}

// NodeSmithPoolStatus defines the observed state of NodeSmithPool.
type NodeSmithPoolStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the NodeSmithPool resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastScaleActivity  *metav1.Time       `json:"lastScaleActivity,omitempty"`
	NextClaimSequence  int64              `json:"nextClaimSequence,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NodeSmithPool is the Schema for the nodesmithpools API
type NodeSmithPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NodeSmithPool
	// +required
	Spec NodeSmithPoolSpec `json:"spec"`

	// status defines the observed state of NodeSmithPool
	// +optional
	Status NodeSmithPoolStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeSmithPoolList contains a list of NodeSmithPool
type NodeSmithPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeSmithPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeSmithPool{}, &NodeSmithPoolList{})
}
