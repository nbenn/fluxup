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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/nbenn/fluxup/internal/logging"
	"github.com/nbenn/fluxup/internal/renovate"
)

const (
	// UpdatesConfigMapName is the name of the ConfigMap containing Renovate output
	UpdatesConfigMapName = "fluxup-updates"
	// UpdatesConfigMapNamespace is the namespace of the updates ConfigMap
	UpdatesConfigMapNamespace = "fluxup-system"
)

// UpdatesConfigMapReconciler reconciles the Renovate updates ConfigMap
type UpdatesConfigMapReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	StatusUpdater *renovate.StatusUpdater
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile processes the Renovate updates ConfigMap
func (r *UpdatesConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Only process our specific ConfigMap
	if req.Name != UpdatesConfigMapName || req.Namespace != UpdatesConfigMapNamespace {
		return ctrl.Result{}, nil
	}

	logger.Info("processing Renovate updates ConfigMap")
	if err := r.StatusUpdater.ProcessRenovateOutput(ctx); err != nil {
		logger.Error("failed to process Renovate output", "error", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UpdatesConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only watch ConfigMaps in the fluxup-system namespace with our specific name
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == UpdatesConfigMapName && obj.GetNamespace() == UpdatesConfigMapNamespace
		})).
		Named("updates-configmap").
		Complete(r)
}
