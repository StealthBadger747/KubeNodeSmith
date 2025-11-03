package controller

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
)

// countInflightClaims returns the number of relevant pending claims tied to the provided pool, along with
// aggregate compute and memory requested by those claims (expressed in millicores and bytes respectively).
func countInflightClaims(pool *kubenodesmithv1alpha1.NodeSmithPool, claims *kubenodesmithv1alpha1.NodeSmithClaimList) (int, int64, int64, error) {
	if pool == nil {
		return 0, 0, 0, fmt.Errorf("pool cannot be nil when counting inflight claims")
	}
	if claims == nil {
		return 0, 0, 0, nil
	}

	logger := log.Log.WithName("countInflightClaims").WithValues("pool", pool.Name)

	validSince := time.Now().Add(-15 * time.Minute)

	var (
		pendingCount    int
		pendingCPUMilli int64
		pendingMemBytes int64
	)

	for i := range claims.Items {
		claim := claims.Items[i]
		if claim.Spec.PoolRef != pool.Name {
			continue
		}
		if !claim.ObjectMeta.DeletionTimestamp.IsZero() {
			logger.V(1).Info("skipping inflight claim marked for deletion", "claim", claim.Name)
			continue
		}

		// Skip claims that are already Ready (fully provisioned).
		// We count claims that are pending or in-progress (not yet Ready).
		readyCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			// Claim is ready, skip it (it's counted as an actual node, not inflight)
			continue
		}

		if !claim.CreationTimestamp.IsZero() && claim.CreationTimestamp.Time.Before(validSince) {
			logger.V(1).Info("skipping stale inflight claim", "claim", claim.Name, "created", claim.CreationTimestamp.Time)
			continue
		}

		pendingCount++

		if req := claim.Spec.Requirements; req != nil {
			if req.CPUCores > 0 {
				pendingCPUMilli += req.CPUCores * 1000
			}
			if req.MemoryMiB > 0 {
				pendingMemBytes += req.MemoryMiB * 1024 * 1024
			}
		}
	}

	return pendingCount, pendingCPUMilli, pendingMemBytes, nil
}
