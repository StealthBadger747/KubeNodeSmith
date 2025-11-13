package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
	"github.com/StealthBadger747/KubeNodeSmith/internal/provider"
)

const (
	// Registration timeout - if node doesn't register within this time, deprovision and fail
	registrationTimeout = 15 * time.Minute

	// Maximum number of times we'll attempt to launch/register a machine before marking the claim failed
	maxLaunchAttempts = 3

	// Requeue intervals for different phases
	immediateRequeueDelay = time.Millisecond
	requeueRegistration   = 30 * time.Second
	requeueInitialization = 10 * time.Second
	requeueOnError        = time.Minute
	requeueFinalization   = 5 * time.Second
)

// NodeClaimReconciler reconciles a NodeSmithClaim object.
type NodeClaimReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	ProviderFactories map[string]ProviderBuilder
}

// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithpools,verbs=get;list
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
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
		return ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
	}

	// Run lifecycle phases in order
	// Each phase returns (result, error) - if non-nil, we stop and return
	if failedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed); failedCond != nil && failedCond.Status == metav1.ConditionTrue {
		logger.Info("claim is in failed state, skipping reconciliation", "reason", failedCond.Reason)
		return ctrl.Result{}, nil
	}

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
func (r *NodeClaimReconciler) reconcileLaunch(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (*ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("claim", claim.Name)

	// Already launched? Check providerID
	if claim.Status.ProviderID != "" {
		// Machine already provisioned, move to next phase
		return nil, nil
	}

	if claim.Status.LaunchAttempts >= maxLaunchAttempts {
		launchCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeLaunched)
		message := fmt.Sprintf("Exceeded maximum launch attempts (%d)", maxLaunchAttempts)
		if launchCond != nil && strings.TrimSpace(launchCond.Message) != "" {
			message = fmt.Sprintf("%s; last launch error: %s", message, launchCond.Message)
		}

		if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
			setCondition := func(condType string, isTrue bool) {
				status := metav1.ConditionFalse
				if isTrue {
					status = metav1.ConditionTrue
				}
				meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
					Type:               condType,
					Status:             status,
					Reason:             "LaunchAttemptsExceeded",
					Message:            message,
					ObservedGeneration: updated.Generation,
				})
			}

			setCondition(kubenodesmithv1alpha1.ConditionTypeFailed, true)
			setCondition(kubenodesmithv1alpha1.ConditionTypeLaunched, false)
		}); err != nil {
			logger.Error(err, "failed to update status after exceeding launch attempts")
			return &ctrl.Result{}, err
		}

		logger.Info("maximum launch attempts reached, marking claim failed")
		return &ctrl.Result{}, nil
	}

	// Check if Launched condition already True (but providerID missing - recovery case)
	launchedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeLaunched)
	if launchedCond != nil && launchedCond.Status == metav1.ConditionTrue {
		// Launched but providerID lost? This shouldn't happen, but log it
		logger.Info("claim marked as launched but missing providerID - possible data loss")
		// TODO: Attempt to find machine by idempotency key from provider
	}

	nextAttempt := claim.Status.LaunchAttempts + 1

	logger.Info("launching machine for claim",
		"cpuCores", claim.Spec.Requirements.CPUCores,
		"memoryMiB", claim.Spec.Requirements.MemoryMiB,
		"idempotencyKey", claim.Spec.IdempotencyKey,
		"attempt", nextAttempt,
		"maxAttempts", maxLaunchAttempts,
	)

	// Get the provider for this claim
	prov, err := r.getProviderForClaim(ctx, claim)
	if err != nil {
		logger.Error(err, "failed to get provider for claim")
		if updateErr := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
			meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
				Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
				Status:             metav1.ConditionFalse,
				Reason:             "ProviderError",
				Message:            fmt.Sprintf("Failed to get provider: %v", err),
				ObservedGeneration: updated.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status after provider error")
		}
		return &ctrl.Result{RequeueAfter: requeueOnError}, err
	}

	// Build the machine machineSpec
	machineSpec := provider.MachineSpec{
		MachineName: claim.Name,
		CPUCores:    claim.Spec.Requirements.CPUCores,
		MemoryMiB:   claim.Spec.Requirements.MemoryMiB,
	}

	// Persist the attempt number so registration timeouts know how many retries remain
	claim.Status.LaunchAttempts = nextAttempt

	// Provision the machine
	machine, err := prov.ProvisionMachine(ctx, machineSpec)
	if err != nil {
		logger.Error(err, "failed to provision machine")
		if updateErr := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
			updated.Status.LaunchAttempts = nextAttempt
			meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
				Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
				Status:             metav1.ConditionFalse,
				Reason:             "ProvisioningFailed",
				Message:            fmt.Sprintf("Failed to provision machine: %v", err),
				ObservedGeneration: updated.Generation,
			})
		}); updateErr != nil {
			logger.Error(updateErr, "failed to update status after provisioning error")
		}
		return &ctrl.Result{RequeueAfter: requeueOnError}, err
	}

	logger.Info("machine provisioned successfully",
		"providerID", machine.ProviderID,
		"kubeNodeName", machine.KubeNodeName,
	)

	// CRITICAL: Store the providerID immediately
	claim.Status.ProviderID = machine.ProviderID

	if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
		updated.Status.ProviderID = machine.ProviderID
		updated.Status.LaunchAttempts = nextAttempt
		meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
			Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
			Status:             metav1.ConditionTrue,
			Reason:             "Launched",
			Message:            fmt.Sprintf("Machine provisioned: %s", machine.ProviderID),
			ObservedGeneration: updated.Generation,
		})
	}); err != nil {
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
	expectedNodeName := claim.Name // TODO: Change to providerID matching when ready
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// TODO: Change to providerID matching when ready
		// if node.Spec.ProviderID == claim.Status.ProviderID {
		if node.Name == expectedNodeName || nodeMatchesClaim(node, claim) {
			// Found it!
			logger.Info("node registered", "nodeName", node.Name)

			pool, err := r.getPoolForClaim(ctx, claim)
			if err != nil {
				logger.Error(err, "failed to fetch pool for node labeling")
				return &ctrl.Result{RequeueAfter: requeueRegistration}, err
			}

			if err := r.ensureNodeLabels(ctx, node, pool); err != nil {
				logger.Error(err, "failed to ensure node labels", "nodeName", node.Name)
				return &ctrl.Result{RequeueAfter: requeueRegistration}, err
			}

			claim.Status.NodeName = node.Name
			claim.Status.LaunchAttempts = 0
			meta.RemoveStatusCondition(&claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed)

			// Apply labels from pool to node if needed
			// TODO: Get pool and apply template labels

			if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
				updated.Status.NodeName = node.Name
				updated.Status.LaunchAttempts = 0
				meta.RemoveStatusCondition(&updated.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed)
				meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
					Type:               kubenodesmithv1alpha1.ConditionTypeRegistered,
					Status:             metav1.ConditionTrue,
					Reason:             "Registered",
					Message:            fmt.Sprintf("Node %s joined cluster", node.Name),
					ObservedGeneration: updated.Generation,
				})
			}); err != nil {
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
			// Timeout! Fail the claim and deprovision the machine
			logger.Info("registration timeout exceeded", "elapsed", timeSinceLaunch)

			// Deprovision the machine
			prov, err := r.getProviderForClaim(ctx, claim)
			if err != nil {
				logger.Error(err, "failed to get provider for deprovisioning after timeout")
			} else {
				machine := provider.Machine{
					ProviderID:   claim.Status.ProviderID,
					KubeNodeName: claim.Status.NodeName,
				}
				if err := prov.DeprovisionMachine(ctx, machine); err != nil {
					logger.Error(err, "failed to deprovision machine after timeout")
					// Continue anyway - we mark this as failed regardless
				} else {
					logger.Info("machine deprovisioned after registration timeout")
					r.Recorder.Eventf(claim, corev1.EventTypeWarning, "MachineDeprovisioned",
						"Machine deprovisioned due to registration timeout")
				}
			}

			claim.Status.ProviderID = ""
			attempts := claim.Status.LaunchAttempts
			if attempts == 0 {
				attempts = 1
			}

			maxAttemptsReached := attempts >= maxLaunchAttempts
			var registeredMessage string
			if maxAttemptsReached {
				registeredMessage = fmt.Sprintf("Node did not register within %v after %d attempts; giving up", registrationTimeout, attempts)
			} else {
				registeredMessage = fmt.Sprintf("Node did not register within %v (attempt %d/%d); retrying", registrationTimeout, attempts, maxLaunchAttempts)
			}

			if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
				updated.Status.ProviderID = ""
				meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
					Type:               kubenodesmithv1alpha1.ConditionTypeRegistered,
					Status:             metav1.ConditionFalse,
					Reason:             "Timeout",
					Message:            registeredMessage,
					ObservedGeneration: updated.Generation,
				})

				if maxAttemptsReached {
					meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
						Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
						Status:             metav1.ConditionFalse,
						Reason:             "RegistrationTimeoutExceeded",
						Message:            registeredMessage,
						ObservedGeneration: updated.Generation,
					})
					meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
						Type:               kubenodesmithv1alpha1.ConditionTypeFailed,
						Status:             metav1.ConditionTrue,
						Reason:             "RegistrationTimeoutExceeded",
						Message:            registeredMessage,
						ObservedGeneration: updated.Generation,
					})
					return
				}

				meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
					Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
					Status:             metav1.ConditionFalse,
					Reason:             "RegistrationTimeout",
					Message:            fmt.Sprintf("Cleared provider ID to retry launch (%d/%d)", attempts+1, maxLaunchAttempts),
					ObservedGeneration: updated.Generation,
				})
			}); err != nil {
				logger.Error(err, "failed to update status after registration timeout")
				return &ctrl.Result{}, err
			}

			if maxAttemptsReached {
				r.Recorder.Eventf(claim, corev1.EventTypeWarning, "RegistrationFailed",
					"Node did not register within %v after %d attempts; giving up", registrationTimeout, attempts)
				return &ctrl.Result{}, nil
			}

			r.Recorder.Eventf(claim, corev1.EventTypeWarning, "RegistrationTimeout",
				"Node did not register within %v (attempt %d/%d); retrying", registrationTimeout, attempts, maxLaunchAttempts)

			return &ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
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
		if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
			meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
				Type:               kubenodesmithv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Claim is fully operational",
				ObservedGeneration: updated.Generation,
			})
		}); err != nil {
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

	if err := r.updateStatus(ctx, claim, func(updated *kubenodesmithv1alpha1.NodeSmithClaim) {
		meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
			Type:               kubenodesmithv1alpha1.ConditionTypeInitialized,
			Status:             metav1.ConditionTrue,
			Reason:             "Initialized",
			Message:            "Node is ready",
			ObservedGeneration: updated.Generation,
		})

		meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
			Type:               kubenodesmithv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            "Claim is fully operational",
			ObservedGeneration: updated.Generation,
		})
	}); err != nil {
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

		prov, err := r.getProviderForClaim(ctx, claim)
		if err != nil {
			// Failed to get provider - this could mean the pool or provider was deleted
			// Log the error and emit a warning event for visibility
			logger.Error(err, "failed to get provider for deprovisioning",
				"providerID", claim.Status.ProviderID,
				"pool", claim.Spec.PoolRef,
			)

			r.Recorder.Eventf(claim, corev1.EventTypeWarning, "DeprovisioningFailed",
				"Cannot deprovision machine %s: provider unavailable. Manual cleanup may be required for pool %s",
				claim.Status.ProviderID, claim.Spec.PoolRef)

			// Check if this is a NotFound error - if so, resources were deleted
			if apierrors.IsNotFound(err) {
				logger.Info("pool or provider was deleted - cannot deprovision machine, manual cleanup required",
					"providerID", claim.Status.ProviderID,
					"pool", claim.Spec.PoolRef,
				)
			}
			// Continue with finalizer removal - don't block cleanup indefinitely
			// but the warning event provides visibility for manual intervention
		} else {
			machine := provider.Machine{
				ProviderID: claim.Status.ProviderID,
			}
			if err := prov.DeprovisionMachine(ctx, machine); err != nil {
				logger.Error(err, "failed to deprovision machine",
					"providerID", claim.Status.ProviderID,
				)
				r.Recorder.Eventf(claim, corev1.EventTypeWarning, "DeprovisioningFailed",
					"Failed to deprovision machine %s: %v. Manual cleanup may be required",
					claim.Status.ProviderID, err)
				// Continue with finalizer removal - don't block cleanup
			} else {
				logger.Info("machine deprovisioned successfully")
				r.Recorder.Eventf(claim, corev1.EventTypeNormal, "MachineDeprovisioned",
					"Machine %s deprovisioned", claim.Status.ProviderID)
			}
		}
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

func (r *NodeClaimReconciler) getPoolForClaim(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (*kubenodesmithv1alpha1.NodeSmithPool, error) {
	var pool kubenodesmithv1alpha1.NodeSmithPool
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.PoolRef}
	if err := r.Get(ctx, key, &pool); err != nil {
		return nil, fmt.Errorf("get pool %s: %w", key.String(), err)
	}
	return &pool, nil
}

func (r *NodeClaimReconciler) ensureNodeLabels(
	ctx context.Context,
	node *corev1.Node,
	pool *kubenodesmithv1alpha1.NodeSmithPool,
) error {
	labelKey := pool.Spec.PoolLabelKey
	if strings.TrimSpace(labelKey) == "" {
		labelKey = "topology.kubenodesmith.io/pool"
	}

	desired := node.DeepCopy()
	if desired.Labels == nil {
		desired.Labels = map[string]string{}
	}

	changed := false
	if desired.Labels[labelKey] != pool.Name {
		desired.Labels[labelKey] = pool.Name
		changed = true
	}

	for k, v := range pool.Spec.MachineTemplate.Labels {
		if existing, ok := desired.Labels[k]; !ok || existing != v {
			desired.Labels[k] = v
			changed = true
		}
	}

	if !changed {
		return nil
	}

	return r.Patch(ctx, desired, client.MergeFrom(node))
}

// getProviderForClaim retrieves the provider instance for a claim by looking up its pool and provider.
func (r *NodeClaimReconciler) getProviderForClaim(ctx context.Context, claim *kubenodesmithv1alpha1.NodeSmithClaim) (provider.Provider, error) {
	logger := logf.FromContext(ctx)

	// Get the pool
	var pool kubenodesmithv1alpha1.NodeSmithPool
	poolKey := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.PoolRef}
	if err := r.Get(ctx, poolKey, &pool); err != nil {
		return nil, fmt.Errorf("get pool %s: %w", poolKey, err)
	}

	if pool.Spec.ProviderRef == "" {
		return nil, fmt.Errorf("pool %s has empty providerRef", pool.Name)
	}

	// Get the provider
	var providerObj kubenodesmithv1alpha1.NodeSmithProvider
	providerKey := types.NamespacedName{Namespace: claim.Namespace, Name: pool.Spec.ProviderRef}
	if err := r.Get(ctx, providerKey, &providerObj); err != nil {
		return nil, fmt.Errorf("get provider %s: %w", providerKey, err)
	}

	providerType := strings.ToLower(strings.TrimSpace(providerObj.Spec.Type))
	if providerType == "" {
		return nil, fmt.Errorf("provider %s has empty type", providerObj.Name)
	}

	// Get the provider factory
	if len(r.ProviderFactories) == 0 {
		return nil, fmt.Errorf("no provider factories configured")
	}

	builder, ok := r.ProviderFactories[providerType]
	if !ok {
		return nil, fmt.Errorf("unsupported provider type %q", providerType)
	}

	// Build the provider instance
	providerInstance, err := builder(ctx, &providerObj)
	if err != nil {
		return nil, fmt.Errorf("initialize provider %s: %w", providerObj.Name, err)
	}

	logger.V(1).Info("resolved provider for claim",
		"pool", pool.Name,
		"provider", providerObj.Name,
		"providerType", providerType,
	)

	return providerInstance, nil
}

func (r *NodeClaimReconciler) updateStatus(
	ctx context.Context,
	claim *kubenodesmithv1alpha1.NodeSmithClaim,
	mutate func(*kubenodesmithv1alpha1.NodeSmithClaim),
) error {
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest kubenodesmithv1alpha1.NodeSmithClaim
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		original := latest.Status.DeepCopy()
		if original == nil {
			original = &kubenodesmithv1alpha1.NodeSmithClaimStatus{}
		}
		mutate(&latest)
		if apiequality.Semantic.DeepEqual(original, &latest.Status) {
			claim.Status = latest.Status
			return nil
		}
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		claim.Status = latest.Status
		return nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		Named("nodeclaim").
		Complete(r)
}
