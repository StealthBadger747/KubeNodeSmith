package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
	"github.com/StealthBadger747/KubeNodeSmith/internal/kube"
)

const (
	FinalizerNodeSmithPool = "kubenodesmith.io/nodepool"
)

type nodeCapacity struct {
	cpuMilli int64
	memBytes int64
}

type capacityBucket struct {
	remainingCPU int64
	remainingMem int64
}

type claimResources struct {
	cpuCores  int64
	memoryMiB int64
}

type podDemand struct {
	pod      corev1.Pod
	cpuMilli int64
	memBytes int64
}

// NodePoolReconciler reconciles a NodeSmithPool object.
type NodePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("nodesmithpool", req.NamespacedName)

	// High-level reconciliation outline:
	// 1. Fetch the NodeSmithPool; return gracefully if it no longer exists.
	// 2. Handle deletion: if deletion timestamp is set, finalize the pool (delete all claims).
	// 3. Ensure finalizer is present on the pool.
	// 4. Calculate cluster state:
	//    - List unschedulable pods to determine demand.
	//    - List existing nodes in this pool to determine current capacity.
	// 5. Scale Up:
	//    - If there are unschedulable pods, calculate how many new nodes are needed.
	//    - Create new NodeSmithClaim resources to request new machines.
	//    - (The NodeClaimReconciler will handle the actual provisioning).
	// 6. Scale Down:
	//    - If no unschedulable pods and node count > minNodes, identify candidates for removal.
	//    - Delete NodeSmithClaim resources for nodes to be removed.
	//    - (The NodeClaimReconciler will handle cordon/drain and deprovisioning).

	var nodePool kubenodesmithv1alpha1.NodeSmithPool
	if err := r.Get(ctx, req.NamespacedName, &nodePool); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate requeue
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion - ensure all claims are cleaned up before removing pool
	if !nodePool.DeletionTimestamp.IsZero() {
		return r.finalizePool(ctx, &nodePool)
	}

	// Add finalizer if missing
	if !controllerutil.ContainsFinalizer(&nodePool, FinalizerNodeSmithPool) {
		logger.Info("adding finalizer to pool")
		controllerutil.AddFinalizer(&nodePool, FinalizerNodeSmithPool)
		if err := r.Update(ctx, &nodePool); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	unschedulablePods, err := kube.GetUnschedulablePods(ctx, r.Client)
	if err != nil {
		logger.Error(err, "list unschedulable pods")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	nodesInPool, err := kube.GetNodesByLabel(ctx, r.Client, nodePool.Spec.PoolLabelKey, nodePool.Name)
	if err != nil {
		logger.Error(err, "failed to list nodes in pool", "pool", nodePool.Name)
	}

	if len(unschedulablePods) != 0 {
		// Scale up to accommodate unschedulable pods
		result, err := r.reconcileScaleUp(ctx, &nodePool, unschedulablePods, nodesInPool)
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}
	} else if len(nodesInPool) > nodePool.Spec.Limits.MinNodes {
		// No unschedulable pods and we're above min nodes - consider scale down
		result, err := r.reconcileScaleDown(ctx, &nodePool, nodesInPool)
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}
	}

	return ctrl.Result{}, nil
}

// reconcileScaleDown handles scaling down the node pool to remove underutilized nodes.
func (r *NodePoolReconciler) reconcileScaleDown(
	ctx context.Context,
	nodePool *kubenodesmithv1alpha1.NodeSmithPool,
	nodesInPool []corev1.Node,
) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("pool", nodePool.Name)

	if nodePool.Spec.Limits.MinNodes > 0 && len(nodesInPool) <= nodePool.Spec.Limits.MinNodes {
		logger.V(1).Info("node pool at or below min size; skipping scale down",
			"minNodes", nodePool.Spec.Limits.MinNodes,
			"currentNodes", len(nodesInPool),
		)
		return ctrl.Result{}, nil
	}

	nodes, err := kube.GetScaleDownCandidates(ctx, r.Client, nodePool)
	if err != nil {
		logger.Error(err, "get scale down candidates")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if len(nodes) != 0 {
		// TODO: detect pool-labeled nodes that lost their claims (e.g. host crash) and recycle them.
		// Only enforce stabilization window when we have actual candidates to remove
		if nodePool.Spec.ScaleDown != nil && nodePool.Spec.ScaleDown.StabilizationWindow != nil {
			if nodePool.Status.LastScaleActivity != nil {
				timeSinceLastScale := time.Since(nodePool.Status.LastScaleActivity.Time)
				if timeSinceLastScale < nodePool.Spec.ScaleDown.StabilizationWindow.Duration {
					remaining := nodePool.Spec.ScaleDown.StabilizationWindow.Duration - timeSinceLastScale
					logger.V(1).Info("within scale-down stabilization window, deferring",
						"timeSinceLastScale", timeSinceLastScale,
						"remaining", remaining,
					)
					return ctrl.Result{RequeueAfter: remaining}, nil
				}
			}
		}

		for _, candidateNode := range nodes {
			logger.Info("scaling down node", "node", candidateNode.Name)

			var claims kubenodesmithv1alpha1.NodeSmithClaimList
			if err := r.List(ctx, &claims, client.InNamespace(nodePool.Namespace)); err != nil {
				logger.Error(err, "list NodeSmithClaims for pool during scale down")
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}

			var targetClaim *kubenodesmithv1alpha1.NodeSmithClaim
			for _, claim := range claims.Items {
				if claim.Spec.PoolRef != nodePool.Name {
					continue
				}
				if claim.Status.NodeName == candidateNode.Name {
					targetClaim = &claim
					break
				}
			}

			if targetClaim == nil {
				logger.Info("no claim found for node, cannot scale down", "nodeName", candidateNode.Name)
				continue
			}

			logger.Info("deleting claim for scale down", "claim", targetClaim.Name, "nodeName", candidateNode.Name)
			if err := r.Delete(ctx, targetClaim); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Info("claim already deleted")
					return ctrl.Result{}, nil
				}
				logger.Error(err, "failed to delete claim for scale down")
				return ctrl.Result{RequeueAfter: time.Minute}, err
			}

			now := metav1.Now()
			scaleDownMessage := fmt.Sprintf("Deleted claim %s for node %s", targetClaim.Name, candidateNode.Name)
			if err := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
				p.Status.LastScaleActivity = &now
				p.Status.ObservedGeneration = p.Generation
				meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
					Type:               "Progressing",
					Status:             metav1.ConditionTrue,
					Reason:             "ScalingDown",
					Message:            scaleDownMessage,
					ObservedGeneration: p.Generation,
				})
			}); err != nil {
				logger.Error(err, "failed to update pool status after scale down")
				return ctrl.Result{}, err
			}

			r.Recorder.Eventf(nodePool, corev1.EventTypeNormal, "ScaledDown",
				"Removing node %s from pool", candidateNode.Name)

			logger.Info("successfully initiated scale down", "nodeName", candidateNode.Name)
			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{}, nil
}

// reconcileScaleUp handles scaling up the node pool to accommodate unschedulable pods.
func (r *NodePoolReconciler) reconcileScaleUp(
	ctx context.Context,
	nodePool *kubenodesmithv1alpha1.NodeSmithPool,
	unschedulablePods []corev1.Pod,
	nodesInPool []corev1.Node,
) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("pool", nodePool.Name)

	candidatePods := filterPodsForPool(unschedulablePods, nodePool)
	if len(candidatePods) == 0 {
		logger.V(1).Info("no unschedulable pods targeting pool; skipping scale up")
		return ctrl.Result{}, nil
	}

	var claims kubenodesmithv1alpha1.NodeSmithClaimList
	if err := r.List(ctx, &claims, client.InNamespace(nodePool.Namespace)); err != nil {
		logger.Error(err, "list NodeSmithClaims for pool")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	nodeTemplate := determineNodeCapacity(nodesInPool, &claims, candidatePods)
	if nodeTemplate.cpuMilli == 0 || nodeTemplate.memBytes == 0 {
		logger.Info("unable to determine node capacity for pool; skipping scale up")
		return ctrl.Result{}, nil
	}

	nodeBuckets, err := buildNodeBuckets(ctx, r.Client, nodesInPool)
	if err != nil {
		logger.Error(err, "failed to compute node capacity")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	pendingBuckets := buildPendingClaimBuckets(&claims, nodePool.Name)
	buckets := append(nodeBuckets, pendingBuckets...)

	newClaimSpecs := planCapacity(candidatePods, buckets, nodeTemplate)
	if len(newClaimSpecs) == 0 {
		logger.V(1).Info("existing capacity sufficient; skipping scale up")
		return ctrl.Result{}, nil
	}

	pendingCount, pendingCPUMilli, pendingMemBytes, err := countInflightClaims(nodePool, &claims)
	if err != nil {
		logger.Error(err, "count inflight claims")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	poolUsage, err := kube.GetPoolResourceUsage(ctx, r.Client, nodePool)
	if err != nil {
		logger.Error(err, "get pool resource usage")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	currentNodes := len(nodesInPool)
	if nodePool.Spec.Limits.MaxNodes > 0 && currentNodes+pendingCount+len(newClaimSpecs) > nodePool.Spec.Limits.MaxNodes {
		msg := fmt.Sprintf("Pool at max capacity: %d/%d nodes (including %d pending)", currentNodes+pendingCount, nodePool.Spec.Limits.MaxNodes, pendingCount)
		logger.Info("node pool at or above max size; skipping scale up",
			"maxNodes", nodePool.Spec.Limits.MaxNodes,
			"currentNodes", currentNodes,
			"pendingClaims", pendingCount,
		)
		r.Recorder.Eventf(nodePool, corev1.EventTypeWarning, "ScaleUpBlocked", msg)
		if updateErr := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type:               "Available",
				Status:             metav1.ConditionFalse,
				Reason:             "MaxNodesReached",
				Message:            msg,
				ObservedGeneration: p.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status for max nodes condition")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	var newCPUMilli, newMemBytes int64
	for _, spec := range newClaimSpecs {
		newCPUMilli += spec.cpuCores * 1000
		newMemBytes += spec.memoryMiB * 1024 * 1024
	}

	if exceeded, reason := exceedsPoolLimits(poolUsage, &nodePool.Spec.Limits, pendingCPUMilli, pendingMemBytes, newCPUMilli, newMemBytes); exceeded {
		logger.Info("skipping scale up due to resource limits", "reason", reason)
		r.Recorder.Eventf(nodePool, corev1.EventTypeWarning, "ScaleUpBlocked", reason)
		if updateErr := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type:               "Available",
				Status:             metav1.ConditionFalse,
				Reason:             "ResourceLimitReached",
				Message:            reason,
				ObservedGeneration: p.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status for resource limit condition")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	nextSeq := nodePool.Status.NextClaimSequence
	if nextSeq <= 0 {
		nextSeq = 1
	}
	created := 0
	for _, spec := range newClaimSpecs {
		claimName := fmt.Sprintf("%s-%06d", nodePool.Name, nextSeq)
		nextSeq++
		if err := r.ensureClaim(ctx, nodePool, claimName, spec); err != nil {
			logger.Error(err, "failed to create NodeSmithClaim", "claim", claimName)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
		created++
	}

	if created == 0 {
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	if err := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
		p.Status.LastScaleActivity = &now
		p.Status.ObservedGeneration = p.Generation
		p.Status.NextClaimSequence = nextSeq
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			Reason:             "ScalingUp",
			Message:            fmt.Sprintf("Created %d new claims for %d pending pods", created, len(candidatePods)),
			ObservedGeneration: p.Generation,
		})
	}); err != nil {
		logger.Error(err, "failed to update pool status")
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(nodePool, corev1.EventTypeNormal, "ScaledUp",
		"Created %d NodeSmithClaims to accommodate pending pods", created)

	return ctrl.Result{}, nil
}

func (r *NodePoolReconciler) ensureClaim(
	ctx context.Context,
	nodePool *kubenodesmithv1alpha1.NodeSmithPool,
	claimName string,
	resources claimResources,
) error {
	key := types.NamespacedName{Namespace: nodePool.Namespace, Name: claimName}
	var existing kubenodesmithv1alpha1.NodeSmithClaim
	if err := r.Get(ctx, key, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get existing claim %s: %w", claimName, err)
	}

	labels := map[string]string{}
	if nodePool.Spec.PoolLabelKey != "" {
		labels[nodePool.Spec.PoolLabelKey] = nodePool.Name
	}
	for k, v := range nodePool.Spec.MachineTemplate.Labels {
		labels[k] = v
	}

	claim := &kubenodesmithv1alpha1.NodeSmithClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: nodePool.Namespace,
			Labels:    labels,
		},
		Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
			PoolRef: nodePool.Name,
			Requirements: &kubenodesmithv1alpha1.NodeSmithClaimRequirements{
				CPUCores:  resources.cpuCores,
				MemoryMiB: resources.memoryMiB,
			},
			IdempotencyKey: string(uuid.NewUUID()),
		},
	}

	if err := controllerutil.SetControllerReference(nodePool, claim, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create claim %s: %w", claimName, err)
	}

	logf.FromContext(ctx).WithValues("pool", nodePool.Name).Info("created NodeSmithClaim", "claim", claimName)
	return nil
}

func filterPodsForPool(pods []corev1.Pod, pool *kubenodesmithv1alpha1.NodeSmithPool) []corev1.Pod {
	labels := buildPoolLabelSet(pool)
	results := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		matches, requires := podMatchesPool(pod, pool, labels)
		if matches && requires {
			results = append(results, *pod)
		}
	}
	return results
}

func buildPoolLabelSet(pool *kubenodesmithv1alpha1.NodeSmithPool) map[string]string {
	labels := map[string]string{}
	if pool.Spec.PoolLabelKey != "" {
		labels[pool.Spec.PoolLabelKey] = pool.Name
	}
	for k, v := range pool.Spec.MachineTemplate.Labels {
		labels[k] = v
	}
	return labels
}

func podMatchesPool(pod *corev1.Pod, pool *kubenodesmithv1alpha1.NodeSmithPool, poolLabels map[string]string) (bool, bool) {
	requiresPool := false
	for key, val := range pod.Spec.NodeSelector {
		labelVal, ok := poolLabels[key]
		if !ok || labelVal != val {
			return false, false
		}
		if key == pool.Spec.PoolLabelKey || pool.Spec.MachineTemplate.Labels[key] != "" {
			requiresPool = true
		}
	}

	required := pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil && pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil
	if required {
		terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) > 0 {
			termMatches := false
			termRequires := false
			for _, term := range terms {
				ok, req := nodeSelectorTermMatches(term, pool, poolLabels)
				if ok {
					termMatches = true
					if req {
						termRequires = true
					}
					break
				}
			}
			if !termMatches {
				return false, false
			}
			requiresPool = requiresPool || termRequires
		}
	}

	return true, requiresPool
}

func nodeSelectorTermMatches(term corev1.NodeSelectorTerm, pool *kubenodesmithv1alpha1.NodeSmithPool, labels map[string]string) (bool, bool) {
	requiresPool := false
	for _, expr := range term.MatchExpressions {
		value, hasLabel := labels[expr.Key]
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			if !hasLabel || !containsString(expr.Values, value) {
				return false, false
			}
		case corev1.NodeSelectorOpNotIn:
			if hasLabel && containsString(expr.Values, value) {
				return false, false
			}
		case corev1.NodeSelectorOpExists:
			if !hasLabel {
				return false, false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			if hasLabel {
				return false, false
			}
		case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
			if !hasLabel {
				return false, false
			}
			labelVal, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return false, false
			}
			reqVal, err := strconv.ParseInt(expr.Values[0], 10, 64)
			if err != nil {
				return false, false
			}
			if expr.Operator == corev1.NodeSelectorOpGt && !(labelVal > reqVal) {
				return false, false
			}
			if expr.Operator == corev1.NodeSelectorOpLt && !(labelVal < reqVal) {
				return false, false
			}
		}
		if expr.Key == pool.Spec.PoolLabelKey || pool.Spec.MachineTemplate.Labels[expr.Key] != "" {
			requiresPool = true
		}
	}
	return true, requiresPool
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func determineNodeCapacity(nodes []corev1.Node, claims *kubenodesmithv1alpha1.NodeSmithClaimList, pods []corev1.Pod) nodeCapacity {
	if len(nodes) > 0 {
		node := nodes[0]
		cpu := int64(0)
		mem := int64(0)
		if v := node.Status.Allocatable.Cpu(); v != nil {
			cpu = v.MilliValue()
		}
		if v := node.Status.Allocatable.Memory(); v != nil {
			mem = v.Value()
		}
		return nodeCapacity{cpuMilli: cpu, memBytes: mem}
	}
	for i := range claims.Items {
		claim := claims.Items[i]
		if claim.Spec.Requirements == nil {
			continue
		}
		if claim.Spec.Requirements.CPUCores > 0 && claim.Spec.Requirements.MemoryMiB > 0 {
			cpu := claim.Spec.Requirements.CPUCores * 1000
			mem := claim.Spec.Requirements.MemoryMiB * 1024 * 1024
			return nodeCapacity{cpuMilli: cpu, memBytes: mem}
		}
	}
	var fallback nodeCapacity
	for i := range pods {
		cpu, mem := kube.GetRequestedResources(&pods[i])
		if cpu > fallback.cpuMilli {
			fallback.cpuMilli = cpu
		}
		if mem > fallback.memBytes {
			fallback.memBytes = mem
		}
	}
	return fallback
}

func buildNodeBuckets(ctx context.Context, c client.Client, nodes []corev1.Node) ([]capacityBucket, error) {
	buckets := make([]capacityBucket, 0, len(nodes))
	for _, node := range nodes {
		allocCPU := int64(0)
		allocMem := int64(0)
		if v := node.Status.Allocatable.Cpu(); v != nil {
			allocCPU = v.MilliValue()
		}
		if v := node.Status.Allocatable.Memory(); v != nil {
			allocMem = v.Value()
		}
		pods, err := listPodsOnNode(ctx, c, node.Name)
		if err != nil {
			return nil, err
		}
		usedCPU := int64(0)
		usedMem := int64(0)
		for i := range pods {
			cpu, mem := kube.GetRequestedResources(&pods[i])
			usedCPU += cpu
			usedMem += mem
		}
		remainingCPU := allocCPU - usedCPU
		if remainingCPU < 0 {
			remainingCPU = 0
		}
		remainingMem := allocMem - usedMem
		if remainingMem < 0 {
			remainingMem = 0
		}
		buckets = append(buckets, capacityBucket{remainingCPU: remainingCPU, remainingMem: remainingMem})
	}
	return buckets, nil
}

func buildPendingClaimBuckets(claims *kubenodesmithv1alpha1.NodeSmithClaimList, poolName string) []capacityBucket {
	buckets := []capacityBucket{}
	for i := range claims.Items {
		claim := claims.Items[i]
		if claim.Spec.PoolRef != poolName {
			continue
		}
		readyCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			continue
		}
		if claim.Spec.Requirements == nil {
			continue
		}
		cpu := claim.Spec.Requirements.CPUCores * 1000
		mem := claim.Spec.Requirements.MemoryMiB * 1024 * 1024
		buckets = append(buckets, capacityBucket{remainingCPU: cpu, remainingMem: mem})
	}
	return buckets
}

func planCapacity(pods []corev1.Pod, buckets []capacityBucket, template nodeCapacity) []claimResources {
	demands := make([]podDemand, 0, len(pods))
	for i := range pods {
		cpu, mem := kube.GetRequestedResources(&pods[i])
		if cpu == 0 && mem == 0 {
			continue
		}
		demands = append(demands, podDemand{pod: pods[i], cpuMilli: cpu, memBytes: mem})
	}
	sort.SliceStable(demands, func(i, j int) bool {
		return demands[i].cpuMilli > demands[j].cpuMilli
	})
	newClaims := make([]claimResources, 0)
	for _, demand := range demands {
		placed := false
		for i := range buckets {
			if buckets[i].remainingCPU >= demand.cpuMilli && buckets[i].remainingMem >= demand.memBytes {
				buckets[i].remainingCPU -= demand.cpuMilli
				buckets[i].remainingMem -= demand.memBytes
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		capacityCPU := template.cpuMilli
		if capacityCPU < demand.cpuMilli {
			capacityCPU = demand.cpuMilli
		}
		capacityMem := template.memBytes
		if capacityMem < demand.memBytes {
			capacityMem = demand.memBytes
		}
		buckets = append(buckets, capacityBucket{
			remainingCPU: capacityCPU - demand.cpuMilli,
			remainingMem: capacityMem - demand.memBytes,
		})
		newClaims = append(newClaims, claimResources{
			cpuCores:  int64(math.Ceil(float64(capacityCPU) / 1000.0)),
			memoryMiB: int64(math.Ceil(float64(capacityMem) / (1024 * 1024))),
		})
	}
	return newClaims
}

func listPodsOnNode(ctx context.Context, c client.Client, nodeName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Fallback: list all and filter
		if err := c.List(ctx, &podList); err != nil {
			return nil, err
		}
	}
	result := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		result = append(result, pod)
	}
	return result, nil
}

func (r *NodePoolReconciler) updateStatus(
	ctx context.Context,
	pool *kubenodesmithv1alpha1.NodeSmithPool,
	mutate func(*kubenodesmithv1alpha1.NodeSmithPool),
) error {
	key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest kubenodesmithv1alpha1.NodeSmithPool
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		original := latest.Status.DeepCopy()
		if original == nil {
			original = &kubenodesmithv1alpha1.NodeSmithPoolStatus{}
		}
		mutate(&latest)
		if apiequality.Semantic.DeepEqual(original, &latest.Status) {
			pool.Status = latest.Status
			return nil
		}
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		pool.Status = latest.Status
		return nil
	})
}

// exceedsPoolLimits verifies if adding a new node with the specified resources would exceed pool limits
// Returns true if limits would be exceeded, false otherwise
func exceedsPoolLimits(
	poolUsage *kube.PoolResourceUsage,
	limits *kubenodesmithv1alpha1.NodePoolLimits,
	pendingCPUMilli int64,
	pendingMemBytes int64,
	newNodeCPUMilli int64,
	newNodeMemBytes int64,
) (bool, string) {
	// Convert limits to millicores/bytes
	limitCPUMilli := limits.CPUCores * 1000
	limitMemBytes := limits.MemoryMiB * 1024 * 1024
	additionalCPUMilli := pendingCPUMilli + newNodeCPUMilli
	additionalMemBytes := pendingMemBytes + newNodeMemBytes

	// Check CPU limits
	if limits.CPUCores > 0 && (poolUsage.TotalCPUMilli+additionalCPUMilli) > limitCPUMilli {
		return true, fmt.Sprintf(
			"adding pending+new capacity of ~%d CPU millicores would exceed pool limit of %d (current total: %d)",
			additionalCPUMilli, limitCPUMilli, poolUsage.TotalCPUMilli,
		)
	}

	// Check memory limits
	if limits.MemoryMiB > 0 && (poolUsage.TotalMemoryBytes+additionalMemBytes) > limitMemBytes {
		return true, fmt.Sprintf(
			"adding pending+new capacity of ~%d bytes memory would exceed pool limit of %d (current total: %d)",
			additionalMemBytes, limitMemBytes, poolUsage.TotalMemoryBytes,
		)
	}

	return false, ""
}

// finalizePool handles cleanup when a pool is deleted.
// Deletes all associated claims (which triggers machine cleanup via claim finalizers).
func (r *NodePoolReconciler) finalizePool(ctx context.Context, nodePool *kubenodesmithv1alpha1.NodeSmithPool) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("pool", nodePool.Name)

	if !controllerutil.ContainsFinalizer(nodePool, FinalizerNodeSmithPool) {
		return ctrl.Result{}, nil // Already finalized
	}

	logger.Info("finalizing pool")

	// List all claims owned by this pool
	var claims kubenodesmithv1alpha1.NodeSmithClaimList
	if err := r.List(ctx, &claims, client.InNamespace(nodePool.Namespace)); err != nil {
		logger.Error(err, "failed to list claims during finalization")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Delete all claims for this pool
	claimsDeleted := 0
	claimsRemaining := 0
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.PoolRef != nodePool.Name {
			continue // Not our claim
		}

		if claim.DeletionTimestamp.IsZero() {
			// Delete the claim
			logger.Info("deleting claim", "claim", claim.Name)
			if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete claim", "claim", claim.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
			claimsDeleted++
		} else {
			// Claim is already being deleted, wait for it
			logger.V(1).Info("waiting for claim to be deleted", "claim", claim.Name)
			claimsRemaining++
		}
	}

	// Wait for all claims to be deleted
	if claimsRemaining > 0 {
		logger.Info("waiting for claims to be deleted", "remaining", claimsRemaining)
		r.Recorder.Eventf(nodePool, corev1.EventTypeNormal, "Finalizing",
			"Waiting for %d claims to be deleted", claimsRemaining)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// All claims deleted, remove finalizer
	logger.Info("removing finalizer from pool", "claimsDeleted", claimsDeleted)
	controllerutil.RemoveFinalizer(nodePool, FinalizerNodeSmithPool)
	if err := r.Update(ctx, nodePool); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(nodePool, corev1.EventTypeNormal, "Finalized", "Pool finalized, deleted %d claims", claimsDeleted)
	logger.Info("pool finalized successfully")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubenodesmithv1alpha1.NodeSmithPool{}).
		Owns(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		Named("nodepool").
		Complete(r)
}
