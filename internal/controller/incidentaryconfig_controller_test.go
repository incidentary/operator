/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	incidentaryv1alpha1 "github.com/incidentary/operator/api/v1alpha1"
)

var _ = Describe("IncidentaryConfig Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName    = "test-resource"
			testNamespace   = "default"
			secretName      = "incidentary-api-key"
			secretKey       = "api-key"
			testAPIKeyValue = "test-api-key-value"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: testNamespace,
		}

		// cleanupConfig removes the IncidentaryConfig test fixture between cases.
		cleanupConfig := func() {
			resource := &incidentaryv1alpha1.IncidentaryConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		}

		// cleanupSecret removes the API-key Secret between cases.
		cleanupSecret := func() {
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      secretName,
				Namespace: testNamespace,
			}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		}

		AfterEach(func() {
			cleanupConfig()
			cleanupSecret()
		})

		It("should set Ready condition when Secret exists", func() {
			By("creating the API-key Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					secretKey: []byte(testAPIKeyValue),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the IncidentaryConfig")
			config := &incidentaryv1alpha1.IncidentaryConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
					APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  secretKey,
					},
					WorkspaceID: "ws_envtest",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue at reconciliation interval")

			By("verifying Status.Phase is Running and Ready condition is True")
			refreshed := &incidentaryv1alpha1.IncidentaryConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, refreshed)).To(Succeed())
			Expect(refreshed.Status.Phase).To(Equal(PhaseRunning))
			Expect(refreshed.Status.LastReconciliation).NotTo(BeNil())

			ready := findCondition(refreshed.Status.Conditions, ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			Expect(ready.Reason).To(Equal(ReasonReconciled))
			Expect(ready.Message).To(ContainSubstring("informers watching"))
		})

		It("should set Ready condition to False when Secret is missing", func() {
			By("creating the IncidentaryConfig with a dangling Secret ref")
			config := &incidentaryv1alpha1.IncidentaryConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
					APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
						Name: "does-not-exist",
						Key:  secretKey,
					},
					WorkspaceID: "ws_envtest",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue to retry the Secret read")

			By("verifying the Ready condition is False with reason SecretNotFound")
			refreshed := &incidentaryv1alpha1.IncidentaryConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, refreshed)).To(Succeed())
			Expect(refreshed.Status.Phase).To(Equal(PhaseDegraded))

			ready := findCondition(refreshed.Status.Conditions, ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(ReasonSecretNotFound))
		})

		It("should set Ready condition to False when API key is empty", func() {
			By("creating a Secret with empty API key")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					secretKey: []byte(""),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the IncidentaryConfig")
			config := &incidentaryv1alpha1.IncidentaryConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
					APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  secretKey,
					},
					WorkspaceID: "ws_envtest",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Ready condition is False with reason InvalidAPIKey")
			refreshed := &incidentaryv1alpha1.IncidentaryConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, refreshed)).To(Succeed())
			Expect(refreshed.Status.Phase).To(Equal(PhaseDegraded))

			ready := findCondition(refreshed.Status.Conditions, ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(ReasonInvalidAPIKey))
		})

		It("should use the configured reconciliation interval for requeue", func() {
			By("creating the Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					secretKey: []byte(testAPIKeyValue),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the IncidentaryConfig with a custom interval")
			config := &incidentaryv1alpha1.IncidentaryConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
					APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  secretKey,
					},
					WorkspaceID:                   "ws_envtest",
					ReconciliationIntervalSeconds: 60,
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			By("reconciling and verifying the requeue interval matches the spec")
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter.Seconds()).To(BeNumerically("==", 60))
		})

		It("should apply the default reconciliation interval when spec omits it", func() {
			By("creating the Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					secretKey: []byte(testAPIKeyValue),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the IncidentaryConfig without setting the interval")
			config := &incidentaryv1alpha1.IncidentaryConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
					APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  secretKey,
					},
					WorkspaceID: "ws_envtest",
				},
			}
			Expect(k8sClient.Create(ctx, config)).To(Succeed())

			By("reconciling and verifying the default (300s) interval is used")
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter.Seconds()).To(BeNumerically("==", DefaultReconciliationIntervalSeconds))
		})

		It("should not error when the IncidentaryConfig does not exist", func() {
			controllerReconciler := &IncidentaryConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent",
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			// Sanity check: the resource really is absent.
			notFound := &incidentaryv1alpha1.IncidentaryConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "nonexistent",
				Namespace: testNamespace,
			}, notFound)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		// TODO(phase-3): add a test that increments Status.WatchedWorkloads
		// once the discovery loop is wired to the informer event stream.
	})
})

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
