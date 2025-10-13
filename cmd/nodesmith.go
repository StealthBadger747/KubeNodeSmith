package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	kube "kubernetes-autoscaler/internal"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

func main() {
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
