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
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/health"
	"github.com/nbenn/fluxup/internal/logging"
)

// HealthCheckInterval is the polling interval for ManagedApp health checks.
// Reduced from 5 minutes to 30 seconds for better dashboard freshness.
const HealthCheckInterval = 30 * time.Second

// ManagedAppReconciler reconciles a ManagedApp object
type ManagedAppReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	HealthChecker *health.Checker
}

// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fluxup.dev,resources=managedapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile reconciles a ManagedApp resource
func (r *ManagedAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// 1. Fetch the ManagedApp
	var app fluxupv1alpha1.ManagedApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("reconciling ManagedApp", "gitPath", app.Spec.GitPath)

	// 2. Perform health check using shared health checker
	result, err := r.HealthChecker.CheckHealth(ctx, &app)
	if err != nil {
		// Determine specific reason from error
		reason := "HealthCheckError"
		if errors.IsNotFound(err) && strings.Contains(err.Error(), "Flux resource") {
			reason = "KustomizationNotFound"
		}

		// Set condition: health check error
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               fluxupv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            err.Error(),
			ObservedGeneration: app.Generation,
		})
		if updateErr := r.Status().Update(ctx, &app); updateErr != nil {
			logger.Error("failed to update status", "error", updateErr)
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// 3. Update Ready condition based on health check result
	status := metav1.ConditionFalse
	reason := reasonNotReady

	if result.Healthy {
		status = metav1.ConditionTrue
		reason = "KustomizationReady"
	} else if !result.FluxReady {
		reason = "KustomizationNotReady"
	} else if !result.WorkloadsReady {
		reason = "WorkloadsNotReady"
	}

	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            result.Message,
		ObservedGeneration: app.Generation,
	})

	// 4. Persist status
	if err := r.Status().Update(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Requeue after interval to re-sync
	return ctrl.Result{RequeueAfter: HealthCheckInterval}, nil
}

// findManagedAppsForWorkload maps workload changes back to ManagedApps.
// With workload auto-discovery, we can't easily map workload changes back to ManagedApps
// without caching the discovery results. For now, return empty - the periodic reconcile
// will catch workload status changes.
func (r *ManagedAppReconciler) findManagedAppsForWorkload(_ context.Context, _ client.Object) []reconcile.Request {
	return nil
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
