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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Parser reads and parses Renovate output from a ConfigMap
type Parser struct {
	client    client.Client
	namespace string
	configMap string
}

// NewParser creates a new Renovate output parser
func NewParser(c client.Client, namespace, configMapName string) *Parser {
	return &Parser{
		client:    c,
		namespace: namespace,
		configMap: configMapName,
	}
}

// Parse reads the ConfigMap and extracts update info
func (p *Parser) Parse(ctx context.Context) ([]UpdateInfo, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: p.configMap, Namespace: p.namespace}
	if err := p.client.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("getting updates configmap: %w", err)
	}

	data, ok := cm.Data["updates.json"]
	if !ok || data == "" {
		return nil, nil // No updates
	}

	var entry RenovateLogEntry
	if err := json.Unmarshal([]byte(data), &entry); err != nil {
		return nil, fmt.Errorf("parsing updates json: %w", err)
	}

	if entry.Config == nil {
		return nil, nil
	}

	return p.extractUpdates(entry.Config), nil
}

func (p *Parser) extractUpdates(config *RenovateConfig) []UpdateInfo {
	var updates []UpdateInfo

	// Process Flux manager updates (HelmRelease chart versions)
	for _, pf := range config.Flux {
		for _, dep := range pf.Deps {
			if len(dep.Updates) > 0 {
				updates = append(updates, UpdateInfo{
					PackageFile:    pf.PackageFile,
					DependencyName: dep.DepName,
					CurrentVersion: dep.CurrentValue,
					NewVersion:     dep.Updates[0].NewValue,
					UpdateType:     dep.Updates[0].UpdateType,
					Datasource:     dep.Datasource,
				})
			}
		}
	}

	// Process helm-values manager updates (image tags in values)
	for _, pf := range config.HelmValues {
		for _, dep := range pf.Deps {
			if len(dep.Updates) > 0 {
				updates = append(updates, UpdateInfo{
					PackageFile:    pf.PackageFile,
					DependencyName: dep.DepName,
					CurrentVersion: dep.CurrentValue,
					NewVersion:     dep.Updates[0].NewValue,
					UpdateType:     dep.Updates[0].UpdateType,
					Datasource:     dep.Datasource,
				})
			}
		}
	}

	// Process regex manager updates (raw deployments with image tags)
	for _, pf := range config.Regex {
		for _, dep := range pf.Deps {
			if len(dep.Updates) > 0 {
				updates = append(updates, UpdateInfo{
					PackageFile:    pf.PackageFile,
					DependencyName: dep.DepName,
					CurrentVersion: dep.CurrentValue,
					NewVersion:     dep.Updates[0].NewValue,
					UpdateType:     dep.Updates[0].UpdateType,
					Datasource:     dep.Datasource,
				})
			}
		}
	}

	return updates
}
