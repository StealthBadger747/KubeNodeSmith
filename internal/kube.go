package kube

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	kubeconfigPath string
	kubeconfigOnce sync.Once
)

func initKubeconfigFlag() {
	defaultPath := ""
	if home := homedir.HomeDir(); home != "" {
		defaultPath = filepath.Join(home, ".kube", "config")
	}
	flag.StringVar(&kubeconfigPath, "kubeconfig", defaultPath, "(optional) absolute path to the kubeconfig file")
	flag.Parse()
}

func GetClientset() *kubernetes.Clientset {
	kubeconfigOnce.Do(initKubeconfigFlag)

	var (
		config *rest.Config
		err    error
	)

	if kubeconfigPath != "" {
		if _, statErr := os.Stat(kubeconfigPath); statErr == nil {
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
			if err != nil {
				panic(fmt.Errorf("failed to load kubeconfig %s: %w", kubeconfigPath, err))
			}
		}
	}

	if config == nil {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(fmt.Errorf("failed to create in-cluster config: %w", err))
		}
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	return clientset
}

func PrintNodes(nodes []corev1.Node) {
	fmt.Printf("Total Candidate Nodes: %d\n", len(nodes))
	fmt.Println("Nodes:")
	for _, node := range nodes {
		status := "Ready"
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue {
				status = "NotReady"
				break
			}
		}
		fmt.Printf("  - %s (%s)\n", node.Name, status)
	}
}

func isDaemonSetPod(p *corev1.Pod) bool {
	for i := range p.OwnerReferences {
		owner := p.OwnerReferences[i]
		if owner.Kind == "DaemonSet" && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func toleratesUnschedulable(pod *corev1.Pod) bool {
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == "node.kubernetes.io/unschedulable" && toleration.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func GetNodesByLabel(ctx context.Context, clientset *kubernetes.Clientset, labelKey string, labelValue string) ([]corev1.Node, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var foundNodes []corev1.Node

	for _, node := range nodes.Items {
		for k := range node.Labels {
			if k == labelKey && node.Labels[k] == labelValue {
				foundNodes = append(foundNodes, node)
				break
			}
		}

	}

	return foundNodes, nil
}

func CordonNode(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) error {
	node, err := clientset.CoreV1().Nodes().Get(
		ctx,
		nodeName,
		metav1.GetOptions{},
	)
	if err != nil {
		return err
	}

	node.Spec.Unschedulable = true

	_, err = clientset.CoreV1().Nodes().Update(
		ctx,
		node,
		metav1.UpdateOptions{},
	)

	return err
}

func PrintPods(pods []corev1.Pod) {
	fmt.Printf("Found %d pods:\n", len(pods))
	for _, p := range pods {
		node := p.Spec.NodeName
		if node == "" {
			node = "<unassigned>"
		}
		phase := string(p.Status.Phase)
		reason := ""
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				reason = c.Reason
				break
			}
		}

		fmt.Printf("  %-40s  ns=%-15s  node=%-20s  phase=%-10s  reason=%s\n",
			p.Name, p.Namespace, node, phase, reason)
	}
}

func isUnschedulable(condition corev1.PodCondition) bool {
	return condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable
}

func GetUnschedulablePods(ctx context.Context, clientset *kubernetes.Clientset) ([]corev1.Pod, error) {
	// Goal: return pods that *need capacity* (i.e., scheduler says they’re unschedulable),
	// not every pod in Pending (which could be image pulls/init containers).
	//
	// Steps:
	// 1) List unscheduled Pending pods only:
	//    - FieldSelector: spec.nodeName == ""   (unbound)
	//    - Filter:       status.phase == Pending
	//
	// 2) Exclude pods that should not trigger scale-out:
	//    - Static/mirror pods:   has annotation "kubernetes.io/config.mirror"
	//    - DaemonSet-managed:    ownerRef.Kind == "DaemonSet" && ownerRef.Controller == true
	//    - Terminal pods:        status.phase in {Succeeded, Failed} (defensive; should not appear)
	//
	// 3) Keep only pods the scheduler marked as unschedulable:
	//    - Look for a Pod condition: type=PodScheduled, status=False, reason="Unschedulable"
	//      (This means the scheduler tried and failed to place it.)
	//    - Optional (recommended): fetch the latest core/v1 Event with reason="FailedScheduling"
	//      for each pod; record e.Message to understand cause (Insufficient cpu/memory, taints,
	//      (anti)affinity, topology spread, quota/limits, etc.). Use this later to pick node flavor.
	//
	// 4) (Optional) Ignore pods that won’t be fixed by adding nodes:
	//    - Namespace over ResourceQuota/LimitRange (if Events/conditions indicate quota violation).
	//    - Hard anti-affinity/topology constraints impossible to satisfy with your node templates.
	//    - PDB blocks don’t apply to scheduling, but keep in mind for scale-in.
	//
	// 5) Return the filtered slice of pods. These are your scale-out drivers.
	//    Implementation notes:
	//    - Prefer informers over polling; debounce (e.g., 5–15s) to batch decisions.
	//    - Aggregate container resource *requests* (not limits) from these pods.
	//    - Account for DaemonSet overhead that will run on any new node when planning capacity.

	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("spec.nodeName", ""),
		).String(),
	})
	if err != nil {
		return nil, err
	}

	out := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		if isDaemonSetPod(&pod) {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if isUnschedulable(condition) {
				out = append(out, pod)
				// TODO: Implement logic to not attempt the autoscale if there aren't enough resources
			}
		}

	}

	return out, nil
}

func LastFailedSchedulingEvent(ctx context.Context, cs *kubernetes.Clientset, p *corev1.Pod) (string, error) {
	el, err := cs.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
			fields.OneTermEqualSelector("involvedObject.name", p.Name),
			fields.OneTermEqualSelector("type", "Warning"),
		).String(),
	})
	if err != nil {
		return "", err
	}
	var msg string
	var newest time.Time
	for _, e := range el.Items {
		if e.Reason != "FailedScheduling" {
			continue
		}
		var t time.Time
		if !e.EventTime.IsZero() {
			t = e.EventTime.Time
		} else if !e.LastTimestamp.IsZero() {
			t = e.LastTimestamp.Time
		} else if !e.FirstTimestamp.IsZero() {
			t = e.FirstTimestamp.Time
		}

		if t.After(newest) {
			newest = t
			msg = e.Message
		}
	}
	return msg, nil
}

func GetEvictablePods(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) ([]corev1.Pod, error) {
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return nil, err
	}

	out := make([]corev1.Pod, 0, len(podList.Items))
	for _, p := range podList.Items {
		// Skip mirror pods (static pods)
		if _, ok := p.Annotations["kubernetes.io/config.mirror"]; ok {
			continue
		}
		// Skip DaemonSet pods
		if isDaemonSetPod(&p) {
			continue
		}
		// Skip terminal pods (Succeeded/Failed)
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		// Skip pods that are already being deleted
		if p.DeletionTimestamp != nil {
			continue
		}
		// Skip pods that tolerate the node being unschedulable (like critical pods)
		if toleratesUnschedulable(&p) {
			continue
		}

		out = append(out, p)
	}
	return out, nil
}

func GetRequestedResources(pod *corev1.Pod) (cpuMilli int64, memBytes int64) {
	for _, container := range pod.Spec.Containers {
		if r := container.Resources.Requests; r != nil {
			if cpu := r.Cpu(); cpu != nil {
				cpuMilli += cpu.MilliValue()
			}
			if mem := r.Memory(); mem != nil {
				memBytes += mem.Value()
			}
		}
	}
	return
}

// PoolResourceUsage represents the current resource utilization of a node pool
// All values are in Kubernetes native units (millicores for CPU, bytes for memory)
type PoolResourceUsage struct {
	// Used resources - what pods are actually consuming (from resource requests)
	// Currently same as Allocated, but can be enhanced to track actual usage
	UsedCPUMilli    int64
	UsedMemoryBytes int64

	// Allocated resources - what pods have requested (from resource requests)
	// This includes all pods: user pods, DaemonSets, system pods, etc.
	AllocatedCPUMilli    int64
	AllocatedMemoryBytes int64

	// Available resources - what's available for new pods (allocatable - allocated)
	AvailableCPUMilli    int64
	AvailableMemoryBytes int64

	// Total allocatable capacity - total resources available for pod scheduling
	// This is node.Status.Allocatable (Capacity minus system reservations)
	AllocatableCPUMilli    int64
	AllocatableMemoryBytes int64

	// Total system capacity - total resources on the nodes (for limit checking)
	// This is node.Status.Capacity (total system resources)
	TotalCPUMilli    int64
	TotalMemoryBytes int64

	NodeCount int
}

// GetPoolResourceUsage calculates the current resource utilization of a node pool
// Based on Kubernetes allocatable resources (what's available for pod scheduling)
// and pod resource requests (what pods have requested)
func GetPoolResourceUsage(ctx context.Context, clientset *kubernetes.Clientset, nodepoolKey string, nodepoolValue string) (*PoolResourceUsage, error) {
	nodes, err := GetNodesByLabel(ctx, clientset, nodepoolKey, nodepoolValue)
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes in pool: %w", err)
	}

	var allocatableCPUMilli int64
	var allocatableMemBytes int64
	var totalCPUMilli int64
	var totalMemBytes int64
	var allocatedCPUMilli int64
	var allocatedMemBytes int64

	// Calculate total node capacity and allocated resources
	for _, node := range nodes {
		// Get node allocatable resources (what's actually available for pod scheduling)
		// This is Capacity minus system reservations (kubelet, system daemons, etc.)
		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
			allocatableCPUMilli += cpu.MilliValue()
		}
		if mem := node.Status.Allocatable.Memory(); mem != nil {
			allocatableMemBytes += mem.Value()
		}

		// Get total system capacity (for limit checking)
		if cpu := node.Status.Capacity.Cpu(); cpu != nil {
			totalCPUMilli += cpu.MilliValue()
		}
		if mem := node.Status.Capacity.Memory(); mem != nil {
			totalMemBytes += mem.Value()
		}

		// Get all pods on this node (including DaemonSets, critical pods, etc.)
		pods, err := getAllPodsOnNode(ctx, clientset, node.Name)
		if err != nil {
			// If we can't get pods for a node, skip it but continue with others
			fmt.Printf("Warning: failed to get pods for node %s: %v\n", node.Name, err)
			continue
		}

		// Calculate allocated resources for this node
		for _, pod := range pods {
			cpuMilli, memBytes := GetRequestedResources(&pod)
			allocatedCPUMilli += cpuMilli
			allocatedMemBytes += memBytes
		}
	}

	// Calculate available resources (based on allocatable, not total)
	availableCPUMilli := allocatableCPUMilli - allocatedCPUMilli
	availableMemBytes := allocatableMemBytes - allocatedMemBytes

	// For now, used resources are the same as allocated (we could enhance this later to track actual usage)
	usedCPUMilli := allocatedCPUMilli
	usedMemBytes := allocatedMemBytes

	return &PoolResourceUsage{
		UsedCPUMilli:           usedCPUMilli,
		UsedMemoryBytes:        usedMemBytes,
		AllocatedCPUMilli:      allocatedCPUMilli,
		AllocatedMemoryBytes:   allocatedMemBytes,
		AvailableCPUMilli:      availableCPUMilli,
		AvailableMemoryBytes:   availableMemBytes,
		AllocatableCPUMilli:    allocatableCPUMilli,
		AllocatableMemoryBytes: allocatableMemBytes,
		TotalCPUMilli:          totalCPUMilli,
		TotalMemoryBytes:       totalMemBytes,
		NodeCount:              len(nodes),
	}, nil
}

// getAllPodsOnNode gets all pods running on a specific node that consume resources
func getAllPodsOnNode(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) ([]corev1.Pod, error) {
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return nil, err
	}

	var pods []corev1.Pod
	for _, pod := range podList.Items {
		// Skip mirror pods (static pods) - they don't consume resources
		if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
			continue
		}
		// Skip pods that are already being deleted - they're not consuming resources
		if pod.DeletionTimestamp != nil {
			continue
		}
		// Skip terminal pods (Succeeded/Failed) - they're not consuming resources
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Include all other pods (DaemonSets, regular pods, critical pods, etc.)
		pods = append(pods, pod)
	}

	return pods, nil
}

func GetScaleDownCandiates(ctx context.Context, clientset *kubernetes.Clientset, nodePrefix string, nodepoolKey string, nodepoolValue string) ([]corev1.Node, error) {
	nodes, err := GetNodesByLabel(ctx, clientset, nodepoolKey, nodepoolValue) //clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var candidates []corev1.Node

	for _, node := range nodes {
		// Only collect nodes that are auto-scaled
		if !strings.HasPrefix(node.Name, nodePrefix+"-") {
			continue
		}
		if node.Spec.Unschedulable {
			continue
		}
		evictablePods, err := GetEvictablePods(ctx, clientset, node.Name)
		if err != nil {
			panic(err)
		}

		if len(evictablePods) != 0 {
			continue
		}
		candidates = append(candidates, node)
	}

	return candidates, nil
}

func LabelNode(ctx context.Context, clientset *kubernetes.Clientset, nodeName string, labels map[string]string) error {
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	for key, value := range labels {
		node.Labels[key] = value
	}

	_, err = clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

func WaitForNodeReady(ctx context.Context, clientset *kubernetes.Clientset, nodeName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for node %s to be ready", nodeName)
		case <-ticker.C:
			node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				// If node is not found, continue waiting - it might not be registered yet
				if errors.IsNotFound(err) {
					fmt.Printf("Node %s not found yet, continuing to wait...\n", nodeName)
					continue
				}
				// For other errors, return immediately
				return fmt.Errorf("error getting node %s: %v", nodeName, err)
			}

			// Check if node is ready
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					fmt.Printf("Node %s is now ready!\n", nodeName)
					return nil
				}
			}

			fmt.Printf("Node %s exists but not ready yet, continuing to wait...\n", nodeName)
		}
	}
}
