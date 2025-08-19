package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"

	"github.com/luthermonson/go-proxmox"
)

const (
	VMID_RANGE_LOWER = 1250
	VMID_RANGE_UPPER = 1300
)

func printNodes(nodeStatuses []*proxmox.NodeStatus) {
	fmt.Print("hello!!!\n")
	for _, status := range nodeStatuses {
		fmt.Printf("Node: %s, Status: %s,  Uptime: %d\n",
			status.Node,
			status.Status,
			status.Uptime,
		)
	}
}

func generateNewVMID(vmids []uint64) uint64 {
	// TODO: This potentially can infinite loop due to VMID exhaustion
	for {
		vmid := rand.Uint64N(VMID_RANGE_UPPER-VMID_RANGE_LOWER) + VMID_RANGE_LOWER
		if !slices.Contains(vmids, vmid) {
			return vmid
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

func createVM(client proxmox.Client) {
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

	// newVMID := generateNewVMID(vmids)
	newVMID := 1256

	fmt.Printf("New VMID: %d\n", newVMID)

	// Create the VM on Alfaromeo
	// node, err := getNodeByName(nodeStatuses, "alfaromeo")
	node, err := client.Node(context.Background(), "alfaromeo")
	if err != nil {
		fmt.Printf("Error getting node: %v\n", err)
		panic(err)
	}

	// node.NewVirtualMachine(ctx, int(newVMID),)

	// vm, err := node.VirtualMachine(ctx, 5000)
	// if err != nil {
	// 	fmt.Printf("Error getting virtual machine: %v\n", err)
	// 	panic(err)
	// }

	// fmt.Printf("VM: %+v\n", vm.VirtualMachineConfig)

	opts := []proxmox.VirtualMachineOption{
		{Name: "name", Value: "golang-test"},
		{Name: "sockets", Value: 1},
		{Name: "cores", Value: 4},
		{Name: "cpu", Value: "host"},
		{Name: "memory", Value: 4096},
		{Name: "boot", Value: "order=net0"},
		{Name: "net0", Value: "virtio=AC:E1:AF:9D:E2:E2,bridge=vmbr0,tag=20"},
		{Name: "ostype", Value: "l26"},
		{Name: "cicustom", Value: "meta=snippets:snippets/zagato-shared-cloud-init-meta-data.yaml"},
		{Name: "ipconfig0", Value: "ip=dhcp"},
		{Name: "ide2", Value: "local-zfs:cloudinit"},
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
	vm, err := node.VirtualMachine(ctx, int(newVMID))
	if err != nil {
		panic(err)
	}
	if _, err := vm.Start(ctx); err != nil {
		panic(err)
	}
}

func main() {
	insecureHTTPClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	tokenID := "root@pam!erik-dev-key"
	secret := "9420db92-429f-4422-9c2f-d41b3d711b51"

	client := proxmox.NewClient("https://100.64.0.25:8006/api2/json",
		proxmox.WithHTTPClient(&insecureHTTPClient),
		proxmox.WithAPIToken(tokenID, secret),
	)

	version, err := client.Version(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(version.Release) // 6.3

	createVM(*client)

}
