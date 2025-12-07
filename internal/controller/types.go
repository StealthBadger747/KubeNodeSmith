package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
	"github.com/StealthBadger747/KubeNodeSmith/internal/provider"
)

// ProviderBuilder constructs a concrete provider implementation from a NodeSmithProvider spec.
type ProviderBuilder func(ctx context.Context, c client.Client, providerObj *kubenodesmithv1alpha1.NodeSmithProvider) (provider.Provider, error)
