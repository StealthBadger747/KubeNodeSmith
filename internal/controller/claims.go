package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
)

// inflightSummary aggregates pending capacity tied up in non-Ready claims.
type inflightSummary struct {
	count    int
	cpuMilli int64
	memBytes int64
}

// countInflightClaims aggregates pending (non-Ready, non-deleted, recent) claims for a pool.
// Returns capacity expressed in millicores and bytes.
func countInflightClaims(pool *kubenodesmithv1alpha1.NodeSmithPool, claims *kubenodesmithv1alpha1.NodeSmithClaimList) inflightSummary {
	var summary inflightSummary
	if pool == nil || claims == nil {
		return summary
	}

	logger := log.Log.WithName("countInflightClaims").WithValues("pool", pool.Name)
	validSince := time.Now().Add(-15 * time.Minute)

	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.PoolRef != pool.Name {
			continue
		}
		if !claim.DeletionTimestamp.IsZero() {
			logger.V(1).Info("skipping inflight claim marked for deletion", "claim", claim.Name)
			continue
		}

		// Already Ready claims are real nodes, not inflight.
		readyCond := meta.FindStatusCondition(claim.Status.Conditions, kubenodesmithv1alpha1.ConditionTypeReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			continue
		}

		if !claim.CreationTimestamp.IsZero() && claim.CreationTimestamp.Time.Before(validSince) {
			logger.V(1).Info("skipping stale inflight claim", "claim", claim.Name, "created", claim.CreationTimestamp.Time)
			continue
		}

		summary.count++
		if req := claim.Spec.Requirements; req != nil {
			if req.CPUCores > 0 {
				summary.cpuMilli += req.CPUCores * 1000
			}
			if req.MemoryMiB > 0 {
				summary.memBytes += req.MemoryMiB * 1024 * 1024
			}
		}
	}
	return summary
}
