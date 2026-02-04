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
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/health"
)

var _ = Describe("ManagedApp Controller", func() {
	Context("When Kustomization is not found", func() {
		const resourceName = "test-missing-ks"
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the ManagedApp resource")
			managedapp := &fluxupv1alpha1.ManagedApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.ManagedAppSpec{
					GitPath: "flux/apps/test/kustomization.yaml",
					KustomizationRef: fluxupv1alpha1.ObjectReference{
						Name:      "non-existent-ks",
						Namespace: "flux-system",
					},
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, &fluxupv1alpha1.ManagedApp{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, managedapp)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &fluxupv1alpha1.ManagedApp{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Ready=False with KustomizationNotFound reason", func() {
			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				HealthChecker: health.NewChecker(k8sClient),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute)) // Requeue with backoff

			By("Checking the status condition")
			var managedapp fluxupv1alpha1.ManagedApp
			Expect(k8sClient.Get(ctx, typeNamespacedName, &managedapp)).To(Succeed())

			readyCondition := meta.FindStatusCondition(managedapp.Status.Conditions, fluxupv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("KustomizationNotFound"))
		})
	})

	Context("When Kustomization exists but is not ready", func() {
		const (
			resourceName = "test-ks-not-ready"
			ksName       = "test-ks"
		)
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		ksNamespacedName := types.NamespacedName{
			Name:      ksName,
			Namespace: "flux-system",
		}

		BeforeEach(func() {
			By("creating the flux-system namespace if needed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-system"}}
			nsErr := k8sClient.Get(ctx, types.NamespacedName{Name: "flux-system"}, &corev1.Namespace{})
			if errors.IsNotFound(nsErr) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating a Kustomization that is not ready")
			ks := &kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ksName,
					Namespace: "flux-system",
				},
				Spec: kustomizev1.KustomizationSpec{
					SourceRef: kustomizev1.CrossNamespaceSourceReference{
						Kind: "GitRepository",
						Name: "flux-system",
					},
					Path:     "./apps",
					Interval: metav1.Duration{Duration: 5 * time.Minute},
				},
			}
			ksErr := k8sClient.Get(ctx, ksNamespacedName, &kustomizev1.Kustomization{})
			if errors.IsNotFound(ksErr) {
				Expect(k8sClient.Create(ctx, ks)).To(Succeed())
			}

			By("creating the ManagedApp resource")
			managedapp := &fluxupv1alpha1.ManagedApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.ManagedAppSpec{
					GitPath: "flux/apps/test/kustomization.yaml",
					KustomizationRef: fluxupv1alpha1.ObjectReference{
						Name:      ksName,
						Namespace: "flux-system",
					},
				},
			}
			appErr := k8sClient.Get(ctx, typeNamespacedName, &fluxupv1alpha1.ManagedApp{})
			if errors.IsNotFound(appErr) {
				Expect(k8sClient.Create(ctx, managedapp)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &fluxupv1alpha1.ManagedApp{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			ks := &kustomizev1.Kustomization{}
			err = k8sClient.Get(ctx, ksNamespacedName, ks)
			if err == nil {
				Expect(k8sClient.Delete(ctx, ks)).To(Succeed())
			}
		})

		It("should set Ready=False when Kustomization is not ready", func() {
			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				HealthChecker: health.NewChecker(k8sClient),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status condition")
			var managedapp fluxupv1alpha1.ManagedApp
			Expect(k8sClient.Get(ctx, typeNamespacedName, &managedapp)).To(Succeed())

			readyCondition := meta.FindStatusCondition(managedapp.Status.Conditions, fluxupv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("KustomizationNotReady"))
		})

		It("should set Ready=True when Kustomization becomes ready", func() {
			By("Setting the Kustomization to Ready")
			var ks kustomizev1.Kustomization
			Expect(k8sClient.Get(ctx, ksNamespacedName, &ks)).To(Succeed())
			ks.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "ReconciliationSucceeded",
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, &ks)).To(Succeed())

			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				HealthChecker: health.NewChecker(k8sClient),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status condition")
			var managedapp fluxupv1alpha1.ManagedApp
			Expect(k8sClient.Get(ctx, typeNamespacedName, &managedapp)).To(Succeed())

			readyCondition := meta.FindStatusCondition(managedapp.Status.Conditions, fluxupv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal("KustomizationReady"))
		})
	})
})
