package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
)

const (
	// Registration timeout - if node doesn't register within this time, deprovision and fail
	registrationTimeout = 15 * time.Minute

	// Requeue intervals for different phases
	requeueRegistration   = 30 * time.Second
	requeueInitialization = 10 * time.Second
	requeueOnError        = time.Minute
	requeueFinalization   = 5 * time.Second
)

// NodeClaimReconciler reconciles a NodeSmithClaim object.
type NodeClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubenodesmith.kubenodesmith.parawell.cloud,resources=nodesmithclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubenodesmith.kubenodesmith.parawell.cloud,resources=nodesmithclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubenodesmith.kubenodesmith.parawell.cloud,resources=nodesmithclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubenodesmith.kubenodesmith.parawell.cloud,resources=nodesmithpools,verbs=get;list
// +kubebuilder:rbac:groups=kubenodesmith.kubenodesmith.parawell.cloud,resources=nodesmithproviders,verbs=get;list
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list
// +kubebuilder:rbac:groups=apps;extensions,resources=daemonsets;replicasets,verbs=get;list
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattachments,verbs=get;list

// Reconcile implements the main reconciliation loop for NodeSmithClaim.
// It follows the Karpenter lifecycle pattern: Launch → Register → Initialize → Ready
func (r *NodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("nodesmithclaim", req.NamespacedName)

	// Fetch the claim
	var claim kubenodesmithv1alpha1.NodeSmithClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion - finalizer ensures proper cleanup
	if !claim.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &claim)
	}

	// Add finalizer if missing (CRITICAL: must happen before any provisioning)
	if !controllerutil.ContainsFinalizer(&claim, kubenodesmithv1alpha1.FinalizerNodeSmithClaim) {
		logger.Info("adding finalizer to claim")
		controllerutil.AddFinalizer(&claim, kubenodesmithv1alpha1.FinalizerNodeSmithClaim)
		if err := r.Update(ctx, &claim); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Return and requeue - ensure finalizer is persisted before continuing
		return ctrl.Result{Requeue: true}, nil
	}

	// Run lifecycle phases in order
	// Each phase returns (result, error) - if non-nil, we stop and return
	if result, err := r.reconcileLaunch(ctx, &claim); result != nil || err != nil {
		return *result, err
	}

	if result, err := r.reconcileRegistration(ctx, &claim); result != nil || err != nil {
		return *result, err
	}

	if result, err := r.reconcileInitialization(ctx, &claim); result != nil || err != nil {
		return *result, err
	}

	// All phases complete - claim is ready
	return ctrl.Result{}, nil
}

// reconcileLaunch handles the machine provisioning phase.
// This is CRITICAL for durability - we must store providerID immediately after provisioning.
func (r *NodeClaimReconciler) reconcileLaunch(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (*ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("claim", claim.Name)

	// Already launched? Check providerID
	if claim.Status.ProviderID != "" {
		// Machine already provisioned, move to next phase
		return nil, nil
	}

	// Check if Launched condition already True (but providerID missing - recovery case)
	launchedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeLaunched)
	if launchedCond != nil && launchedCond.Status == metav1.ConditionTrue {
		// Launched but providerID lost? This shouldn't happen, but log it
		logger.Info("claim marked as launched but missing providerID - possible data loss")
		// TODO: Attempt to find machine by idempotency key from provider
	}

	logger.Info("launching machine for claim",
		"cpuCores", claim.Spec.Requirements.CPUCores,
		"memoryMiB", claim.Spec.Requirements.MemoryMiB,
		"idempotencyKey", claim.Spec.IdempotencyKey,
	)

	// TODO: Get provider from pool and call ProvisionMachine
	// For now, we'll stub this out with a placeholder
	// In a real implementation:
	// 1. Get the pool referenced by claim.Spec.PoolRef
	// 2. Get the provider from the pool
	// 3. Call provider.ProvisionMachine(ctx, spec)
	// 4. Store the returned providerID immediately

	// Placeholder: simulate machine provisioning
	// providerID := fmt.Sprintf("proxmox://cluster-1/vms/%s", claim.Spec.IdempotencyKey[:8])

	// TODO: Uncomment when provider integration is ready
	// claim.Status.ProviderID = providerID

	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
		Status:             metav1.ConditionTrue,
		Reason:             "Launched",
		Message:            "Machine provisioned successfully",
		ObservedGeneration: claim.Generation,
	})

	if err := r.Status().Update(ctx, claim); err != nil {
		logger.Error(err, "failed to update status after launch")
		// CRITICAL: If this fails, next reconcile will retry provisioning
		// Provider MUST be idempotent (return existing machine for same idempotency key)
		return &ctrl.Result{}, err
	}

	r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Launched", "Machine provisioned")
	logger.Info("machine launched successfully")

	// Continue to registration phase immediately
	return nil, nil
}

// reconcileRegistration waits for the node to register with the cluster.
// Implements timeout logic - if node doesn't appear within registrationTimeout, fail the claim.
func (r *NodeClaimReconciler) reconcileRegistration(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (*ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("claim", claim.Name)

	// Not launched yet?
	if claim.Status.ProviderID == "" {
		return nil, nil // Previous phase incomplete
	}

	// Already registered?
	if claim.Status.NodeName != "" {
		return nil, nil // Move to next phase
	}

	// Look for node with matching providerID
	// NOTE: For now using hostname matching since you haven't implemented providerID in cloud-init yet
	// TODO: Switch to providerID matching when ready

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		logger.Error(err, "failed to list nodes")
		return &ctrl.Result{RequeueAfter: requeueRegistration}, nil
	}

	// Try to find the node (using name matching for now)
	// In production, use: node.Spec.ProviderID == claim.Status.ProviderID
	expectedNodeName := fmt.Sprintf("%s-%s", claim.Spec.PoolRef, claim.Name)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// TODO: Change to providerID matching when ready
		// if node.Spec.ProviderID == claim.Status.ProviderID {
		if node.Name == expectedNodeName || nodeMatchesClaim(node, claim) {
			// Found it!
			logger.Info("node registered", "nodeName", node.Name)
			claim.Status.NodeName = node.Name

			// Apply labels from pool to node if needed
			// TODO: Get pool and apply template labels

			meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               kubenodesmithv1alpha1.ConditionTypeRegistered,
				Status:             metav1.ConditionTrue,
				Reason:             "Registered",
				Message:            fmt.Sprintf("Node %s joined cluster", node.Name),
				ObservedGeneration: claim.Generation,
			})

			if err := r.Status().Update(ctx, claim); err != nil {
				logger.Error(err, "failed to update status after registration")
				return &ctrl.Result{}, err
			}

			r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Registered", "Node %s joined cluster", node.Name)
			return nil, nil
		}
	}

	// Node not found yet - check timeout
	launchedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeLaunched)
	if launchedCond != nil {
		timeSinceLaunch := time.Since(launchedCond.LastTransitionTime.Time)
		if timeSinceLaunch > registrationTimeout {
			// Timeout! Fail the claim
			logger.Info("registration timeout exceeded", "elapsed", timeSinceLaunch)

			// TODO: Deprovision the machine
			// provider.DeprovisionMachine(ctx, claim.Status.ProviderID)

			meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               kubenodesmithv1alpha1.ConditionTypeRegistered,
				Status:             metav1.ConditionFalse,
				Reason:             "Timeout",
				Message:            fmt.Sprintf("Node did not register within %v", registrationTimeout),
				ObservedGeneration: claim.Generation,
			})

			if err := r.Status().Update(ctx, claim); err != nil {
				logger.Error(err, "failed to update status after timeout")
			}

			r.Recorder.Eventf(claim, corev1.EventTypeWarning, "RegistrationTimeout",
				"Node did not register within %v", registrationTimeout)

			// Don't requeue - this is a terminal failure
			return &ctrl.Result{}, fmt.Errorf("registration timeout")
		}

		logger.V(1).Info("waiting for node to register", "elapsed", timeSinceLaunch, "timeout", registrationTimeout)
	}

	// Still waiting - requeue
	return &ctrl.Result{RequeueAfter: requeueRegistration}, nil
}

// reconcileInitialization waits for the node to become Ready.
func (r *NodeClaimReconciler) reconcileInitialization(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (*ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("claim", claim.Name)

	// Not registered yet?
	if claim.Status.NodeName == "" {
		return nil, nil // Previous phase incomplete
	}

	// Already initialized?
	initializedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeInitialized)
	if initializedCond != nil && initializedCond.Status == metav1.ConditionTrue {
		// Check if Ready condition is also set
		readyCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			return nil, nil // Fully ready, nothing to do
		}

		// Initialized but not ready - set Ready condition
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               kubenodesmithv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            "Claim is fully operational",
			ObservedGeneration: claim.Generation,
		})

		if err := r.Status().Update(ctx, claim); err != nil {
			logger.Error(err, "failed to update Ready condition")
			return &ctrl.Result{}, err
		}

		r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Ready", "Claim is ready")
		return nil, nil
	}

	// Get the node
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("node not found - may have been deleted")
			// Node disappeared - this is unexpected, requeue
			return &ctrl.Result{RequeueAfter: requeueInitialization}, nil
		}
		logger.Error(err, "failed to get node")
		return &ctrl.Result{RequeueAfter: requeueInitialization}, nil
	}

	// Check if node is Ready
	nodeReady := false
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			nodeReady = true
			break
		}
	}

	if !nodeReady {
		logger.V(1).Info("waiting for node to become ready", "nodeName", node.Name)
		return &ctrl.Result{RequeueAfter: requeueInitialization}, nil
	}

	// Node is ready!
	logger.Info("node is ready", "nodeName", node.Name)

	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               kubenodesmithv1alpha1.ConditionTypeInitialized,
		Status:             metav1.ConditionTrue,
		Reason:             "Initialized",
		Message:            "Node is ready",
		ObservedGeneration: claim.Generation,
	})

	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               kubenodesmithv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "Claim is fully operational",
		ObservedGeneration: claim.Generation,
	})

	if err := r.Status().Update(ctx, claim); err != nil {
		logger.Error(err, "failed to update status after initialization")
		return &ctrl.Result{}, err
	}

	r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Ready", "Claim is ready, node %s is operational", node.Name)
	logger.Info("claim is now ready")

	return nil, nil
}

// finalize handles cleanup when a claim is deleted.
// Following Karpenter pattern: delete node first, then deprovision machine, then remove finalizer.
func (r *NodeClaimReconciler) finalize(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("claim", claim.Name)

	if !controllerutil.ContainsFinalizer(claim, kubenodesmithv1alpha1.FinalizerNodeSmithClaim) {
		return ctrl.Result{}, nil // Already finalized
	}

	logger.Info("finalizing claim")

	// Step 1: Delete the Node first (triggers drain)
	if claim.Status.NodeName != "" {
		var node corev1.Node
		err := r.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, &node)
		if err == nil {
			logger.Info("deleting node", "nodeName", node.Name)
			if err := r.Delete(ctx, &node); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete node")
				return ctrl.Result{RequeueAfter: requeueFinalization}, err
			}
			r.Recorder.Eventf(claim, corev1.EventTypeNormal, "NodeDeleted", "Node %s deleted", node.Name)
		} else if !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to get node for deletion")
			return ctrl.Result{RequeueAfter: requeueFinalization}, err
		}

		// Wait for node to be fully deleted before deprovisioning machine
		if err == nil || !apierrors.IsNotFound(err) {
			logger.V(1).Info("waiting for node to be deleted")
			return ctrl.Result{RequeueAfter: requeueFinalization}, nil
		}
	}

	// Step 2: Deprovision the machine from provider
	if claim.Status.ProviderID != "" {
		logger.Info("deprovisioning machine", "providerID", claim.Status.ProviderID)

		// TODO: Get provider and call DeprovisionMachine
		// provider.DeprovisionMachine(ctx, claim.Status.ProviderID)

		r.Recorder.Eventf(claim, corev1.EventTypeNormal, "MachineDeprovisioned",
			"Machine deprovisioned")
	}

	// Step 3: Remove finalizer (allows garbage collection)
	logger.Info("removing finalizer")
	controllerutil.RemoveFinalizer(claim, kubenodesmithv1alpha1.FinalizerNodeSmithClaim)
	if err := r.Update(ctx, claim); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("claim finalized successfully")
	return ctrl.Result{}, nil
}

// nodeMatchesClaim attempts to match a node to a claim using heuristics.
// TODO: Replace with providerID matching when implemented.
func nodeMatchesClaim(node *corev1.Node, claim *kubenodesmithv1alpha1.NodeSmithClaim) bool {
	// Check if node has the pool label
	if poolLabel, ok := node.Labels["topology.kubenodesmith.io/pool"]; ok {
		if poolLabel == claim.Spec.PoolRef {
			// TODO: Add more specific matching criteria
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		Named("nodeclaim").
		Complete(r)
}
