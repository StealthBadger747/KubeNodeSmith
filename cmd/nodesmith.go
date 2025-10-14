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

		// Put labels on kube node
		if err := kube.LabelNode(ctx, cs, kubeNodeName, nodepoolCfg.MachineTemplate.Labels); err != nil {
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

			err := kube.CordonNode(ctx, cs, node.Name)
			if err != nil {
				panic(err)
			}

			evictable, err := kube.GetEvictablePods(ctx, cs, node.Name)
			if err != nil {
				panic(err)
			}
			if len(evictable) != 0 {
				panic(fmt.Errorf("node not empty"))
			}

			err = cs.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})
			if err != nil {
				panic(fmt.Errorf("deleting node object: %v", err))
			}

			err = provider.DeprovisionMachine(ctx, providerpkg.Machine{
				KubeNodeName: node.Name,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: deprovisioning machine `%s`: %v\n", node.Name, err)
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

	for {
		// Scaling loop
		for _, np := range cfg.NodePools {
			prov, ok := providers[np.ProviderRef]
			if !ok {
				log.Fatalf("provider %s not found for nodepool", np.ProviderRef)
			}

			scaler(ctx, kube.GetClientset(), &np, *prov)
		}

		fmt.Printf("\n--------------------------------\n\n")

		time.Sleep(pollInterval)
	}
}
