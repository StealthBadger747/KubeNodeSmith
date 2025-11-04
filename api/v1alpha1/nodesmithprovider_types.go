package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NodeSmithProviderSpec defines the desired state of NodeSmithProvider
// +kubebuilder:validation:XValidation:rule="self.type in ['proxmox', 'redfish']",message="type must be one of: proxmox, redfish"
// +kubebuilder:validation:XValidation:rule="self.type == 'proxmox' ? has(self.proxmox) : true",message="proxmox field required when type is 'proxmox'"
type NodeSmithProviderSpec struct {
	Type                 string                          `json:"type"`
	CredentialsSecretRef *corev1.SecretReference         `json:"credentialsSecretRef,omitempty"`
	Proxmox              *ProxmoxProviderSpec            `json:"proxmox,omitempty"`
	AdditionalSettings   map[string]runtime.RawExtension `json:"additionalSettings,omitempty"`
}

// ProxmoxProviderSpec carries provider-specific knobs for Proxmox clusters.
// +kubebuilder:validation:XValidation:rule="self.endpoint.startsWith('https://') || self.endpoint.startsWith('http://')",message="endpoint must be a valid HTTP/HTTPS URL"
// +kubebuilder:validation:XValidation:rule="!has(self.vmMemOverheadMiB) || self.vmMemOverheadMiB >= 0",message="vmMemOverheadMiB must be non-negative"
type ProxmoxProviderSpec struct {
	Endpoint          string          `json:"endpoint"`
	NodeWhitelist     []string        `json:"nodeWhitelist,omitempty"`
	VMIDRange         *VMIDRange      `json:"vmIDRange,omitempty"`
	ManagedNodeTag    string          `json:"managedNodeTag,omitempty"`
	VMMemOverheadMiB  int64           `json:"vmMemOverheadMiB,omitempty"`
	NetworkInterfaces []ProxmoxNIC    `json:"networkInterfaces,omitempty"`
	VMOptions         []ProxmoxOption `json:"vmOptions,omitempty"`
}

// VMIDRange constrains the VM identifiers a provider may use.
// +kubebuilder:validation:XValidation:rule="self.lower >= 100",message="lower must be >= 100 (Proxmox reserved range)"
// +kubebuilder:validation:XValidation:rule="self.upper <= 999999999",message="upper must be <= 999999999 (Proxmox max VMID)"
// +kubebuilder:validation:XValidation:rule="self.upper >= self.lower",message="upper must be >= lower"
type VMIDRange struct {
	Lower int64 `json:"lower"`
	Upper int64 `json:"upper"`
}

// ProxmoxNIC describes a Proxmox network device definition.
type ProxmoxNIC struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Bridge    string `json:"bridge"`
	VLANTag   int    `json:"vlanTag,omitempty"`
	MACPrefix string `json:"macPrefix,omitempty"`
}

// ProxmoxOption is a name/value pair passed to the Proxmox API.
type ProxmoxOption struct {
	Name  string               `json:"name"`
	Value apiextensionsv1.JSON `json:"value"`
}

// NodeSmithProviderStatus defines the observed state of NodeSmithProvider.
type NodeSmithProviderStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the NodeSmithProvider resource.
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NodeSmithProvider is the Schema for the nodesmithproviders API
type NodeSmithProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NodeSmithProvider
	// +required
	Spec NodeSmithProviderSpec `json:"spec"`

	// status defines the observed state of NodeSmithProvider
	// +optional
	Status NodeSmithProviderStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeSmithProviderList contains a list of NodeSmithProvider
type NodeSmithProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeSmithProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeSmithProvider{}, &NodeSmithProviderList{})
}
