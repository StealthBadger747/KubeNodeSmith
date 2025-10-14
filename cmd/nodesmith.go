package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	kube "github.com/StealthBadger747/KubeNodeSmith/internal"
	cfgpkg "github.com/StealthBadger747/KubeNodeSmith/internal/config"
	providerpkg "github.com/StealthBadger747/KubeNodeSmith/internal/provider"
	proxprovider "github.com/StealthBadger747/KubeNodeSmith/internal/provider/proxmox"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func startHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "health server error: %v\n", err)
		}
	}()
}

// checkPoolLimits verifies if adding a new node with the specified resources would exceed pool limits
// Returns true if limits would be exceeded, false otherwise
func checkPoolLimits(poolUsage *kube.PoolResourceUsage, limits *cfgpkg.NodePoolLimits, newNodeCPUMilli int64, newNodeMemBytes int64) (bool, string) {
	// Convert limits to millicores/bytes
	limitCPUMilli := limits.CPUCores * 1000
	limitMemBytes := limits.MemoryMiB * 1024 * 1024

	// Estimate new node capacity based on pod requirements (this is what the provider will provision)
	// For now, assume the new node will be sized to accommodate this pod
	// In a more sophisticated autoscaler, this would be based on machine templates
	estimatedNewNodeCPUMilli := newNodeCPUMilli
	estimatedNewNodeMemBytes := newNodeMemBytes

	// Check CPU limits
	if limits.CPUCores > 0 && (poolUsage.TotalCPUMilli+estimatedNewNodeCPUMilli) > limitCPUMilli {
		return true, fmt.Sprintf("Adding new node with ~%d CPU millicores would exceed pool limit of %d (current total: %d)",
			estimatedNewNodeCPUMilli, limitCPUMilli, poolUsage.TotalCPUMilli)
	}

	// Check memory limits
	if limits.MemoryMiB > 0 && (poolUsage.TotalMemoryBytes+estimatedNewNodeMemBytes) > limitMemBytes {
		return true, fmt.Sprintf("Adding new node with ~%d bytes memory would exceed pool limit of %d (current total: %d)",
			estimatedNewNodeMemBytes, limitMemBytes, poolUsage.TotalMemoryBytes)
	}

	return false, ""
}

// deleteNode handles the complete node deletion process including cordon, pod eviction check, and machine deprovisioning
func deleteNode(ctx context.Context, cs *kubernetes.Clientset, provider proxprovider.Provider, nodeName string) error {
	fmt.Printf("Deleting node `%s`\n", nodeName)

	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
		return fmt.Errorf("node %s is a control plane node, refusing deletion", nodeName)
	}
	if _, isMaster := node.Labels["node-role.kubernetes.io/master"]; isMaster {
		return fmt.Errorf("node %s is a master node, refusing deletion", nodeName)
	}

	// Cordon the node
	if err := kube.CordonNode(ctx, cs, nodeName); err != nil {
		return fmt.Errorf("cordon node %s: %w", nodeName, err)
	}

	// Check for evictable pods
	evictable, err := kube.GetEvictablePods(ctx, cs, nodeName)
	if err != nil {
		return fmt.Errorf("get evictable pods on node %s: %w", nodeName, err)
	}

	if len(evictable) != 0 {
		return fmt.Errorf("node %s not empty (has %d evictable pods), skipping deletion", nodeName, len(evictable))
	}

	// Delete the node object
	if err := cs.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting node object %s: %w", nodeName, err)
	}

	// Deprovision the machine
	if err := provider.DeprovisionMachine(ctx, providerpkg.Machine{
		KubeNodeName: nodeName,
	}); err != nil {
		return fmt.Errorf("deprovisioning machine %s: %w", nodeName, err)
	}

	fmt.Printf("Successfully deleted node `%s`\n", nodeName)
	return nil
}

// Reconciler looks for nodes that are orphaned (missing pool label) and deletes them
func reconciler(ctx context.Context, cs *kubernetes.Clientset, nodepoolCfg *cfgpkg.NodePool, provider proxprovider.Provider) {
	fmt.Printf("Reconciling nodepool %q", nodepoolCfg.Name)

	nodesInPool, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list nodes in pool %q: %v\n", nodepoolCfg.Name, err)
		return
	}

	poolLabelKey := nodepoolCfg.GetPoolLabelKey()
	namePrefix := nodepoolCfg.MachineTemplate.KubeNodeNamePrefix

	for _, node := range nodesInPool.Items {
		if !strings.HasPrefix(node.Name, namePrefix) {
			continue
		}
		if _, hasPoolLabel := node.Labels[poolLabelKey]; hasPoolLabel {
			continue
		}
		if node.DeletionTimestamp != nil {
			continue
		}
		if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			continue
		}
		if _, isMaster := node.Labels["node-role.kubernetes.io/master"]; isMaster {
			continue
		}

		fmt.Printf("Found node `%s` without pool label to reconcile (delete)\n", node.Name)

		if err := deleteNode(ctx, cs, provider, node.Name); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile node %s: %v\n", node.Name, err)
		}
	}
}

func scaler(ctx context.Context, cs *kubernetes.Clientset, nodepoolCfg *cfgpkg.NodePool, provider proxprovider.Provider) {
	pods, err := kube.GetUnschedulablePods(ctx, cs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unsched list error:", err)
		os.Exit(1)
	}

	nodesInPool, err := kube.GetNodesByLabel(ctx, cs, nodepoolCfg.GetPoolLabelKey(), nodepoolCfg.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list nodes in pool %q: %v\n", nodepoolCfg.Name, err)
		return
	}
	nodePoolSize := len(nodesInPool)

	fmt.Println("Looking for scale up opportunities!")
	if len(pods) != 0 {
		// Scale up....
		fmt.Println("Found pods to scale up!")

		kube.PrintPods(pods)

		lfse, _ := kube.LastFailedSchedulingEvent(ctx, cs, &pods[0])
		fmt.Printf("LFSE: %s\n", lfse)

		if nodepoolCfg.Limits.MaxNodes > 0 && nodePoolSize >= nodepoolCfg.Limits.MaxNodes {
			fmt.Printf("Node pool %q at or above max size (%d), skipping scale up\n", nodepoolCfg.Name, nodepoolCfg.Limits.MaxNodes)
			return
		}

		cpuMili, memBytes := kube.GetRequestedResources(&pods[0])
		cpu := int64(math.Ceil(float64(cpuMili) / 1000))
		memMiB := memBytes / (1024 * 1024)
		fmt.Printf("Pod requires CPU: %d cores, Memory: %d MiB\n", cpu, memMiB)

		// Get current pool resource usage
		poolUsage, err := kube.GetPoolResourceUsage(ctx, cs, nodepoolCfg.GetPoolLabelKey(), nodepoolCfg.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get pool resource usage: %v\n", err)
			return
		}

		// Display comprehensive resource information
		fmt.Printf("Pool resource status across %d nodes:\n", poolUsage.NodeCount)
		fmt.Printf("  Total system capacity: %d CPU cores, %d MiB memory\n",
			poolUsage.TotalCPUMilli/1000, poolUsage.TotalMemoryBytes/(1024*1024))
		fmt.Printf("  Allocatable capacity: %d CPU cores, %d MiB memory\n",
			poolUsage.AllocatableCPUMilli/1000, poolUsage.AllocatableMemoryBytes/(1024*1024))
		fmt.Printf("  Allocated: %d CPU cores, %d MiB memory\n",
			poolUsage.AllocatedCPUMilli/1000, poolUsage.AllocatedMemoryBytes/(1024*1024))
		fmt.Printf("  Available: %d CPU cores, %d MiB memory\n",
			poolUsage.AvailableCPUMilli/1000, poolUsage.AvailableMemoryBytes/(1024*1024))

		// Check if adding a new node sized for this pod would exceed pool limits
		if exceeded, reason := checkPoolLimits(poolUsage, &nodepoolCfg.Limits, cpuMili, memBytes); exceeded {
			fmt.Printf("%s, skipping scale up\n", reason)
			return
		}

		provisionedMachine, err := provider.ProvisionMachine(ctx, providerpkg.MachineSpec{
			NamePrefix: nodepoolCfg.MachineTemplate.KubeNodeNamePrefix,
			CPUCores:   cpu,
			MemoryMiB:  memMiB,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "provision machine: %v\n", err)
			return
		}
		kubeNodeName := provisionedMachine.KubeNodeName

		// Wait for the new node to be ready
		fmt.Printf("Waiting for VM name: %s to be ready...\n", kubeNodeName)
		if err := kube.WaitForNodeReady(ctx, cs, kubeNodeName, 5*time.Minute); err != nil {
			fmt.Printf("Warning: VM %s failed to become ready: %v\n", kubeNodeName, err)
		}

		labels := make(map[string]string, len(nodepoolCfg.MachineTemplate.Labels)+1)
		for key, value := range nodepoolCfg.MachineTemplate.Labels {
			labels[key] = value
		}

		poolLabelKey := nodepoolCfg.GetPoolLabelKey()
		if existing, ok := labels[poolLabelKey]; ok && existing != nodepoolCfg.Name {
			fmt.Printf("Overriding pool label %s=%s with %s for nodepool %s\n", poolLabelKey, existing, nodepoolCfg.Name, nodepoolCfg.Name)
		}
		labels[poolLabelKey] = nodepoolCfg.Name

		// Put labels on kube node
		if err := kube.LabelNode(ctx, cs, kubeNodeName, labels); err != nil {
			fmt.Fprintf(os.Stderr, "failed to label node %s: %v\n", kubeNodeName, err)
		}

	} else {
		// Scale Down...
		// TODO: acquire a lease
		fmt.Println("Looking for nodes to scale down!")

		if nodepoolCfg.Limits.MinNodes > 0 && nodePoolSize <= nodepoolCfg.Limits.MinNodes {
			fmt.Printf("Node pool %q at or below min size (%d), skipping scale down\n", nodepoolCfg.Name, nodepoolCfg.Limits.MinNodes)
			return
		}

		nodes, err := kube.GetScaleDownCandiates(ctx, cs, nodepoolCfg.MachineTemplate.KubeNodeNamePrefix, nodepoolCfg.GetPoolLabelKey(), nodepoolCfg.Name)
		if err != nil {
			panic(err)
		}

		for _, node := range nodes {
			fmt.Printf("Found node `%s` to scale down!\n", node.Name)

			if err := deleteNode(ctx, cs, provider, node.Name); err != nil {
				fmt.Fprintf(os.Stderr, "scale down node %s: %v\n", node.Name, err)
			}
		}
	}
}

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", os.Getenv("SCALER_CONFIG_PATH"), "path to scaler config")
	flag.Parse()

	if configPath == "" {
		log.Fatal("config path must be provided via --config or SCALER_CONFIG_PATH")
	}

	cfg, err := cfgpkg.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	pollInterval := cfg.PollingInterval.AsDuration()
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
		log.Printf("polling interval not specified or <= 0, defaulting to %s", pollInterval)
	} else {
		log.Printf("polling interval set to %s", pollInterval)
	}

	ctx := context.Background()
	providers := make(map[string]*proxprovider.Provider)
	for name, providerCfg := range cfg.Providers {
		if providerCfg.Type != "proxmox" {
			continue
		}

		prox, err := proxprovider.NewProvider(ctx, providerCfg)
		if err != nil {
			log.Fatalf("init proxmox provider %s: %v", name, err)
		}
		providers[name] = prox
	}

	if len(providers) == 0 {
		log.Fatal("no proxmox provider found in configuration")
	}

	startHealthServer()

	i := 0
	for {
		// Scaling loop
		// Every 5th loop do a recoliliation pass
		fmt.Printf("\n--------------------------------\n")
		fmt.Printf("Starting scaling loop at %s\n", time.Now().Format(time.RFC3339))

		for _, np := range cfg.NodePools {
			prov, ok := providers[np.ProviderRef]
			if !ok {
				log.Fatalf("provider %s not found for nodepool", np.ProviderRef)
			}

			if i%5 == 0 {
				reconciler(ctx, kube.GetClientset(), &np, *prov)
				i = 0
			} else {
				scaler(ctx, kube.GetClientset(), &np, *prov)
			}
		}

		i++

		fmt.Printf("\n--------------------------------\n\n")

		time.Sleep(pollInterval)
	}
}
