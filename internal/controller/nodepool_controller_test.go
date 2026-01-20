package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	kubenodesmithv1alpha1 "github.com/StealthBadger747/KubeNodeSmith/api/v1alpha1"
)

var _ = Describe("NodePool controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		nodePool := &kubenodesmithv1alpha1.NodeSmithPool{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind NodeSmithPool")
			err := k8sClient.Get(ctx, typeNamespacedName, nodePool)
			if err != nil && errors.IsNotFound(err) {
				resource := &kubenodesmithv1alpha1.NodeSmithPool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: kubenodesmithv1alpha1.NodeSmithPoolSpec{
						ProviderRef:  "default-provider",
						PoolLabelKey: "topology.kubenodesmith.io/pool",
						Limits: kubenodesmithv1alpha1.NodePoolLimits{
							MinNodes:  1,
							MaxNodes:  5,
							CPUCores:  4,
							MemoryMiB: 8192,
						},
						MachineTemplate: kubenodesmithv1alpha1.MachineTemplate{
							Labels: map[string]string{
								"node-role.kubernetes.io/worker": "",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kubenodesmithv1alpha1.NodeSmithPool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("cleaning up the specific resource instance NodeSmithPool")
			controllerutil.RemoveFinalizer(resource, FinalizerNodeSmithPool)
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("reconciling the created resource")
			controllerReconciler := &NodePoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(32),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("refreshes availability without unschedulable pods", func() {
			By("creating a pool node with capacity")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pool-node-1",
					Labels: map[string]string{
						"topology.kubenodesmith.io/pool": resourceName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			node.Status.Capacity = corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			}
			node.Status.Allocatable = node.Status.Capacity
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			By("creating an inflight claim that would exceed the pool limit")
			claim := &kubenodesmithv1alpha1.NodeSmithClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pool-claim-1",
					Namespace: "default",
				},
				Spec: kubenodesmithv1alpha1.NodeSmithClaimSpec{
					PoolRef: resourceName,
					Requirements: &kubenodesmithv1alpha1.NodeSmithClaimRequirements{
						CPUCores:  1,
						MemoryMiB: 9000,
					},
				},
			}
			Expect(k8sClient.Create(ctx, claim)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, claim)
			})

			By("reconciling the pool to refresh status")
			controllerReconciler := &NodePoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(32),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			freshPool := &kubenodesmithv1alpha1.NodeSmithPool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, freshPool)).To(Succeed())

			cond := meta.FindStatusCondition(freshPool.Status.Conditions, "Available")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ResourceLimitReached"))
		})
	})
})
