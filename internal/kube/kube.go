package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
)

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

func GetNodesByLabel(ctx context.Context, c client.Client, labelKey string, labelValue string) ([]corev1.Node, error) {
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList, client.MatchingLabels{labelKey: labelValue}); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func CordonNode(ctx context.Context, c client.Client, nodeName string) error {
	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	return c.Patch(ctx, &node, patch)
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

func GetUnschedulablePods(ctx context.Context, c client.Client) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList); err != nil {
		return nil, err
	}

	out := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			continue
		}
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		if isDaemonSetPod(&pod) {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if isUnschedulable(condition) {
				out = append(out, pod)
				break
			}
		}
	}

	return out, nil
}

func LastFailedSchedulingEvent(ctx context.Context, c client.Client, p *corev1.Pod) (string, error) {
	var eventList corev1.EventList
	if err := c.List(ctx, &eventList, client.InNamespace(p.Namespace), client.MatchingFields{
		"involvedObject.kind": "Pod",
		"involvedObject.name": p.Name,
		"type":                "Warning",
	}); err != nil {
		return "", err
	}

	var msg string
	var newest time.Time
	for _, e := range eventList.Items {
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

func GetEvictablePods(ctx context.Context, c client.Client, nodeName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Fallback: list all and filter
		if err := c.List(ctx, &podList); err != nil {
			return nil, err
		}
	}

	out := make([]corev1.Pod, 0, len(podList.Items))
	for _, p := range podList.Items {
		if p.Spec.NodeName != nodeName {
			continue
		}
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
func GetPoolResourceUsage(ctx context.Context, c client.Client, nodePool *kubenodesmithv1alpha1.NodeSmithPool) (*PoolResourceUsage, error) {
	nodepoolKey := nodePool.Spec.PoolLabelKey
	nodepoolValue := nodePool.Name

	nodes, err := GetNodesByLabel(ctx, c, nodepoolKey, nodepoolValue)
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes in pool: %w", err)
	}

	var allPods corev1.PodList
	if err := c.List(ctx, &allPods); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	podsByNode := make(map[string][]corev1.Pod)
	for _, pod := range allPods.Items {
		if pod.Spec.NodeName != "" {
			podsByNode[pod.Spec.NodeName] = append(podsByNode[pod.Spec.NodeName], pod)
		}
	}

	var allocatableCPUMilli int64
	var allocatableMemBytes int64
	var totalCPUMilli int64
	var totalMemBytes int64
	var allocatedCPUMilli int64
	var allocatedMemBytes int64

	for _, node := range nodes {
		// Node capacity/allocatable
		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
			allocatableCPUMilli += cpu.MilliValue()
		}
		if mem := node.Status.Allocatable.Memory(); mem != nil {
			allocatableMemBytes += mem.Value()
		}
		if cpu := node.Status.Capacity.Cpu(); cpu != nil {
			totalCPUMilli += cpu.MilliValue()
		}
		if mem := node.Status.Capacity.Memory(); mem != nil {
			totalMemBytes += mem.Value()
		}

		// Pod usage for this node (lookup from map)
		if nodePods, ok := podsByNode[node.Name]; ok {
			for _, pod := range nodePods {
				// Skip mirror pods, terminal pods, deleted pods
				if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
					continue
				}
				if pod.DeletionTimestamp != nil {
					continue
				}
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
					continue
				}

				cpuMilli, memBytes := GetRequestedResources(&pod)
				allocatedCPUMilli += cpuMilli
				allocatedMemBytes += memBytes
			}
		}
	}

	availableCPUMilli := allocatableCPUMilli - allocatedCPUMilli
	availableMemBytes := allocatableMemBytes - allocatedMemBytes

	return &PoolResourceUsage{
		UsedCPUMilli:           allocatedCPUMilli,
		UsedMemoryBytes:        allocatedMemBytes,
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

func GetScaleDownCandidates(ctx context.Context, c client.Client, nodePool *kubenodesmithv1alpha1.NodeSmithPool) ([]corev1.Node, error) {
	nodepoolKey := nodePool.Spec.PoolLabelKey
	nodepoolValue := nodePool.Name

	nodes, err := GetNodesByLabel(ctx, c, nodepoolKey, nodepoolValue)
	if err != nil {
		return nil, err
	}

	candidates := make([]corev1.Node, 0, len(nodes))

	for _, node := range nodes {
		if node.Spec.Unschedulable {
			continue
		}
		evictablePods, err := GetEvictablePods(ctx, c, node.Name)
		if err != nil {
			return nil, fmt.Errorf("get evictable pods for node %s: %w", node.Name, err)
		}

		if len(evictablePods) != 0 {
			continue
		}
		candidates = append(candidates, node)
	}

	return candidates, nil
}

func LabelNode(ctx context.Context, c client.Client, nodeName string, labels map[string]string) error {
	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}

	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	for key, value := range labels {
		node.Labels[key] = value
	}

	return c.Patch(ctx, &node, patch)
}

func WaitForNodeReady(ctx context.Context, c client.Client, nodeName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for node %s to be ready", nodeName)
		case <-ticker.C:
			var node corev1.Node
			if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("error getting node %s: %w", nodeName, err)
			}

			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
	}
}
