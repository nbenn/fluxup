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
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

// ManagedAppReconciler reconciles a ManagedApp object
type ManagedAppReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch

// Reconcile reconciles a ManagedApp resource
func (r *ManagedAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the ManagedApp
	var app fluxupv1alpha1.ManagedApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling ManagedApp", "gitPath", app.Spec.GitPath)

	// 2. Check workload health
	ready, err := r.checkWorkloadHealth(ctx, &app)
	if err != nil {
		// Set condition: workload not found, requeue with backoff
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "WorkloadNotFound",
			Message:            err.Error(),
			ObservedGeneration: app.Generation,
		})
		if updateErr := r.Status().Update(ctx, &app); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// 3. Update Ready condition
	status := metav1.ConditionFalse
	reason := "WorkloadNotReady"
	if ready {
		status = metav1.ConditionTrue
		reason = "WorkloadReady"
	}
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		ObservedGeneration: app.Generation,
	})

	// 4. Persist status
	if err := r.Status().Update(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Requeue after interval to re-sync
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// checkWorkloadHealth determines if the workload is healthy
func (r *ManagedAppReconciler) checkWorkloadHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
	if app.Spec.WorkloadRef == nil {
		// No explicit workload ref - just check the Kustomization is ready
		return r.checkKustomizationHealth(ctx, app)
	}

	ref := app.Spec.WorkloadRef
	ns := ref.Namespace
	if ns == "" {
		ns = app.Namespace
	}
	key := types.NamespacedName{Name: ref.Name, Namespace: ns}

	switch ref.Kind {
	case "HelmRelease":
		var hr helmv2.HelmRelease
		if err := r.Get(ctx, key, &hr); err != nil {
			return false, err
		}
		return isHelmReleaseReady(&hr), nil

	case "Deployment":
		var deploy appsv1.Deployment
		if err := r.Get(ctx, key, &deploy); err != nil {
			return false, err
		}
		if deploy.Spec.Replicas == nil {
			return deploy.Status.ReadyReplicas > 0, nil
		}
		return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil

	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := r.Get(ctx, key, &sts); err != nil {
			return false, err
		}
		if sts.Spec.Replicas == nil {
			return sts.Status.ReadyReplicas > 0, nil
		}
		return sts.Status.ReadyReplicas == *sts.Spec.Replicas, nil

	default:
		return false, fmt.Errorf("unsupported workload kind: %s", ref.Kind)
	}
}

func (r *ManagedAppReconciler) checkKustomizationHealth(ctx context.Context, app *fluxupv1alpha1.ManagedApp) (bool, error) {
	ref := app.Spec.KustomizationRef
	ns := ref.Namespace
	if ns == "" {
		ns = DefaultFluxNamespace
	}
	key := types.NamespacedName{Name: ref.Name, Namespace: ns}

	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, key, &ks); err != nil {
		return false, err
	}

	for _, cond := range ks.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

func isHelmReleaseReady(hr *helmv2.HelmRelease) bool {
	for _, cond := range hr.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// findManagedAppsForWorkload maps workload changes back to ManagedApps
func (r *ManagedAppReconciler) findManagedAppsForWorkload(ctx context.Context, obj client.Object) []reconcile.Request {
	var apps fluxupv1alpha1.ManagedAppList
	if err := r.List(ctx, &apps); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, app := range apps.Items {
		if app.Spec.WorkloadRef == nil {
			continue
		}
		ref := app.Spec.WorkloadRef
		ns := ref.Namespace
		if ns == "" {
			ns = app.Namespace
		}

		// Get the kind from the object
		gvk := obj.GetObjectKind().GroupVersionKind()
		objKind := gvk.Kind
		// If GVK is not set (common for typed objects), infer from type
		if objKind == "" {
			switch obj.(type) {
			case *appsv1.Deployment:
				objKind = "Deployment"
			case *appsv1.StatefulSet:
				objKind = "StatefulSet"
			case *helmv2.HelmRelease:
				objKind = "HelmRelease"
			}
		}

		if ref.Kind == objKind && ref.Name == obj.GetName() && ns == obj.GetNamespace() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      app.Name,
					Namespace: app.Namespace,
				},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ManagedAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fluxupv1alpha1.ManagedApp{}).
		Watches(&helmv2.HelmRelease{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.findManagedAppsForWorkload)).
		Named("managedapp").
		Complete(r)
}
