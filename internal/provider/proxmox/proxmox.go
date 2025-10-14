package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"

	proxmoxapi "github.com/luthermonson/go-proxmox"

	"github.com/StealthBadger747/KubeNodeSmith/internal/config"
	"github.com/StealthBadger747/KubeNodeSmith/internal/provider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gopkg.in/yaml.v3"
)

// Options captures provider-scoped configuration decoded from config.ProviderConfig.Options.
type Options struct {
	Endpoint         string
	NodeWhitelist    []string
	VMIDRange        VMIDRange
	VMMemOverheadMiB int64
	managedNodeTag   string
	Proxmox          *config.ProxmoxProviderOptions
}

// VMIDRange describes the inclusive lower/upper bounds allowed for new VMs.
type VMIDRange struct {
	Lower uint64
	Upper uint64
}

// Credentials bundles the auth material required to talk to Proxmox.
type Credentials struct {
	TokenID               string
	Secret                string
	InsecureSkipTLSVerify bool
}

// Provider implements provider.Provider for Proxmox clusters.
type Provider struct {
	client proxmoxapi.Client
	opts   Options
}

// Endpoint returns the API endpoint configured for this provider.
func (p *Provider) Endpoint() string {
	return p.opts.Endpoint
}

func generateNewVMID(existingVMIDs []uint64, opts Options) (int, error) {
	if opts.VMIDRange.Upper <= opts.VMIDRange.Lower {
		return 0, fmt.Errorf("invalid VMID range: lower=%d upper=%d", opts.VMIDRange.Lower, opts.VMIDRange.Upper)
	}
	span := opts.VMIDRange.Upper - opts.VMIDRange.Lower + 1
	if span == 0 {
		return 0, fmt.Errorf("vmid range overflow")
	}

	used := make(map[uint64]struct{}, len(existingVMIDs))
	for _, id := range existingVMIDs {
		used[id] = struct{}{}
	}
	if uint64(len(used)) >= span {
		return 0, fmt.Errorf("no VMIDs available in range [%d,%d]", opts.VMIDRange.Lower, opts.VMIDRange.Upper)
	}

	start := opts.VMIDRange.Lower + rand.Uint64N(span)
	for i := uint64(0); i < span; i++ {
		candidate := start + i
		if candidate > opts.VMIDRange.Upper {
			candidate = opts.VMIDRange.Lower + (candidate - opts.VMIDRange.Upper - 1)
		}
		if _, taken := used[candidate]; !taken {
			return int(candidate), nil
		}
	}

	return 0, fmt.Errorf("no VMIDs available in range [%d,%d]", opts.VMIDRange.Lower, opts.VMIDRange.Upper)
}

// Generates a randomized MAC address
func generateRandomMAC(prefix string) string {
	b := [6]byte{
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
	}
	if prefix != "" {
		parts := strings.Split(prefix, ":")
		for i := 0; i < len(parts) && i < len(b); i++ {
			if parts[i] == "" {
				continue
			}
			if val, err := strconv.ParseUint(parts[i], 16, 8); err == nil {
				b[i] = byte(val)
			}
		}
	}
	b[0] &^= 0x01 // ensure unicast
	b[0] |= 0x02  // locally administered
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// NewProvider constructs a Proxmox-backed provider instance. It is expected to be called by the
// autoscaler during startup. Secrets needed for authentication should already be resolved by the
// caller and passed in via creds.
func NewProvider(ctx context.Context, cfg config.ProviderConfig) (*Provider, error) {
	parsedOpts, err := parseOptions(cfg)
	if err != nil {
		return nil, fmt.Errorf("parse proxmox options: %w", err)
	}

	creds, err := loadCredentials(ctx, cfg.CredentialsRef)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{}
	if creds.InsecureSkipTLSVerify {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	client := proxmoxapi.NewClient(
		parsedOpts.Endpoint,
		proxmoxapi.WithHTTPClient(httpClient),
		proxmoxapi.WithAPIToken(creds.TokenID, creds.Secret),
	)

	return &Provider{
		client: *client,
		opts:   parsedOpts,
	}, nil
}

// parseOptions converts the generic map of provider options into a strongly typed Options struct.
func parseOptions(cfg config.ProviderConfig) (Options, error) {
	if cfg.Type != "proxmox" {
		return Options{}, fmt.Errorf("expected proxmox provider config")
	}
	if cfg.Options == nil {
		return Options{}, fmt.Errorf("proxmox provider options are required")
	}
	if cfg.Proxmox == nil {
		return Options{}, fmt.Errorf("proxmox provider vmOptions are required")
	}

	encoded, err := yaml.Marshal(cfg.Options)
	if err != nil {
		return Options{}, fmt.Errorf("marshal proxmox options: %w", err)
	}

	var spec struct {
		Endpoint      string   `yaml:"endpoint"`
		NodeWhitelist []string `yaml:"nodeWhitelist"`
		VMIDRange     *struct {
			Lower int64 `yaml:"lower"`
			Upper int64 `yaml:"upper"`
		} `yaml:"vmIDRange"`
		VMMemOverheadMiB int64  `yaml:"vmMemOverheadMiB"`
		managedNodeTag   string `yaml:"managedNodeTag"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return Options{}, fmt.Errorf("decode proxmox options: %w", err)
	}

	if spec.Endpoint == "" {
		return Options{}, fmt.Errorf("proxmox option endpoint is required")
	}

	opts := Options{
		Endpoint:         spec.Endpoint,
		NodeWhitelist:    append([]string(nil), spec.NodeWhitelist...),
		VMMemOverheadMiB: spec.VMMemOverheadMiB,
		managedNodeTag:   spec.managedNodeTag,
	}

	if spec.VMIDRange != nil {
		if spec.VMIDRange.Lower > spec.VMIDRange.Upper {
			return Options{}, fmt.Errorf("proxmox option vmIDRange.lower must be <= upper")
		}
		opts.VMIDRange = VMIDRange{Lower: uint64(spec.VMIDRange.Lower), Upper: uint64(spec.VMIDRange.Upper)}
	}

	opts.Proxmox = cfg.Proxmox
	return opts, nil
}

// loadCredentials fetches and validates the credentials referenced by ref.
func loadCredentials(ctx context.Context, ref *config.CredentialsRef) (Credentials, error) {
	if ref == nil {
		return Credentials{}, fmt.Errorf("credentialsRef is required")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return Credentials{}, fmt.Errorf("build in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return Credentials{}, fmt.Errorf("create kubernetes client: %w", err)
	}

	secret, err := clientset.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	tokenID := strings.TrimSpace(string(secret.Data["PROXMOX_TOKEN_ID"]))
	if tokenID == "" {
		return Credentials{}, fmt.Errorf("secret %s/%s missing PROXMOX_TOKEN_ID", ref.Namespace, ref.Name)
	}

	secretValue := strings.TrimSpace(string(secret.Data["PROXMOX_SECRET"]))
	if secretValue == "" {
		return Credentials{}, fmt.Errorf("secret %s/%s missing PROXMOX_SECRET", ref.Namespace, ref.Name)
	}

	insecure := false
	if raw, ok := secret.Data["PROXMOX_SKIP_TLS_VERIFY"]; ok {
		val := strings.TrimSpace(string(raw))
		if val != "" {
			parsed, err := strconv.ParseBool(val)
			if err != nil {
				return Credentials{}, fmt.Errorf("secret %s/%s invalid PROXMOX_SKIP_TLS_VERIFY: %w", ref.Namespace, ref.Name, err)
			}
			insecure = parsed
		}
	}

	return Credentials{
		TokenID:               tokenID,
		Secret:                secretValue,
		InsecureSkipTLSVerify: insecure,
	}, nil
}

// TODO: Make this smarter
// getAvailableNode randomly probes nodes (whitelist first if provided) and
// returns the first one that passes a basic mem/CPU check.
func (p *Provider) getAvailableNode(ctx context.Context, spec provider.MachineSpec) (*proxmoxapi.Node, error) {
	const (
		cpuUtilMax = 0.95 // basic sanity ceiling; adjust as needed
	)

	cluster, err := p.client.Cluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("getAvailableNode: cluster: %w", err)
	}
	// Node-level utilization snapshot.
	nodeRes, err := cluster.Resources(ctx, "node")
	if err != nil {
		return nil, fmt.Errorf("getAvailableNode: node resources: %w", err)
	}

	// Build a lookup of resource by node name.
	resByNode := make(map[string]proxmoxapi.ClusterResource, len(nodeRes))
	allNames := make([]string, 0, len(nodeRes))
	for _, r := range nodeRes {
		if r.Node == "" {
			continue
		}
		resByNode[r.Node] = *r
		allNames = append(allNames, r.Node)
	}

	// Candidate order: whitelist (if any), otherwise all nodes.
	candidates := p.opts.NodeWhitelist
	if len(candidates) == 0 {
		candidates = allNames
	}

	// Randomize candidates.
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })

	needMiB := int64(spec.MemoryMiB) + p.opts.VMMemOverheadMiB
	if needMiB <= 0 {
		needMiB = 512 // safety default
	}

	for _, name := range candidates {
		r, ok := resByNode[name]
		if !ok {
			// Whitelist might include nodes not present in the current snapshot.
			continue
		}
		if r.MaxMem == 0 {
			continue
		}

		freeMiB := int64((r.MaxMem - r.Mem) / (1024 * 1024))
		if freeMiB < needMiB {
			continue
		}

		cpuUtil := r.CPU
		if cpuUtil > cpuUtilMax {
			continue
		}

		// Passed basic checks—return the live node handle.
		n, err := p.client.Node(ctx, name)
		if err != nil {
			// If node fetch fails transiently, try the next candidate.
			continue
		}
		return n, nil
	}

	return nil, fmt.Errorf("getAvailableNode: no node meets basic fit (need %d MiB, cpu<%.2f)", needMiB, cpuUtilMax)
}

// buildVirtualMachineOptions is a helper that will map MachineSpec + provider options into the list of
// proxmox VirtualMachineOption entries.
func buildVirtualMachineOptions(machineName string, spec provider.MachineSpec, opts Options) ([]proxmoxapi.VirtualMachineOption, error) {
	if opts.Proxmox == nil {
		return nil, fmt.Errorf("proxmox provider vmOptions not configured")
	}

	vmOpts := make([]proxmoxapi.VirtualMachineOption, 0, len(opts.Proxmox.VMOptions)+len(opts.Proxmox.NetworkInterfaces)+8)

	for _, opt := range opts.Proxmox.VMOptions {
		vmOpts = append(vmOpts, proxmoxapi.VirtualMachineOption{Name: opt.Name, Value: opt.Value})
	}

	setOption := func(name string, value any) {
		for i := range vmOpts {
			if vmOpts[i].Name == name {
				vmOpts[i].Value = value
				return
			}
		}
		vmOpts = append(vmOpts, proxmoxapi.VirtualMachineOption{Name: name, Value: value})
	}

	setOption("name", machineName)
	memory := spec.MemoryMiB
	if opts.VMMemOverheadMiB > 0 {
		memory += opts.VMMemOverheadMiB
	}
	setOption("memory", memory)
	setOption("cores", spec.CPUCores)

	for idx, nic := range opts.Proxmox.NetworkInterfaces {
		name := nic.Name
		if name == "" {
			name = fmt.Sprintf("net%d", idx)
		}
		model := nic.Model
		if model == "" {
			model = "virtio"
		}
		mac := generateRandomMAC(nic.MACPrefix)
		parts := []string{fmt.Sprintf("%s=%s", model, mac)}
		if nic.Bridge != "" {
			parts = append(parts, fmt.Sprintf("bridge=%s", nic.Bridge))
		}
		if nic.VLANTag != 0 {
			parts = append(parts, fmt.Sprintf("tag=%d", nic.VLANTag))
		}
		setOption(name, strings.Join(parts, ","))
	}

	return vmOpts, nil
}

// ProvisionMachine creates a new VM in the Proxmox cluster that will eventually join the Kubernetes cluster.
func (p *Provider) ProvisionMachine(ctx context.Context, spec provider.MachineSpec) (*provider.Machine, error) {
	cluster, err := p.client.Cluster(ctx)
	if err != nil {
		fmt.Printf("Error getting cluster: %v\n", err)
		panic(err)
	}
	clusterResources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		fmt.Printf("Error getting cluster resources: %v\n", err)
		panic(err)
	}

	vmids := make([]uint64, 0, len(clusterResources))

	for _, resource := range clusterResources {
		// fmt.Printf("VMID: %d\n", resource.VMID)
		vmids = append(vmids, resource.VMID)
	}

	newVMID, err := generateNewVMID(vmids, p.opts)
	if err != nil {
		return nil, fmt.Errorf("allocate VMID: %w", err)
	}
	proxNode, err := p.getAvailableNode(ctx, spec)
	if err != nil {
		fmt.Printf("Error getting available proxmox node: %v\n", err)
		panic(err)
	}

	fmt.Printf("New VMID: %d, on proxmox node: %s\n", newVMID, proxNode.Name)

	machineName := fmt.Sprintf("%s-%d", spec.NamePrefix, newVMID)

	vmOptions, err := buildVirtualMachineOptions(machineName, spec, p.opts)
	if err != nil {
		return nil, fmt.Errorf("build VM options: %w", err)
	}

	newVMTask, err := proxNode.NewVirtualMachine(ctx, newVMID, vmOptions...)
	if err != nil {
		fmt.Printf("Error creating new VM: %v\n", err)
		panic(err)
	}

	// 5) Wait for task to finish
	if err := newVMTask.WaitFor(ctx, 600); err != nil {
		panic(err)
	}

	// 6) Power on
	vm, err := proxNode.VirtualMachine(ctx, newVMID)
	if err != nil {
		panic(err)
	}
	if _, err := vm.Start(ctx); err != nil {
		panic(err)
	}

	return &provider.Machine{
		ProviderID:   machineName,
		KubeNodeName: machineName,
	}, nil
}

func findNodeWithVM(vmName string, ctx context.Context, proxClient *proxmoxapi.Client) (*proxmoxapi.Node, *proxmoxapi.VirtualMachine, error) {
	nodeStatuses, err := proxClient.Nodes(ctx)
	if err != nil {
		fmt.Printf("Error getting nodeStatuses: %v\n", err)
		panic(err)
	}
	for _, nodeStatus := range nodeStatuses {
		node, err := proxClient.Node(ctx, nodeStatus.Name)
		if err != nil {
			fmt.Printf("Error getting node: %v\n", err)
			panic(err)
		}
		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			fmt.Printf("Error getting vms: %v\n", err)
			panic(err)
		}
		for _, vm := range vms {
			if vm.Name == vmName {
				return node, vm, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("VM with name %s not found", vmName)
}

// DeprovisionMachine deletes a VM previously created by ProvisionMachine.
func (p *Provider) DeprovisionMachine(ctx context.Context, machine provider.Machine) error {
	_, proxVM, err := findNodeWithVM(machine.KubeNodeName, ctx, &p.client)
	if err != nil {
		return err
	}

	if !strings.Contains(proxVM.Tags, p.opts.managedNodeTag) {
		err := fmt.Errorf("refusing to delete targeted VM `%s` in node %s because it does not have required tag", proxVM.Name, machine.KubeNodeName)
		return err
	}

	stopVMTask, err := proxVM.Stop(ctx)
	if err != nil {
		fmt.Printf("Error Stopping VM: %v\n", err)
		return err
	}

	if err := stopVMTask.WaitFor(ctx, 600); err != nil {
		return err
	}

	deleteVMTask, err := proxVM.Delete(ctx)
	if err != nil {
		fmt.Printf("Error Deleting VM: %v\n", err)
		return err
	}

	if err := deleteVMTask.WaitFor(ctx, 600); err != nil {
		return err
	}

	return nil
}

// ListMachines returns the set of machines currently managed by this provider. The result should only include machines that match the prefixes
// and tags supplied during provisioning so the autoscaler can detect drift.
func (p *Provider) ListMachines(ctx context.Context, namePrefix string) ([]provider.Machine, error) {
	nodeStatuses, err := p.client.Nodes(ctx)
	if err != nil {
		fmt.Printf("Error getting nodeStatuses: %v\n", err)
		panic(err)
	}
	machines := []provider.Machine{}
	for _, nodeStatus := range nodeStatuses {
		node, err := p.client.Node(ctx, nodeStatus.Name)
		if err != nil {
			fmt.Printf("Error getting node: %v\n", err)
			panic(err)
		}
		vms, err := node.VirtualMachines(ctx)
		if err != nil {
			fmt.Printf("Error getting vms: %v\n", err)
			panic(err)
		}
		for _, vm := range vms {
			if strings.HasPrefix(vm.Name, namePrefix) && strings.Contains(vm.Tags, p.opts.managedNodeTag) {
				machines = append(machines, provider.Machine{
					ProviderID:   vm.Name,
					KubeNodeName: vm.Name,
				})
			}
		}
	}
	return machines, nil
}
