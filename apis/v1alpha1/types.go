package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion identifies the API group/versions packaged here.
var GroupVersion = schema.GroupVersion{
	Group:   "kubenodesmith.parawell.cloud",
	Version: "v1alpha1",
}

// Convenience helpers for dynamic clients.
func NodeSmithControllerGVR() schema.GroupVersionResource {
	return GroupVersion.WithResource("nodesmithcontrollers")
}

func NodeSmithProviderGVR() schema.GroupVersionResource {
	return GroupVersion.WithResource("nodesmithproviders")
}

func NodeSmithPoolGVR() schema.GroupVersionResource {
	return GroupVersion.WithResource("nodesmithpools")
}

// NodeSmithController configures the behaviour of the autoscaler instance.
type NodeSmithController struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSmithControllerSpec   `json:"spec,omitempty"`
	Status NodeSmithControllerStatus `json:"status,omitempty"`
}

// NodeSmithControllerList is the list variant for NodeSmithController.
type NodeSmithControllerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeSmithController `json:"items"`
}

// NodeSmithControllerSpec captures top-level controller settings.
type NodeSmithControllerSpec struct {
	// PollingInterval controls how often the controller scans for scale decisions.
	PollingInterval *metav1.Duration `json:"pollingInterval,omitempty"`
	// Pools enumerates the NodeSmithPool objects this controller should manage.
	Pools []string `json:"pools,omitempty"`
	// DefaultProvider is used when a pool omits ProviderRef (future use).
	DefaultProvider string `json:"defaultProvider,omitempty"`
}

// NodeSmithControllerStatus surfaces reconciliation health to operators.
type NodeSmithControllerStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// NodeSmithProvider describes a backing infrastructure provider (e.g., Proxmox).
type NodeSmithProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSmithProviderSpec   `json:"spec,omitempty"`
	Status NodeSmithProviderStatus `json:"status,omitempty"`
}

// NodeSmithProviderList is the list variant for NodeSmithProvider.
type NodeSmithProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeSmithProvider `json:"items"`
}

// NodeSmithProviderSpec captures implementation-specific configuration.
type NodeSmithProviderSpec struct {
	Type                 string                          `json:"type"`
	CredentialsSecretRef *corev1.SecretReference         `json:"credentialsSecretRef,omitempty"`
	Proxmox              *ProxmoxProviderSpec            `json:"proxmox,omitempty"`
	AdditionalSettings   map[string]runtime.RawExtension `json:"additionalSettings,omitempty"`
}

// ProxmoxProviderSpec carries provider-specific knobs for Proxmox clusters.
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
	Name  string      `json:"name"`
	Value interface{} `json:"value"`
}

// NodeSmithProviderStatus surfaces reconciliation state to operators.
type NodeSmithProviderStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// NodeSmithPool defines an autoscaled pool of machines.
type NodeSmithPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSmithPoolSpec   `json:"spec,omitempty"`
	Status NodeSmithPoolStatus `json:"status,omitempty"`
}

// NodeSmithPoolList is the list variant for NodeSmithPool.
type NodeSmithPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeSmithPool `json:"items"`
}

// NodeSmithPoolSpec captures scaling parameters for a pool.
type NodeSmithPoolSpec struct {
	ProviderRef     string           `json:"providerRef"`
	PoolLabelKey    string           `json:"poolLabelKey,omitempty"`
	Limits          NodePoolLimits   `json:"limits"`
	MachineTemplate MachineTemplate  `json:"machineTemplate"`
	ScaleUp         *ScaleUpPolicy   `json:"scaleUp,omitempty"`
	ScaleDown       *ScaleDownPolicy `json:"scaleDown,omitempty"`
}

// NodePoolLimits constrains pool size and aggregate resources.
type NodePoolLimits struct {
	MinNodes  int   `json:"minNodes"`
	MaxNodes  int   `json:"maxNodes"`
	CPUCores  int64 `json:"cpuCores"`
	MemoryMiB int64 `json:"memoryMiB"`
}

// MachineTemplate captures metadata for nodes provisioned into the pool.
type MachineTemplate struct {
	KubeNodeNamePrefix string            `json:"kubeNodeNamePrefix"`
	Architecture       string            `json:"architecture,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
}

// ScaleUpPolicy controls expansion behaviour.
type ScaleUpPolicy struct {
	BatchSize           int              `json:"batchSize,omitempty"`
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`
}

// ScaleDownPolicy controls contraction behaviour.
type ScaleDownPolicy struct {
	MaxConcurrent int              `json:"maxConcurrent,omitempty"`
	DrainTimeout  *metav1.Duration `json:"drainTimeout,omitempty"`
}

// NodeSmithPoolStatus surfaces recent scaling actions.
type NodeSmithPoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastScaleActivity  *metav1.Time       `json:"lastScaleActivity,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
