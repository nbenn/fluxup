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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

var _ = Describe("ManagedApp Controller", func() {
	Context("When reconciling a minimal ManagedApp", func() {
		const resourceName = "test-minimal"
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
						Name:      "apps",
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

		It("should set Ready=False when Kustomization is not found", func() {
			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
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
			Expect(readyCondition.Reason).To(Equal("WorkloadNotFound"))
		})
	})

	Context("When reconciling a ManagedApp with Deployment workload", func() {
		const (
			resourceName   = "test-deployment"
			deploymentName = "test-deploy"
		)
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the Deployment")
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, &appsv1.Deployment{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			}

			By("creating the ManagedApp resource")
			managedapp := &fluxupv1alpha1.ManagedApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.ManagedAppSpec{
					GitPath: "flux/apps/test/deployment.yaml",
					KustomizationRef: fluxupv1alpha1.ObjectReference{
						Name:      "apps",
						Namespace: "flux-system",
					},
					WorkloadRef: &fluxupv1alpha1.WorkloadReference{
						Kind: "Deployment",
						Name: deploymentName,
					},
				},
			}
			err = k8sClient.Get(ctx, typeNamespacedName, &fluxupv1alpha1.ManagedApp{})
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

			deploy := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, deploy)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deploy)).To(Succeed())
			}
		})

		It("should set Ready=False when Deployment has no ready replicas", func() {
			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
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
			Expect(readyCondition.Reason).To(Equal("WorkloadNotReady"))
		})

		It("should set Ready=True when Deployment has ready replicas", func() {
			By("Updating Deployment status to have ready replicas")
			var deploy appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, &deploy)).To(Succeed())
			deploy.Status.Replicas = 1
			deploy.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &deploy)).To(Succeed())

			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
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
			Expect(readyCondition.Reason).To(Equal("WorkloadReady"))
		})
	})

	Context("When ManagedApp references non-existent workload", func() {
		const resourceName = "test-missing-workload"
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the ManagedApp resource referencing non-existent Deployment")
			managedapp := &fluxupv1alpha1.ManagedApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: fluxupv1alpha1.ManagedAppSpec{
					GitPath: "flux/apps/test/deployment.yaml",
					KustomizationRef: fluxupv1alpha1.ObjectReference{
						Name:      "apps",
						Namespace: "flux-system",
					},
					WorkloadRef: &fluxupv1alpha1.WorkloadReference{
						Kind: "Deployment",
						Name: "non-existent-deployment",
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

		It("should set Ready=False with WorkloadNotFound reason", func() {
			By("Reconciling the resource")
			controllerReconciler := &ManagedAppReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Checking the status condition")
			var managedapp fluxupv1alpha1.ManagedApp
			Expect(k8sClient.Get(ctx, typeNamespacedName, &managedapp)).To(Succeed())

			readyCondition := meta.FindStatusCondition(managedapp.Status.Conditions, fluxupv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("WorkloadNotFound"))
		})
	})
})
