package controller

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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
	var cs = kubernetes.NewForConfigOrDie(ctrl.GetConfigOrDie())

	// High-level reconciliation outline:
	// 1. Fetch the NodeSmithPool; return gracefully if it no longer exists.
	// 2. Capture a logger scoped to the pool and stash a deep copy of the original status.
	// 3. If deletion timestamp is set, ensure finalizers run: drain provider-owned machines,
	//    delete or release NodeSmithClaims, update status, then remove the finalizer.
	// 4. Validate spec invariants (providerRef present, limits sane, scale policies valid) and
	//    surface configuration errors via status conditions.
	// 5. Resolve the referenced NodeSmithProvider, initializing the concrete provider client and
	//    failing early if credentials/options are missing.
	// 6. List NodeSmithClaims tied to this pool and correlate them with provider machines and
	//    registered Kubernetes nodes to understand actual capacity.
	// 7. Determine desired capacity using min/max limits, outstanding claims, and scale-up/down
	//    policies (batch size, stabilization windows, drain concurrency, etc.).
	// 8. Handle scale-up by creating new NodeSmithClaims and, when ready, issuing ProvisionMachine
	//    calls with appropriate MachineSpec derived from the pool template and limits.
	// 9. Handle scale-down by picking surplus machines, coordinating node cordon/drain, updating
	//    the corresponding claims, and calling DeprovisionMachine respecting drain timeouts.
	// 10. Ensure machines/nodes carry the pool label key, template labels, taints, and other
	//     desired metadata; detect drift and record conditions/events.
	// 11. Update status (ObservedGeneration, Conditions, LastScaleActivity, counts) and emit
	//     events; decide whether to requeue immediately or after backoff based on ongoing work.

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

	unschedulablePods, err := kube.GetUnschedulablePods(ctx, cs)
	if err != nil {
		logger.Error(err, "list unschedulable pods")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	nodesInPool, err := kube.GetNodesByLabel(ctx, cs, nodePool.Spec.PoolLabelKey, nodePool.Name)
	if err != nil {
		logger.Error(err, "failed to list nodes in pool", "pool", nodePool.Name)
	}

	if len(unschedulablePods) != 0 {
		// Scale up to accommodate unschedulable pods
		result, err := r.reconcileScaleUp(ctx, &nodePool, cs, unschedulablePods, nodesInPool)
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}
	} else if len(nodesInPool) > nodePool.Spec.Limits.MinNodes {
		// No unschedulable pods and we're above min nodes - consider scale down
		result, err := r.reconcileScaleDown(ctx, &nodePool, cs, nodesInPool)
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
	cs *kubernetes.Clientset,
	nodesInPool []corev1.Node,
) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("pool", nodePool.Name)

	// Check stabilization window if configured
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

	if nodePool.Spec.Limits.MinNodes > 0 && len(nodesInPool) <= nodePool.Spec.Limits.MinNodes {
		logger.V(1).Info("node pool at or below min size; skipping scale down",
			"minNodes", nodePool.Spec.Limits.MinNodes,
			"currentNodes", len(nodesInPool),
		)
		return ctrl.Result{}, nil
	}

	nodes, err := kube.GetScaleDownCandidates(ctx, cs, nodePool)
	if err != nil {
		logger.Error(err, "get scale down candidates")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if len(nodes) != 0 {
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
	cs *kubernetes.Clientset,
	unschedulablePods []corev1.Pod,
	nodesInPool []corev1.Node,
) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("pool", nodePool.Name)

	// List all claims for this pool
	var claims kubenodesmithv1alpha1.NodeSmithClaimList
	if err := r.List(ctx, &claims,
		client.InNamespace(nodePool.Namespace),
	); err != nil {
		logger.Error(err, "list NodeSmithClaims for pool")
		message := fmt.Sprintf("Failed to list claims: %v", err)
		if updateErr := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type:               "Available",
				Status:             metav1.ConditionUnknown,
				Reason:             "ClaimListFailed",
				Message:            message,
				ObservedGeneration: p.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status after claim list failure")
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Count inflight (pending) claims
	pendingCount, pendingCPUMilli, pendingMemBytes, err := countInflightClaims(nodePool, &claims)
	if err != nil {
		logger.Error(err, "count inflight claims")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	nodePoolSize := len(nodesInPool) + pendingCount

	// Check max node limit
	if nodePool.Spec.Limits.MaxNodes > 0 && nodePoolSize >= nodePool.Spec.Limits.MaxNodes {
		logger.Info("node pool at or above max size; skipping scale up",
			"maxNodes", nodePool.Spec.Limits.MaxNodes,
			"currentNodes", len(nodesInPool),
			"pendingClaims", pendingCount,
		)
		r.Recorder.Eventf(nodePool, corev1.EventTypeWarning, "ScaleUpBlocked",
			"Pool at max capacity: %d/%d nodes (including %d pending)",
			nodePoolSize, nodePool.Spec.Limits.MaxNodes, pendingCount)

		if updateErr := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type:               "Available",
				Status:             metav1.ConditionFalse,
				Reason:             "MaxNodesReached",
				Message:            fmt.Sprintf("Pool at maximum capacity: %d nodes", nodePoolSize),
				ObservedGeneration: p.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status for max nodes condition")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check stabilization window if configured
	if nodePool.Spec.ScaleUp != nil && nodePool.Spec.ScaleUp.StabilizationWindow != nil {
		if nodePool.Status.LastScaleActivity != nil {
			timeSinceLastScale := time.Since(nodePool.Status.LastScaleActivity.Time)
			if timeSinceLastScale < nodePool.Spec.ScaleUp.StabilizationWindow.Duration {
				remaining := nodePool.Spec.ScaleUp.StabilizationWindow.Duration - timeSinceLastScale
				logger.V(1).Info("within stabilization window, deferring scale up",
					"timeSinceLastScale", timeSinceLastScale,
					"remaining", remaining,
				)
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		}
	}

	// Pick the first unschedulable pod to provision for
	pod := unschedulablePods[0]
	cpuMilli, memBytes := kube.GetRequestedResources(&pod)
	cpu := int64(math.Ceil(float64(cpuMilli) / 1000))
	memMiB := memBytes / (1024 * 1024)

	logger.Info("found unschedulable pod requiring capacity",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"cpuCores", cpu,
		"memoryMiB", memMiB,
	)

	// Get current pool resource usage
	poolUsage, err := kube.GetPoolResourceUsage(ctx, cs, nodePool)
	if err != nil {
		logger.Error(err, "get pool resource usage")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	logger.V(1).Info("pool resource snapshot",
		"nodes", poolUsage.NodeCount,
		"allocatableCPUMilli", poolUsage.AllocatableCPUMilli,
		"allocatableMemoryBytes", poolUsage.AllocatableMemoryBytes,
		"availableCPUMilli", poolUsage.AvailableCPUMilli,
		"availableMemoryBytes", poolUsage.AvailableMemoryBytes,
	)

	// Check if adding a new node would exceed pool resource limits
	if exceeded, reason := exceedsPoolLimits(poolUsage, &nodePool.Spec.Limits, pendingCPUMilli, pendingMemBytes, cpuMilli, memBytes); exceeded {
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

	// Create NodeSmithClaim
	if err := r.createClaim(ctx, nodePool, pod, cpu, memMiB); err != nil {
		logger.Error(err, "failed to create claim")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Update status with scale activity timestamp
	now := metav1.Now()
	scaleUpMessage := fmt.Sprintf("Created claim for pod %s/%s", pod.Namespace, pod.Name)
	if err := r.updateStatus(ctx, nodePool, func(p *kubenodesmithv1alpha1.NodeSmithPool) {
		p.Status.LastScaleActivity = &now
		p.Status.ObservedGeneration = p.Generation
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionTrue,
			Reason:             "ScalingUp",
			Message:            scaleUpMessage,
			ObservedGeneration: p.Generation,
		})
	}); err != nil {
		logger.Error(err, "failed to update pool status")
		return ctrl.Result{}, err
	}

	logger.Info("successfully initiated scale up",
		"pod", pod.Name,
		"cpuCores", cpu,
		"memoryMiB", memMiB,
	)

	r.Recorder.Eventf(nodePool, corev1.EventTypeNormal, "ScaledUp",
		"Created claim to accommodate pod %s/%s (CPU: %d cores, Memory: %d MiB)",
		pod.Namespace, pod.Name, cpu, memMiB)

	return ctrl.Result{}, nil
}

// createClaim creates a NodeSmithClaim for the given pod's resource requirements.
func (r *NodePoolReconciler) createClaim(
	ctx context.Context,
	nodePool *kubenodesmithv1alpha1.NodeSmithPool,
	pod corev1.Pod,
	cpu, memMiB int64,
) error {
	logger := logf.FromContext(ctx)

	idempotencyKey := generateIdempotencyKey(nodePool.Name, string(pod.UID), cpu, memMiB)
	claimName := fmt.Sprintf("%s-%s", nodePool.Name, idempotencyKey[:10])

	// Check if claim already exists
	var existingClaim kubenodesmithv1alpha1.NodeSmithClaim
	existingKey := types.NamespacedName{Namespace: nodePool.Namespace, Name: claimName}
	if err := r.Get(ctx, existingKey, &existingClaim); err == nil {
		if existingClaim.Spec.IdempotencyKey == idempotencyKey {
			logger.Info("reusing existing NodeSmithClaim", "claim", existingClaim.Name)
			return nil
		}
		return fmt.Errorf("claim name collision: %s", claimName)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing claim: %w", err)
	}

	// Create new claim
	claim := &kubenodesmithv1alpha1.NodeSmithClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: nodePool.Namespace,
			Labels: map[string]string{
				nodePool.Spec.PoolLabelKey: nodePool.Name,
			},
		},
		Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
			PoolRef: nodePool.Name,
			Requirements: &kubenodesmithv1alpha1.NodeSmithClaimRequirements{
				CPUCores:  cpu,
				MemoryMiB: memMiB,
			},
			IdempotencyKey: idempotencyKey,
		},
	}

	if err := controllerutil.SetControllerReference(nodePool, claim, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := r.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Race condition: another reconcile created it first
			if err := r.Get(ctx, existingKey, &existingClaim); err == nil {
				if existingClaim.Spec.IdempotencyKey == idempotencyKey {
					logger.Info("another reconciler created the NodeSmithClaim first", "claim", existingClaim.Name)
					return nil
				}
			}
		}
		return fmt.Errorf("failed to create claim: %w", err)
	}

	logger.Info("created NodeSmithClaim", "claim", claimName, "pod", pod.Name)
	return nil
}

func generateIdempotencyKey(poolName, podUID string, cpuCores, memMiB int64) string {
	input := fmt.Sprintf("%s:%s:%d:%d", poolName, podUID, cpuCores, memMiB)
	sum := sha1.Sum([]byte(input))
	return hex.EncodeToString(sum[:])
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
