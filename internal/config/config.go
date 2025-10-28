package config

import (
	"context"
	"fmt"
	"time"

	nodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/apis/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// Duration wraps time.Duration to preserve the previous API surface.
type Duration struct {
	time.Duration
}

// AsDuration exposes the inner time.Duration value.
func (d Duration) AsDuration() time.Duration {
	return d.Duration
}

// Config represents the runtime configuration for the autoscaler.
type Config struct {
	PollingInterval Duration
	Providers       map[string]ProviderConfig
	NodePools       []NodePool
}

// ProviderConfig captures backend-specific configuration.
type ProviderConfig struct {
	Type           string
	CredentialsRef *CredentialsRef
	Proxmox        *ProxmoxProviderOptions
}

// CredentialsRef points the autoscaler to the secret holding provider credentials.
type CredentialsRef struct {
	Name      string
	Namespace string
}

// NodePool describes an autoscaled group of machines.
type NodePool struct {
	Name            string
	ProviderRef     string
	PoolLabelKey    string
	Limits          NodePoolLimits
	MachineTemplate MachineTemplate
	ScaleUp         *ScaleUpPolicy
	ScaleDown       *ScaleDownPolicy
}

// NodePoolLimits constrains min/max nodes per pool and resource limits.
type NodePoolLimits struct {
	MinNodes  int
	MaxNodes  int
	CPUCores  int64
	MemoryMiB int64
}

// MachineTemplate captures the requested machine shape per node pool.
type MachineTemplate struct {
	KubeNodeNamePrefix string
	Architecture       string
	Labels             map[string]string
}

// ScaleUpPolicy defines pacing controls for scale-out operations.
type ScaleUpPolicy struct {
	BatchSize           int
	StabilizationWindow *Duration
}

// ScaleDownPolicy defines pacing controls for scale-in operations.
type ScaleDownPolicy struct {
	MaxConcurrent int
	DrainTimeout  *Duration
}

// GetPoolLabelKey returns the pool label key for this node pool, with a default value if not specified.
func (np *NodePool) GetPoolLabelKey() string {
	if np.PoolLabelKey != "" {
		return np.PoolLabelKey
	}
	return "topology.kubenodesmith.io/pool"
}

// ProxmoxProviderOptions contains strongly typed Proxmox-specific settings.
type ProxmoxProviderOptions struct {
	Endpoint          string
	NodeWhitelist     []string
	VMIDRange         *VMIDRange
	ManagedNodeTag    string
	VMMemOverheadMiB  int64
	NetworkInterfaces []ProxmoxNetworkInterface
	VMOptions         []ProxmoxOption
}

// VMIDRange constrains the VM identifiers a provider may use.
type VMIDRange struct {
	Lower int64
	Upper int64
}

// ProxmoxNetworkInterface describes a Proxmox network interface definition.
type ProxmoxNetworkInterface struct {
	Name      string
	Model     string
	Bridge    string
	VLANTag   int
	MACPrefix string
}

// ProxmoxOption is a name/value pair passed through to the Proxmox API.
type ProxmoxOption struct {
	Name  string
	Value interface{}
}

// Validate performs light integrity checks on the configuration.
func (c *Config) Validate() error {
	if c.Providers == nil {
		c.Providers = map[string]ProviderConfig{}
	}

	for idx, pool := range c.NodePools {
		if pool.Name == "" {
			return fmt.Errorf("nodePools[%d] has empty name", idx)
		}
		if pool.ProviderRef == "" {
			return fmt.Errorf("nodePool %q is missing providerRef", pool.Name)
		}
		if _, ok := c.Providers[pool.ProviderRef]; !ok {
			return fmt.Errorf("nodePool %q references unknown provider %q", pool.Name, pool.ProviderRef)
		}
		if pool.Limits.MaxNodes < pool.Limits.MinNodes {
			return fmt.Errorf("nodePool %q has maxNodes < minNodes", pool.Name)
		}
	}
	return nil
}

// LoadFromCRDs assembles the autoscaler configuration from NodeSmith CRDs.
func LoadFromCRDs(ctx context.Context, client dynamic.Interface, namespace, controllerName string) (*Config, error) {
	ctrlObj, err := client.
		Resource(nodesmithv1alpha1.NodeSmithControllerGVR()).
		Namespace(namespace).
		Get(ctx, controllerName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get NodeSmithController %q: %w", controllerName, err)
	}

	var controller nodesmithv1alpha1.NodeSmithController
	if err := convertUnstructured(ctrlObj, &controller); err != nil {
		return nil, fmt.Errorf("decode NodeSmithController %q: %w", controllerName, err)
	}

	cfg := &Config{
		Providers: make(map[string]ProviderConfig),
		NodePools: make([]NodePool, 0, len(controller.Spec.Pools)),
	}

	if controller.Spec.PollingInterval != nil {
		cfg.PollingInterval = Duration{Duration: controller.Spec.PollingInterval.Duration}
	}

	if cfg.PollingInterval.Duration <= 0 {
		cfg.PollingInterval = Duration{Duration: 30 * time.Second}
	}

	providerNames := make(map[string]struct{})
	for _, poolName := range controller.Spec.Pools {
		if poolName == "" {
			return nil, fmt.Errorf("controller %q has empty pool name entry", controllerName)
		}

		poolObj, err := client.
			Resource(nodesmithv1alpha1.NodeSmithPoolGVR()).
			Namespace(namespace).
			Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get NodeSmithPool %q: %w", poolName, err)
		}

		var pool nodesmithv1alpha1.NodeSmithPool
		if err := convertUnstructured(poolObj, &pool); err != nil {
			return nil, fmt.Errorf("decode NodeSmithPool %q: %w", poolName, err)
		}

		providerRef := pool.Spec.ProviderRef
		if providerRef == "" {
			providerRef = controller.Spec.DefaultProvider
		}
		if providerRef == "" {
			return nil, fmt.Errorf("pool %q is missing providerRef and controller defaultProvider is unset", poolName)
		}

		providerNames[providerRef] = struct{}{}

		cfg.NodePools = append(cfg.NodePools, convertPool(poolName, &pool.Spec, providerRef))
	}

	for name := range providerNames {
		providerObj, err := client.
			Resource(nodesmithv1alpha1.NodeSmithProviderGVR()).
			Namespace(namespace).
			Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get NodeSmithProvider %q: %w", name, err)
		}

		var provider nodesmithv1alpha1.NodeSmithProvider
		if err := convertUnstructured(providerObj, &provider); err != nil {
			return nil, fmt.Errorf("decode NodeSmithProvider %q: %w", name, err)
		}

		cfgProvider, err := convertProvider(&provider.Spec)
		if err != nil {
			return nil, fmt.Errorf("convert NodeSmithProvider %q: %w", name, err)
		}

		cfg.Providers[name] = cfgProvider
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func convertUnstructured[T any](obj *unstructured.Unstructured, target *T) error {
	if obj == nil {
		return fmt.Errorf("nil object")
	}
	content := obj.UnstructuredContent()
	return runtime.DefaultUnstructuredConverter.FromUnstructured(content, target)
}

func convertPool(name string, spec *nodesmithv1alpha1.NodeSmithPoolSpec, providerRef string) NodePool {
	conv := NodePool{
		Name:         name,
		ProviderRef:  providerRef,
		PoolLabelKey: spec.PoolLabelKey,
		Limits: NodePoolLimits{
			MinNodes:  spec.Limits.MinNodes,
			MaxNodes:  spec.Limits.MaxNodes,
			CPUCores:  spec.Limits.CPUCores,
			MemoryMiB: spec.Limits.MemoryMiB,
		},
		MachineTemplate: MachineTemplate{
			KubeNodeNamePrefix: spec.MachineTemplate.KubeNodeNamePrefix,
			Architecture:       spec.MachineTemplate.Architecture,
			Labels:             copyStringMap(spec.MachineTemplate.Labels),
		},
	}

	if spec.ScaleUp != nil {
		conv.ScaleUp = &ScaleUpPolicy{
			BatchSize: spec.ScaleUp.BatchSize,
		}
		if spec.ScaleUp.StabilizationWindow != nil {
			conv.ScaleUp.StabilizationWindow = &Duration{Duration: spec.ScaleUp.StabilizationWindow.Duration}
		}
	}

	if spec.ScaleDown != nil {
		conv.ScaleDown = &ScaleDownPolicy{
			MaxConcurrent: spec.ScaleDown.MaxConcurrent,
		}
		if spec.ScaleDown.DrainTimeout != nil {
			conv.ScaleDown.DrainTimeout = &Duration{Duration: spec.ScaleDown.DrainTimeout.Duration}
		}
	}

	return conv
}

func convertProvider(spec *nodesmithv1alpha1.NodeSmithProviderSpec) (ProviderConfig, error) {
	if spec.Type == "" {
		return ProviderConfig{}, fmt.Errorf("provider type must be specified")
	}

	cfg := ProviderConfig{
		Type: spec.Type,
	}

	if spec.CredentialsSecretRef != nil {
		cfg.CredentialsRef = &CredentialsRef{
			Name:      spec.CredentialsSecretRef.Name,
			Namespace: spec.CredentialsSecretRef.Namespace,
		}
	}

	switch spec.Type {
	case "proxmox":
		if spec.Proxmox == nil {
			return ProviderConfig{}, fmt.Errorf("proxmox provider requires spec.proxmox")
		}
		prox, err := convertProxmox(spec.Proxmox)
		if err != nil {
			return ProviderConfig{}, err
		}
		cfg.Proxmox = prox
	default:
		return ProviderConfig{}, fmt.Errorf("unsupported provider type %q", spec.Type)
	}

	return cfg, nil
}

func convertProxmox(spec *nodesmithv1alpha1.ProxmoxProviderSpec) (*ProxmoxProviderOptions, error) {
	if spec.Endpoint == "" {
		return nil, fmt.Errorf("proxmox endpoint is required")
	}

	opts := &ProxmoxProviderOptions{
		Endpoint:          spec.Endpoint,
		NodeWhitelist:     append([]string(nil), spec.NodeWhitelist...),
		ManagedNodeTag:    spec.ManagedNodeTag,
		VMMemOverheadMiB:  spec.VMMemOverheadMiB,
		VMOptions:         make([]ProxmoxOption, 0, len(spec.VMOptions)),
		NetworkInterfaces: make([]ProxmoxNetworkInterface, 0, len(spec.NetworkInterfaces)),
	}

	if spec.VMIDRange != nil {
		opts.VMIDRange = &VMIDRange{
			Lower: spec.VMIDRange.Lower,
			Upper: spec.VMIDRange.Upper,
		}
		if opts.VMIDRange.Upper < opts.VMIDRange.Lower {
			return nil, fmt.Errorf("proxmox vmIDRange.upper must be >= lower")
		}
	}

	for _, nic := range spec.NetworkInterfaces {
		opts.NetworkInterfaces = append(opts.NetworkInterfaces, ProxmoxNetworkInterface{
			Name:      nic.Name,
			Model:     nic.Model,
			Bridge:    nic.Bridge,
			VLANTag:   nic.VLANTag,
			MACPrefix: nic.MACPrefix,
		})
	}

	for _, opt := range spec.VMOptions {
		opts.VMOptions = append(opts.VMOptions, ProxmoxOption{
			Name:  opt.Name,
			Value: opt.Value,
		})
	}

	return opts, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
