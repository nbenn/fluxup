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

package renovate

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/logging"
)

// StatusUpdater updates ManagedApp status based on Renovate output
type StatusUpdater struct {
	client client.Client
	parser *Parser
	mapper *Mapper
}

// NewStatusUpdater creates a new status updater
func NewStatusUpdater(c client.Client, namespace, configMapName string) *StatusUpdater {
	return &StatusUpdater{
		client: c,
		parser: NewParser(c, namespace, configMapName),
		mapper: NewMapper(c),
	}
}

// ProcessRenovateOutput parses Renovate output and updates ManagedApp statuses
func (u *StatusUpdater) ProcessRenovateOutput(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	// Parse updates from ConfigMap
	updates, err := u.parser.Parse(ctx)
	if err != nil {
		return err
	}

	if len(updates) == 0 {
		logger.Debug("no updates found in Renovate output")
		return nil
	}

	logger.Info("found updates in Renovate output", "count", len(updates))

	// Map updates to ManagedApps
	mapped, err := u.mapper.MapUpdatesToManagedApps(ctx, updates)
	if err != nil {
		return err
	}

	// Update each ManagedApp's status
	for appKey, versionInfo := range mapped {
		if err := u.updateManagedAppStatus(ctx, appKey, versionInfo); err != nil {
			logger.Error("failed to update ManagedApp status", "app", appKey, "error", err)
			continue
		}
		logger.Info("updated ManagedApp with available update", "app", appKey)
	}

	return nil
}

func (u *StatusUpdater) updateManagedAppStatus(ctx context.Context, key types.NamespacedName, version *fluxupv1alpha1.VersionInfo) error {
	var app fluxupv1alpha1.ManagedApp
	if err := u.client.Get(ctx, key, &app); err != nil {
		return err
	}

	// Set available update
	app.Status.AvailableUpdate = version

	// Set UpdateAvailable condition
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeUpdateAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             "UpdateDetected",
		Message:            "A newer version is available",
		ObservedGeneration: app.Generation,
	})

	return u.client.Status().Update(ctx, &app)
}

// ClearUpdateStatus removes the update available condition when no updates exist
func (u *StatusUpdater) ClearUpdateStatus(ctx context.Context, key types.NamespacedName) error {
	var app fluxupv1alpha1.ManagedApp
	if err := u.client.Get(ctx, key, &app); err != nil {
		return err
	}

	app.Status.AvailableUpdate = nil
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               fluxupv1alpha1.ConditionTypeUpdateAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             "NoUpdateAvailable",
		Message:            "Application is up to date",
		ObservedGeneration: app.Generation,
	})

	return u.client.Status().Update(ctx, &app)
}
