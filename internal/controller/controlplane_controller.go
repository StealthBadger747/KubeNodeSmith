package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
	"github.com/StealthBadger747/KubeNodeSmith/internal/provider"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type providerCacheEntry struct {
	provider        provider.Provider
	resourceVersion string
}

// ControlPlaneReconciler reconciles a NodeSmithControlPlane object
type ControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ProviderFactories map[string]ProviderBuilder

	providerMu    sync.RWMutex
	providerCache map[types.NamespacedName]providerCacheEntry
}

// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithcontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithcontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubenodesmith.parawell.cloud,resources=nodesmithcontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NodeSmithControlPlane object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("controlplane", req.NamespacedName)

	var controllerObj kubenodesmithv1alpha1.NodeSmithControlPlane
	if err := r.Get(ctx, req.NamespacedName, &controllerObj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get NodeSmithControlPlane: %w", err)
	}

	originalStatus := controllerObj.Status.DeepCopy()

	ready := true
	reason := "PoolsResolved"
	message := ""

	resolvedPools := make([]string, 0, len(controllerObj.Spec.Pools))
	resolvedProviders := make([]string, 0)

	for _, poolName := range controllerObj.Spec.Pools {
		var pool kubenodesmithv1alpha1.NodeSmithPool
		poolKey := types.NamespacedName{Namespace: controllerObj.Namespace, Name: poolName}
		if err := r.Get(ctx, poolKey, &pool); err != nil {
			if apierrors.IsNotFound(err) {
				ready = false
				reason = "PoolMissing"
				message = fmt.Sprintf("referenced pool %q not found", poolName)
				logger.Info("referenced pool not found", "pool", poolName)
				break
			}
			return ctrl.Result{}, fmt.Errorf("get NodeSmithPool %s: %w", poolKey, err)
		}
		resolvedPools = append(resolvedPools, pool.Name)

		if pool.Spec.ProviderRef == "" {
			ready = false
			reason = "ProviderRefMissing"
			message = fmt.Sprintf("pool %q has empty spec.providerRef", pool.Name)
			break
		}

		var providerObj kubenodesmithv1alpha1.NodeSmithProvider
		providerKey := types.NamespacedName{Namespace: controllerObj.Namespace, Name: pool.Spec.ProviderRef}
		if err := r.Get(ctx, providerKey, &providerObj); err != nil {
			if apierrors.IsNotFound(err) {
				ready = false
				reason = "ProviderMissing"
				message = fmt.Sprintf("provider %q referenced by pool %q not found", pool.Spec.ProviderRef, pool.Name)
				logger.Info("referenced provider not found", "provider", pool.Spec.ProviderRef, "pool", pool.Name)
				break
			}
			return ctrl.Result{}, fmt.Errorf("get NodeSmithProvider %s: %w", providerKey, err)
		}

		providerType := strings.ToLower(strings.TrimSpace(providerObj.Spec.Type))
		if providerType == "" {
			ready = false
			reason = "ProviderTypeMissing"
			message = fmt.Sprintf("provider %q missing spec.type", providerObj.Name)
			break
		}

		if _, err := r.ensureProvider(ctx, providerType, &providerObj); err != nil {
			ready = false
			reason = "ProviderInitializationFailed"
			message = fmt.Sprintf("failed to initialize provider %q: %v", providerObj.Name, err)
			logger.Error(err, "failed to initialize provider", "provider", providerObj.Name)
			break
		}

		resolvedProviders = append(resolvedProviders, providerObj.Name)
	}

	if len(controllerObj.Spec.Pools) == 0 {
		message = "no pools configured"
	}

	if message == "" {
		message = fmt.Sprintf("resolved pools [%s] with providers [%s]",
			strings.Join(resolvedPools, ", "), strings.Join(resolvedProviders, ", "))
	}

	now := metav1.Now()
	controllerObj.Status.ObservedGeneration = controllerObj.GetGeneration()
	controllerObj.Status.LastSyncTime = &now

	conditionStatus := metav1.ConditionTrue
	if !ready {
		conditionStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&controllerObj.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: controllerObj.GetGeneration(),
	})

	if !apiequality.Semantic.DeepEqual(originalStatus, &controllerObj.Status) {
		if err := r.Status().Update(ctx, &controllerObj); err != nil {
			return ctrl.Result{}, fmt.Errorf("update NodeSmithControlPlane status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	poolSource := source.Kind(
		mgr.GetCache(),
		&kubenodesmithv1alpha1.NodeSmithPool{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapPoolToControllers),
	)

	providerSource := source.Kind(
		mgr.GetCache(),
		&kubenodesmithv1alpha1.NodeSmithProvider{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapProviderToControllers),
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubenodesmithv1alpha1.NodeSmithControlPlane{}).
		WatchesRawSource(poolSource).
		WatchesRawSource(providerSource).
		Named("controlplane").
		Complete(r)
}

func (r *ControlPlaneReconciler) ensureProvider(ctx context.Context, providerType string, providerObj *kubenodesmithv1alpha1.NodeSmithProvider) (provider.Provider, error) {
	key := types.NamespacedName{Namespace: providerObj.Namespace, Name: providerObj.Name}
	r.providerMu.RLock()
	if entry, ok := r.providerCache[key]; ok && entry.resourceVersion == providerObj.ResourceVersion {
		defer r.providerMu.RUnlock()
		return entry.provider, nil
	}
	r.providerMu.RUnlock()

	if len(r.ProviderFactories) == 0 {
		return nil, fmt.Errorf("no provider factories registered")
	}

	builder, ok := r.ProviderFactories[providerType]
	if !ok {
		return nil, fmt.Errorf("unsupported provider type %q", providerObj.Spec.Type)
	}

	providerInstance, err := builder(ctx, providerObj)
	if err != nil {
		return nil, err
	}

	r.providerMu.Lock()
	if r.providerCache == nil {
		r.providerCache = make(map[types.NamespacedName]providerCacheEntry)
	}
	r.providerCache[key] = providerCacheEntry{
		provider:        providerInstance,
		resourceVersion: providerObj.ResourceVersion,
	}
	r.providerMu.Unlock()

	return providerInstance, nil
}

func (r *ControlPlaneReconciler) mapPoolToControllers(ctx context.Context, pool *kubenodesmithv1alpha1.NodeSmithPool) []reconcile.Request {
	var controllerList kubenodesmithv1alpha1.NodeSmithControlPlaneList
	if err := r.List(ctx, &controllerList, client.InNamespace(pool.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "list NodeSmithControlPlanes for pool watch", "namespace", pool.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(controllerList.Items))
	for _, controllerObj := range controllerList.Items {
		if contains(controllerObj.Spec.Pools, pool.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: controllerObj.Namespace,
					Name:      controllerObj.Name,
				},
			})
		}
	}

	return requests
}

func (r *ControlPlaneReconciler) mapProviderToControllers(ctx context.Context, providerObj *kubenodesmithv1alpha1.NodeSmithProvider) []reconcile.Request {
	logger := logf.FromContext(ctx)

	var poolList kubenodesmithv1alpha1.NodeSmithPoolList
	if err := r.List(ctx, &poolList, client.InNamespace(providerObj.Namespace)); err != nil {
		logger.Error(err, "list NodeSmithPools for provider watch", "namespace", providerObj.Namespace)
		return nil
	}

	poolNames := make(map[string]struct{})
	for _, pool := range poolList.Items {
		if pool.Spec.ProviderRef == providerObj.Name {
			poolNames[pool.Name] = struct{}{}
		}
	}

	var controllerList kubenodesmithv1alpha1.NodeSmithControlPlaneList
	if err := r.List(ctx, &controllerList, client.InNamespace(providerObj.Namespace)); err != nil {
		logger.Error(err, "list NodeSmithControlPlanes for provider watch", "namespace", providerObj.Namespace)
		return nil
	}

	requestSet := make(map[types.NamespacedName]struct{})
	for _, controllerObj := range controllerList.Items {
		for poolName := range poolNames {
			if contains(controllerObj.Spec.Pools, poolName) {
				requestSet[types.NamespacedName{
					Namespace: controllerObj.Namespace,
					Name:      controllerObj.Name,
				}] = struct{}{}
				break
			}
		}
	}

	if len(requestSet) == 0 {
		// Provider update still matters; requeue all controllers in namespace to refresh caches.
		for _, controllerObj := range controllerList.Items {
			requestSet[types.NamespacedName{
				Namespace: controllerObj.Namespace,
				Name:      controllerObj.Name,
			}] = struct{}{}
		}
	}

	requests := make([]reconcile.Request, 0, len(requestSet))
	for nn := range requestSet {
		requests = append(requests, reconcile.Request{NamespacedName: nn})
	}

	return requests
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
