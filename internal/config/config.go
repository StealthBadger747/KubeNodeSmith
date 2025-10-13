package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration to support YAML unmarshalling from strings like "30s".
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses duration strings (or numbers representing nanoseconds).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		d.Duration = 0
		return nil
	}

	switch value.Kind {
	case yaml.ScalarNode:
		var asString string
		if err := value.Decode(&asString); err == nil {
			if asString == "" {
				d.Duration = 0
				return nil
			}
			parsed, err := time.ParseDuration(asString)
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", asString, err)
			}
			d.Duration = parsed
			return nil
		}

		var asInt int64
		if err := value.Decode(&asInt); err == nil {
			d.Duration = time.Duration(asInt)
			return nil
		}
	}

	return fmt.Errorf("invalid duration value: %s", value.Value)
}

// AsDuration exposes the inner time.Duration value.
func (d Duration) AsDuration() time.Duration {
	return d.Duration
}

// Config represents the root configuration document.
type Config struct {
	SchemaVersion   string                    `yaml:"schemaVersion"`
	PollingInterval Duration                  `yaml:"pollingInterval"`
	Providers       map[string]ProviderConfig `yaml:"providers"`
	NodePools       []NodePool                `yaml:"nodePools"`
}

// ProviderConfig captures backend-specific configuration.
type ProviderConfig struct {
	Type           string                  `yaml:"type"`
	CredentialsRef *CredentialsRef         `yaml:"credentialsRef,omitempty"`
	Options        map[string]any          `yaml:"options,omitempty"`
	Proxmox        *ProxmoxProviderOptions `yaml:"-"`
}

// CredentialsRef points the autoscaler to the secret holding provider credentials.
type CredentialsRef struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// NodePool describes an autoscaled group of machines.
type NodePool struct {
	Name            string           `yaml:"name"`
	ProviderRef     string           `yaml:"providerRef"`
	Limits          NodePoolLimits   `yaml:"limits"`
	MachineTemplate MachineTemplate  `yaml:"machineTemplate"`
	ScaleUp         *ScaleUpPolicy   `yaml:"scaleUp,omitempty"`
	ScaleDown       *ScaleDownPolicy `yaml:"scaleDown,omitempty"`
}

// NodePoolLimits constrains min/max nodes per pool.
type NodePoolLimits struct {
	MinNodes int `yaml:"minNodes"`
	MaxNodes int `yaml:"maxNodes"`
}

// MachineTemplate captures the requested machine shape per node pool.
type MachineTemplate struct {
	KubeNodeNamePrefix string            `yaml:"kubeNodeNamePrefix"`
	CPUCores           int64             `yaml:"cpuCores"`
	MemoryMiB          int64             `yaml:"memoryMiB"`
	DiskGiB            int64             `yaml:"diskGiB"`
	Architecture       string            `yaml:"architecture"`
	Labels             map[string]string `yaml:"labels,omitempty"`
}

// ScaleUpPolicy defines pacing controls for scale-out operations.
type ScaleUpPolicy struct {
	BatchSize           int       `yaml:"batchSize,omitempty"`
	StabilizationWindow *Duration `yaml:"stabilizationWindow,omitempty"`
}

// ScaleDownPolicy defines pacing controls for scale-in operations.
type ScaleDownPolicy struct {
	MaxConcurrent int       `yaml:"maxConcurrent,omitempty"`
	DrainTimeout  *Duration `yaml:"drainTimeout,omitempty"`
}

// Load reads and parses a configuration file from disk.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	return decode(f)
}

// decode unmarshals YAML into a Config while enforcing known fields.
func decode(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if err == io.EOF {
			return &cfg, nil
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if err := cfg.postProcess(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate performs light integrity checks on the configuration.
func (c *Config) Validate() error {
	if c.SchemaVersion == "" {
		return fmt.Errorf("schemaVersion is required")
	}
	if c.SchemaVersion != "v1alpha1" {
		return fmt.Errorf("unsupported schemaVersion %q", c.SchemaVersion)
	}
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

func (c *Config) postProcess() error {
	for name, prov := range c.Providers {
		updated, err := prov.decodeOptions()
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		c.Providers[name] = updated
	}
	return nil
}

func (p ProviderConfig) decodeOptions() (ProviderConfig, error) {
	if p.Type != "proxmox" {
		return p, nil
	}
	if p.Options == nil {
		return p, nil
	}
	encoded, err := yaml.Marshal(p.Options)
	if err != nil {
		return p, fmt.Errorf("marshal proxmox options: %w", err)
	}
	var opts ProxmoxProviderOptions
	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	if err := dec.Decode(&opts); err != nil {
		return p, fmt.Errorf("decode proxmox options: %w", err)
	}
	p.Proxmox = &opts
	if p.Options != nil {
		delete(p.Options, "networkInterfaces")
		delete(p.Options, "vmOptions")
	}
	return p, nil
}

type ProxmoxProviderOptions struct {
	NetworkInterfaces []ProxmoxNetworkInterface `yaml:"networkInterfaces"`
	VMOptions         []ProxmoxOption           `yaml:"vmOptions"`
}

type ProxmoxNetworkInterface struct {
	Name      string `yaml:"name"`
	Model     string `yaml:"model"`
	Bridge    string `yaml:"bridge"`
	VLANTag   int    `yaml:"vlanTag"`
	MACPrefix string `yaml:"macPrefix"`
}

type ProxmoxOption struct {
	Name  string      `yaml:"name"`
	Value interface{} `yaml:"value"`
}
