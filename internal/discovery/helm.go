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

package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/logging"
)

// Workload kind constants
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
)

// helmReleaseData represents the decoded Helm release data from the secret.
type helmReleaseData struct {
	Manifest string `json:"manifest"`
}

// discoverPVCsFromHelmRelease discovers PVCs from a HelmRelease by:
// 1. Finding the Helm release secret (sh.helm.release.v1.<name>.v<N>)
// 2. Decoding the release data (base64 -> base64 -> gzip -> JSON)
// 3. Parsing the manifest to find all Deployments/StatefulSets
// 4. Finding Pods via ownerReferences
// 5. Extracting PVCs from pod volumes
func (d *Discoverer) discoverPVCsFromHelmRelease(ctx context.Context, app *fluxupv1alpha1.ManagedApp) ([]PVCInfo, error) {
	logger := logging.FromContext(ctx)

	if app.Spec.HelmReleaseRef == nil {
		return nil, fmt.Errorf("helmReleaseRef is not set")
	}

	// Determine namespace for the Helm release secret
	ns := app.Spec.HelmReleaseRef.Namespace
	if ns == "" {
		ns = app.Namespace
	}
	releaseName := app.Spec.HelmReleaseRef.Name

	logger.Debug("discovering PVCs from HelmRelease",
		"release", releaseName,
		"namespace", ns)

	// Find the latest Helm release secret
	secret, err := d.findHelmReleaseSecret(ctx, releaseName, ns)
	if err != nil {
		return nil, fmt.Errorf("finding Helm release secret: %w", err)
	}

	// Decode the release data to get the manifest
	manifest, err := d.decodeHelmReleaseSecret(secret)
	if err != nil {
		return nil, fmt.Errorf("decoding Helm release secret: %w", err)
	}

	// Parse the manifest to find workloads
	workloads := d.parseManifestForWorkloads(manifest)

	logger.Debug("found workloads in Helm manifest", "count", len(workloads))

	// Find PVCs used by these workloads
	return d.findPVCsForWorkloads(ctx, workloads, ns)
}

// findHelmReleaseSecret finds the latest Helm release secret for the given release name.
// Helm stores release data in secrets named sh.helm.release.v1.<name>.v<N> where N is the revision.
func (d *Discoverer) findHelmReleaseSecret(ctx context.Context, releaseName, namespace string) (*corev1.Secret, error) {
	// List all secrets with the Helm release prefix
	prefix := fmt.Sprintf("sh.helm.release.v1.%s.v", releaseName)

	var secrets corev1.SecretList
	if err := d.client.List(ctx, &secrets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}

	// Find the latest revision
	var latestSecret *corev1.Secret
	latestRevision := 0

	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if !strings.HasPrefix(secret.Name, prefix) {
			continue
		}

		// Extract revision number from the name
		revStr := strings.TrimPrefix(secret.Name, prefix)
		var rev int
		if _, err := fmt.Sscanf(revStr, "%d", &rev); err != nil {
			continue
		}

		if rev > latestRevision {
			latestRevision = rev
			latestSecret = secret
		}
	}

	if latestSecret == nil {
		return nil, fmt.Errorf("no Helm release secret found for %s in namespace %s", releaseName, namespace)
	}

	return latestSecret, nil
}

// decodeHelmReleaseSecret decodes the Helm release secret data.
// The data is stored as: base64(base64(gzip(json)))
func (d *Discoverer) decodeHelmReleaseSecret(secret *corev1.Secret) (string, error) {
	releaseData, ok := secret.Data["release"]
	if !ok {
		return "", fmt.Errorf("secret %s has no 'release' key", secret.Name)
	}

	// First base64 decode (the secret data is already base64 decoded by Kubernetes,
	// but Helm adds another layer of base64 encoding)
	decoded1, err := base64.StdEncoding.DecodeString(string(releaseData))
	if err != nil {
		return "", fmt.Errorf("first base64 decode: %w", err)
	}

	// Second base64 decode
	decoded2, err := base64.StdEncoding.DecodeString(string(decoded1))
	if err != nil {
		return "", fmt.Errorf("second base64 decode: %w", err)
	}

	// Gzip decompress
	gzReader, err := gzip.NewReader(bytes.NewReader(decoded2))
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	decompressed, err := io.ReadAll(gzReader)
	if err != nil {
		return "", fmt.Errorf("decompressing: %w", err)
	}

	// Parse JSON to extract manifest
	var data helmReleaseData
	if err := json.Unmarshal(decompressed, &data); err != nil {
		return "", fmt.Errorf("parsing JSON: %w", err)
	}

	return data.Manifest, nil
}

// splitYAMLDocuments returns an iterator over YAML documents separated by "---".
func splitYAMLDocuments(manifest string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for doc := range strings.SplitSeq(manifest, "\n---\n") {
			doc = strings.TrimSpace(doc)
			if doc != "" {
				if !yield(doc) {
					return
				}
			}
		}
	}
}

// parseManifestForWorkloads parses a YAML manifest and extracts Deployment and StatefulSet resources.
func (d *Discoverer) parseManifestForWorkloads(manifest string) []WorkloadInfo {
	var workloads []WorkloadInfo

	// Split manifest into individual documents
	for doc := range splitYAMLDocuments(manifest) {
		// Parse as generic map to check kind
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			// Skip unparseable documents
			continue
		}

		kind, ok := obj["kind"].(string)
		if !ok {
			continue
		}

		if kind != kindDeployment && kind != kindStatefulSet {
			continue
		}

		// Extract metadata
		metadata, ok := obj["metadata"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)

		if name != "" {
			workloads = append(workloads, WorkloadInfo{
				Kind:      kind,
				Name:      name,
				Namespace: namespace,
			})
		}
	}

	return workloads
}

// findPVCsForWorkloads finds all PVCs used by the given workloads.
// It does this by finding Pods owned by the workloads and extracting their PVC references.
func (d *Discoverer) findPVCsForWorkloads(ctx context.Context, workloads []WorkloadInfo, defaultNamespace string) ([]PVCInfo, error) {
	pvcSet := make(map[string]PVCInfo)

	for _, workload := range workloads {
		ns := workload.Namespace
		if ns == "" {
			ns = defaultNamespace
		}

		// Find pods owned by this workload
		pods, err := d.findPodsForWorkload(ctx, workload.Kind, workload.Name, ns)
		if err != nil {
			// Log but continue - workload might not have pods yet
			continue
		}

		// Extract PVC references from pods
		for _, pod := range pods {
			for _, vol := range pod.Spec.Volumes {
				if vol.PersistentVolumeClaim != nil {
					pvcName := vol.PersistentVolumeClaim.ClaimName

					// Get the PVC to retrieve its access modes
					var pvc corev1.PersistentVolumeClaim
					if err := d.client.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: ns}, &pvc); err != nil {
						// Skip if PVC doesn't exist
						continue
					}

					key := ns + "/" + pvcName
					if _, exists := pvcSet[key]; !exists {
						pvcSet[key] = PVCInfo{
							Name:        pvcName,
							Namespace:   ns,
							AccessModes: pvc.Spec.AccessModes,
						}
					}
				}
			}
		}
	}

	// Convert map to slice
	result := make([]PVCInfo, 0, len(pvcSet))
	for _, pvc := range pvcSet {
		result = append(result, pvc)
	}

	return result, nil
}

// findPodsForWorkload finds all Pods owned by a Deployment or StatefulSet.
func (d *Discoverer) findPodsForWorkload(ctx context.Context, kind, name, namespace string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	var result []corev1.Pod
	for _, pod := range pods.Items {
		// Check ownerReferences
		for _, owner := range pod.OwnerReferences {
			// For Deployments, pods are owned by ReplicaSets, which are owned by Deployments
			// For StatefulSets, pods are directly owned by the StatefulSet
			if kind == kindStatefulSet && owner.Kind == kindStatefulSet && owner.Name == name {
				result = append(result, pod)
				break
			}
			if kind == kindDeployment && owner.Kind == "ReplicaSet" {
				// Check if the ReplicaSet is owned by the Deployment
				if isReplicaSetOwnedByDeployment(owner.Name, name) {
					result = append(result, pod)
					break
				}
			}
		}
	}

	return result, nil
}

// isReplicaSetOwnedByDeployment checks if a ReplicaSet is owned by a specific Deployment.
// ReplicaSets created by Deployments are named <deployment-name>-<hash>.
func isReplicaSetOwnedByDeployment(rsName, deploymentName string) bool {
	return strings.HasPrefix(rsName, deploymentName+"-")
}
