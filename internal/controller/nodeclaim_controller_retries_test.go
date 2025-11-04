package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clientrecord "k8s.io/client-go/tools/record"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
	"github.com/StealthBadger747/KubeNodeSmith/internal/provider"
)

type stubProvider struct {
	deprovisionCount int
	lastMachine      provider.Machine
}

func (s *stubProvider) ProvisionMachine(ctx context.Context, spec provider.MachineSpec) (*provider.Machine, error) {
	return &provider.Machine{
		ProviderID:   "stub-provider",
		KubeNodeName: "stub-node",
	}, nil
}

func (s *stubProvider) DeprovisionMachine(ctx context.Context, machine provider.Machine) error {
	s.deprovisionCount++
	s.lastMachine = machine
	return nil
}

func (s *stubProvider) ListMachines(ctx context.Context, namePrefix string) ([]provider.Machine, error) {
	return nil, nil
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := kubenodesmithv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add nodesmith scheme: %v", err)
	}

	return scheme
}

func TestReconcileLaunchMarksClaimFailedWhenAttemptsExceeded(t *testing.T) {
	scheme := newTestScheme(t)

	claim := &kubenodesmithv1alpha1.NodeSmithClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "claim-failed",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
			PoolRef: "pool",
		},
		Status: kubenodesmithv1alpha1.NodeSmithClaimStatus{
			LaunchAttempts: maxLaunchAttempts,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		WithObjects(claim.DeepCopy()).
		Build()

	reconciler := &NodeClaimReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: clientrecord.NewFakeRecorder(10),
	}

	ctx := context.Background()

	result, err := reconciler.reconcileLaunch(ctx, claim)
	if err != nil {
		t.Fatalf("reconcileLaunch returned error: %v", err)
	}
	if result == nil {
		t.Fatalf("expected result, got nil")
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %#v", result)
	}

	failedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed)
	if failedCond == nil || failedCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected failed condition to be true, got %#v", failedCond)
	}
}

func TestReconcileRegistrationRetriesAfterTimeout(t *testing.T) {
	scheme := newTestScheme(t)
	now := time.Now()

	claim := &kubenodesmithv1alpha1.NodeSmithClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "claim-retry",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
			PoolRef: "pool",
		},
		Status: kubenodesmithv1alpha1.NodeSmithClaimStatus{
			ProviderID:     "provider-1",
			LaunchAttempts: 1,
			Conditions: []metav1.Condition{
				{
					Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(now.Add(-2 * registrationTimeout)),
				},
			},
		},
	}

	pool := &kubenodesmithv1alpha1.NodeSmithPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pool",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithPoolSpec{
			ProviderRef: "provider",
			Limits: kubenodesmithv1alpha1.NodePoolLimits{
				MinNodes:  0,
				MaxNodes:  0,
				CPUCores:  0,
				MemoryMiB: 0,
			},
			MachineTemplate: kubenodesmithv1alpha1.MachineTemplate{
				KubeNodeNamePrefix: "pool-node",
			},
		},
	}

	providerObj := &kubenodesmithv1alpha1.NodeSmithProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "provider",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithProviderSpec{
			Type: "proxmox",
			Proxmox: &kubenodesmithv1alpha1.ProxmoxProviderSpec{
				Endpoint: "https://example",
			},
		},
	}

	stub := &stubProvider{}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		WithObjects(claim.DeepCopy(), pool, providerObj).
		Build()

	reconciler := &NodeClaimReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: clientrecord.NewFakeRecorder(10),
		ProviderFactories: map[string]ProviderBuilder{
			"proxmox": func(ctx context.Context, providerObj *kubenodesmithv1alpha1.NodeSmithProvider) (provider.Provider, error) {
				return stub, nil
			},
		},
	}

	ctx := context.Background()
	result, err := reconciler.reconcileRegistration(ctx, claim)
	if err != nil {
		t.Fatalf("reconcileRegistration returned error: %v", err)
	}
	if result == nil {
		t.Fatalf("expected result, got nil")
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected requeue for retry, got %#v", result)
	}

	if stub.deprovisionCount != 1 {
		t.Fatalf("expected deprovision to be called once, got %d", stub.deprovisionCount)
	}

	if claim.Status.ProviderID != "" {
		t.Fatalf("expected providerID to be cleared, got %q", claim.Status.ProviderID)
	}

	failedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed)
	if failedCond != nil && failedCond.Status == metav1.ConditionTrue {
		t.Fatalf("did not expect failed condition to be true")
	}

	launchedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeLaunched)
	if launchedCond == nil || launchedCond.Status != metav1.ConditionFalse || launchedCond.Reason != "RegistrationTimeout" {
		t.Fatalf("expected launched condition to indicate retry, got %#v", launchedCond)
	}
}

func TestReconcileRegistrationMarksFailedAfterMaxAttempts(t *testing.T) {
	scheme := newTestScheme(t)
	now := time.Now()

	claim := &kubenodesmithv1alpha1.NodeSmithClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "claim-max",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
			PoolRef: "pool",
		},
		Status: kubenodesmithv1alpha1.NodeSmithClaimStatus{
			ProviderID:     "provider-1",
			LaunchAttempts: maxLaunchAttempts,
			Conditions: []metav1.Condition{
				{
					Type:               kubenodesmithv1alpha1.ConditionTypeLaunched,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(now.Add(-2 * registrationTimeout)),
				},
			},
		},
	}

	pool := &kubenodesmithv1alpha1.NodeSmithPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pool",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithPoolSpec{
			ProviderRef: "provider",
			Limits: kubenodesmithv1alpha1.NodePoolLimits{
				MinNodes:  0,
				MaxNodes:  0,
				CPUCores:  0,
				MemoryMiB: 0,
			},
			MachineTemplate: kubenodesmithv1alpha1.MachineTemplate{
				KubeNodeNamePrefix: "pool-node",
			},
		},
	}

	providerObj := &kubenodesmithv1alpha1.NodeSmithProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "provider",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: kubenodesmithv1alpha1.NodeSmithProviderSpec{
			Type: "proxmox",
			Proxmox: &kubenodesmithv1alpha1.ProxmoxProviderSpec{
				Endpoint: "https://example",
			},
		},
	}

	stub := &stubProvider{}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kubenodesmithv1alpha1.NodeSmithClaim{}).
		WithObjects(claim.DeepCopy(), pool, providerObj).
		Build()

	reconciler := &NodeClaimReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: clientrecord.NewFakeRecorder(10),
		ProviderFactories: map[string]ProviderBuilder{
			"proxmox": func(ctx context.Context, providerObj *kubenodesmithv1alpha1.NodeSmithProvider) (provider.Provider, error) {
				return stub, nil
			},
		},
	}

	ctx := context.Background()
	result, err := reconciler.reconcileRegistration(ctx, claim)
	if err != nil {
		t.Fatalf("reconcileRegistration returned error: %v", err)
	}
	if result == nil {
		t.Fatalf("expected result, got nil")
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue after failure, got %#v", result)
	}

	if stub.deprovisionCount != 1 {
		t.Fatalf("expected deprovision to be called once, got %d", stub.deprovisionCount)
	}

	failedCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeFailed)
	if failedCond == nil || failedCond.Status != metav1.ConditionTrue || failedCond.Reason != "RegistrationTimeoutExceeded" {
		t.Fatalf("expected failed condition to be true with timeout exceeded, got %#v", failedCond)
	}

	if claim.Status.ProviderID != "" {
		t.Fatalf("expected providerID to be cleared on failure, got %q", claim.Status.ProviderID)
	}
}
