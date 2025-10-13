package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	kube "kubernetes-autoscaler/internal"

	"github.com/luthermonson/go-proxmox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	VMID_RANGE_LOWER = 1250
	VMID_RANGE_UPPER = 1300
	VM_TAG           = "zagato-k3s-auto"
	VM_NAME_PREFIX   = "zagato-worker-auto"
	VM_MEM_OVERHEAD  = 2046
)

func printNodes(nodeStatuses []*proxmox.NodeStatus) {
	for _, status := range nodeStatuses {
		fmt.Printf("Node: %s, Status: %s,  Uptime: %d\n",
			status.Node,
			status.Status,
			status.Uptime,
		)
	}
}

func generateNewVMID(vmids []uint64) int {
	// TODO: This potentially can infinite loop due to VMID exhaustion
	for {
		vmid := rand.Uint64N(VMID_RANGE_UPPER-VMID_RANGE_LOWER) + VMID_RANGE_LOWER
		if !slices.Contains(vmids, vmid) {
			return int(vmid)
		}
	}
}

// func getNodeByName(nodeStatuses proxmox.NodeStatuses, targetName string) (proxmox.Node, error) {
// 	for _, nodeStatus := range nodeStatuses {
// 		if nodeStatus.Name == targetName {
// 			return nodeStatus.Node, nil
// 		}
// 	}
// 	return nil, fmt.Errorf("node with name %q not found", targetName)
// }

func generateRandomMAC() string {
	b := [6]byte{
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
		byte(rand.Uint32N(256)),
	}
	b[0] &^= 0x01 // ensure unicast
	b[0] |= 0x02  // locally administered
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

func createVM(client proxmox.Client, nodeName string, cpu int64, memMiB int64) (string, int) {
	ctx := context.Background()

	// Steps:
	// 1. Get a list of all VMIDs already in use
	// 2. Create a VM in the valid range specified (randomized and unused VMID)
	// 3. Profit???

	nodeStatuses, err := client.Nodes(ctx)
	if err != nil {
		fmt.Printf("Error getting nodeStatuses: %v\n", err)
		panic(err)
	}
	printNodes(nodeStatuses)

	cluster, err := client.Cluster(ctx)
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

	newVMID := generateNewVMID(vmids)

	fmt.Printf("New VMID: %d\n", newVMID)

	// Create the VM on Alfaromeo
	// node, err := getNodeByName(nodeStatuses, "alfaromeo")
	node, err := client.Node(context.Background(), nodeName)
	if err != nil {
		fmt.Printf("Error getting node: %v\n", err)
		panic(err)
	}

	nodeName = fmt.Sprintf("%s-%d", VM_NAME_PREFIX, newVMID)

	// node.NewVirtualMachine(ctx, int(newVMID),)

	// vm, err := node.VirtualMachine(ctx, 5000)
	// if err != nil {
	// 	fmt.Printf("Error getting virtual machine: %v\n", err)
	// 	panic(err)
	// }

	// fmt.Printf("VM: %+v\n", vm.VirtualMachineConfig)

	opts := []proxmox.VirtualMachineOption{
		{Name: "name", Value: nodeName},
		{Name: "sockets", Value: 1},
		{Name: "cpu", Value: "host"},
		{Name: "cores", Value: cpu},
		{Name: "memory", Value: memMiB},
		{Name: "boot", Value: "order=net0"},
		{Name: "net0", Value: fmt.Sprintf("virtio=%s,bridge=vmbr0,tag=20", generateRandomMAC())},
		{Name: "ostype", Value: "l26"},
		{Name: "cicustom", Value: "meta=snippets:snippets/zagato-shared-cloud-init-meta-data.yaml"},
		{Name: "ipconfig0", Value: "ip=dhcp"},
		{Name: "ide2", Value: "local-zfs:cloudinit"},
		{Name: "scsi0", Value: "local-zfs:16"},
		{Name: "tags", Value: VM_TAG},
		// {Name: "", Value: },
		// {Name: "", Value: },
		// {Name: "", Value: },
		// {Name: "", Value: },
	}

	newVMTask, err := node.NewVirtualMachine(ctx, newVMID, opts...)
	if err != nil {
		fmt.Printf("Error creating new VM: %v\n", err)
		panic(err)
	}

	// 5) Wait for task to finish
	if err := newVMTask.WaitFor(ctx, 600); err != nil {
		panic(err)
	}

	// 6) Power on
	vm, err := node.VirtualMachine(ctx, newVMID)
	if err != nil {
		panic(err)
	}
	if _, err := vm.Start(ctx); err != nil {
		panic(err)
	}

	return nodeName, newVMID
}

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

func deleteVM(client proxmox.Client, proxNodeName string, vmid int) {
	ctx := context.Background()
	kubeNodeName := fmt.Sprintf("%s-%d", VM_NAME_PREFIX, vmid)

	// Kube steps
	clientset := kube.GetClientset()

	err := kube.CordonNode(ctx, clientset, kubeNodeName)
	if err != nil {
		panic(err)
	}

	evictable, err := kube.GetEvictablePods(ctx, clientset, kubeNodeName)
	if err != nil {
		panic(err)
	}
	if len(evictable) != 0 {
		panic(fmt.Errorf("node not empty"))
	}

	clientset.CoreV1().Nodes().Delete(ctx, kubeNodeName, metav1.DeleteOptions{})

	// Proxmox steps
	node, err := client.Node(context.Background(), proxNodeName)
	if err != nil {
		fmt.Printf("Error getting node: %v\n", err)
		panic(err)
	}

	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		panic(err)
	}
	if !strings.Contains(vm.Tags, VM_TAG) {
		err := fmt.Errorf("refusing to delete targeted VMID %d in node %s because it does not have required tag", vmid, proxNodeName)
		panic(err)
	}

	stopVMTask, err := vm.Stop(ctx)
	if err != nil {
		fmt.Printf("Error Stopping VM: %v\n", err)
		panic(err)
	}

	if err := stopVMTask.WaitFor(ctx, 600); err != nil {
		panic(err)
	}

	deleteVMTask, err := vm.Delete(ctx)
	if err != nil {
		fmt.Printf("Error Deleting VM: %v\n", err)
		panic(err)
	}

	if err := deleteVMTask.WaitFor(ctx, 600); err != nil {
		panic(err)
	}
}

func printUsage() {
	fmt.Println(`Usage:
  proxctl <command> [options]

Commands:
  create        Create a VM
  delete        Delete a VM

Global environment:
  PROXMOX_SECRET   API token secret (required)

Subcommand options:
  create:
    --node <name>   Proxmox node (default "alfaromeo")

  delete:
    --node <name>   Proxmox node (default "alfaromeo")
    --vmid <id>     VMID to delete (required)`)
}

func old_main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	// Shared client setup
	insecureHTTPClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	tokenID := "root@pam!erik-dev-key"
	secret := os.Getenv("PROXMOX_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "error: PROXMOX_SECRET environment variable not set")
		os.Exit(1)
	}
	client := proxmox.NewClient("https://100.64.0.25:8006/api2/json",
		proxmox.WithHTTPClient(&insecureHTTPClient),
		proxmox.WithAPIToken(tokenID, secret),
	)

	// Print Proxmox version once for visibility
	version, err := client.Version(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "version error:", err)
		os.Exit(1)
	}
	fmt.Println(version.Release)

	switch os.Args[1] {
	case "create":
		createFlags := flag.NewFlagSet("create", flag.ExitOnError)
		node := createFlags.String("node", "alfaromeo", "Proxmox node")
		_ = createFlags.Parse(os.Args[2:])

		createVM(*client, *node, 4, 4096)

	case "delete":
		deleteFlags := flag.NewFlagSet("delete", flag.ExitOnError)
		node := deleteFlags.String("node", "alfaromeo", "Proxmox node")
		vmid := deleteFlags.Int("vmid", -1, "VMID to delete (required)")
		_ = deleteFlags.Parse(os.Args[2:])

		if *vmid <= 0 {
			fmt.Fprintln(os.Stderr, "error: --vmid is required and must be > 0")
			deleteFlags.Usage()
			os.Exit(2)
		}
		deleteVM(*client, *node, *vmid)

	case "auto":
		ctx := context.Background()
		cs := kube.GetClientset()
		startHealthServer()

		for {
			pods, err := kube.GetUnschedulablePods(ctx, cs)
			if err != nil {
				fmt.Fprintln(os.Stderr, "unsched list error:", err)
				os.Exit(1)
			}
			fmt.Println("Looking for scale up opportunities!")
			if len(pods) != 0 {
				// Scale up....
				fmt.Println("Found pods to scale up!")

				kube.PrintPods(pods)

				lfse, _ := kube.LastFailedSchedulingEvent(ctx, cs, &pods[0])
				fmt.Printf("LFSE: %s\n", lfse)

				cpuMili, memBytes := kube.GetRequestedResources(&pods[0])
				cpu := int64(math.Ceil(float64(cpuMili) / 1000))
				memMiB := memBytes / (1024 * 1024)
				fmt.Printf("CPU: %d\n", cpu)
				fmt.Printf("memBytes: %d\n", memMiB)

				nodeName, vmid := createVM(*client, "alfaromeo", cpu, memMiB+VM_MEM_OVERHEAD)

				// Wait for the new node to be ready
				fmt.Printf("Waiting for VM name: %s vmid: %d to be ready...\n", nodeName, vmid)
				if err := kube.WaitForNodeReady(ctx, cs, nodeName, 5*time.Minute); err != nil {
					fmt.Printf("Warning: VM %d failed to become ready: %v\n", vmid, err)
				}
			} else {
				// Scale Down...
				// TODO: acquire a lease

				fmt.Println("Looking for nodes to scale down!")

				nodes, err := kube.GetScaleDownCandiates(ctx, cs, VM_NAME_PREFIX)
				if err != nil {
					panic(err)
				}

				for _, node := range nodes {
					fmt.Printf("Found node `%s` to scale down!\n", node.Name)
					nodeNameParts := strings.Split(node.Name, "-")
					if len(nodeNameParts) > 0 {
						lastPart := nodeNameParts[len(nodeNameParts)-1]
						vmid, err := strconv.Atoi(lastPart)
						if err != nil {
							panic(fmt.Sprintf("invalid node name `%s`", node.Name))
						}
						deleteVM(*client, "alfaromeo", vmid)
					} else {
						panic(fmt.Sprintf("invalid node name `%s`", node.Name))
					}
				}
			}

			time.Sleep(30 * time.Second)

		}

	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}
