package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
						ProviderRef: "default-provider",
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
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("reconciling the created resource")
			controllerReconciler := &NodePoolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
