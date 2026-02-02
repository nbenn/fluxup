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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

// Mapper maps Renovate updates to ManagedApp resources
type Mapper struct {
	client client.Client
}

// NewMapper creates a new mapper
func NewMapper(c client.Client) *Mapper {
	return &Mapper{client: c}
}

// MappedUpdate contains the update info mapped to a ManagedApp
type MappedUpdate struct {
	AppKey  types.NamespacedName
	Version *fluxupv1alpha1.VersionInfo
}

// MapUpdatesToManagedApps matches Renovate updates to ManagedApp resources
func (m *Mapper) MapUpdatesToManagedApps(ctx context.Context, updates []UpdateInfo) (map[types.NamespacedName]*fluxupv1alpha1.VersionInfo, error) {
	// Fetch all ManagedApps
	var apps fluxupv1alpha1.ManagedAppList
	if err := m.client.List(ctx, &apps); err != nil {
		return nil, err
	}

	// Build lookup map: gitPath -> ManagedApp key
	pathToApp := make(map[string]types.NamespacedName)
	for _, app := range apps.Items {
		if app.Spec.GitPath != "" {
			pathToApp[app.Spec.GitPath] = types.NamespacedName{
				Name:      app.Name,
				Namespace: app.Namespace,
			}
		}
	}

	result := make(map[types.NamespacedName]*fluxupv1alpha1.VersionInfo)

	for _, update := range updates {
		appKey, ok := pathToApp[update.PackageFile]
		if !ok {
			continue // No ManagedApp for this file
		}

		if result[appKey] == nil {
			result[appKey] = &fluxupv1alpha1.VersionInfo{}
		}

		switch update.Datasource {
		case "helm":
			result[appKey].Chart = update.NewVersion
		case "docker":
			result[appKey].Images = append(result[appKey].Images, fluxupv1alpha1.ImageInfo{
				Name: update.DependencyName,
				Tag:  update.NewVersion,
			})
		}
	}

	return result, nil
}
