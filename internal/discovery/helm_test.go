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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
)

// encodeHelmRelease encodes a manifest in the Helm release format:
// base64(base64(gzip(json)))
func encodeHelmRelease(manifest string) []byte {
	// Create the release data JSON
	data := helmReleaseData{Manifest: manifest}
	jsonData, _ := json.Marshal(data)

	// Gzip compress
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	_, _ = gzWriter.Write(jsonData)
	_ = gzWriter.Close()

	// Double base64 encode
	encoded1 := base64.StdEncoding.EncodeToString(buf.Bytes())
	encoded2 := base64.StdEncoding.EncodeToString([]byte(encoded1))

	return []byte(encoded2)
}

func TestDecodeHelmReleaseSecret(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  replicas: 1`

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.test-release.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease(manifest),
		},
	}

	c := setupTestClient(t, secret)
	d := New(c)

	decoded, err := d.decodeHelmReleaseSecret(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if decoded != manifest {
		t.Errorf("decoded manifest mismatch:\ngot: %s\nwant: %s", decoded, manifest)
	}
}

func TestDecodeHelmReleaseSecret_MissingReleaseKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.test-release.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"other": []byte("data"),
		},
	}

	c := setupTestClient(t)
	d := New(c)

	_, err := d.decodeHelmReleaseSecret(secret)
	if err == nil {
		t.Fatal("expected error for missing 'release' key")
	}
}

func TestFindHelmReleaseSecret(t *testing.T) {
	// Create multiple versions of a Helm release secret
	secretV1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.my-app.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease("v1 manifest"),
		},
	}
	secretV2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.my-app.v2",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease("v2 manifest"),
		},
	}
	secretV3 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.my-app.v3",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease("v3 manifest"),
		},
	}

	c := setupTestClient(t, secretV1, secretV2, secretV3)
	d := New(c)

	secret, err := d.findHelmReleaseSecret(context.Background(), "my-app", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return the latest version (v3)
	if secret.Name != "sh.helm.release.v1.my-app.v3" {
		t.Errorf("expected v3 secret, got %s", secret.Name)
	}
}

func TestFindHelmReleaseSecret_NotFound(t *testing.T) {
	c := setupTestClient(t) // No secrets
	d := New(c)

	_, err := d.findHelmReleaseSecret(context.Background(), "nonexistent", "default")
	if err == nil {
		t.Fatal("expected error for nonexistent release")
	}
}

func TestParseManifestForWorkloads(t *testing.T) {
	manifest := `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: myapp
spec:
  replicas: 2
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: myapp
spec:
  replicas: 3
---
apiVersion: v1
kind: Service
metadata:
  name: web-svc
spec:
  ports:
  - port: 80
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data:
  key: value`

	c := setupTestClient(t)
	d := New(c)

	workloads := d.parseManifestForWorkloads(manifest)

	if len(workloads) != 2 {
		t.Errorf("expected 2 workloads (Deployment and StatefulSet), got %d", len(workloads))
	}

	// Check workload details
	kinds := make(map[string]string)
	for _, w := range workloads {
		kinds[w.Kind] = w.Name
	}

	if kinds["Deployment"] != "web" {
		t.Errorf("expected Deployment 'web', got %s", kinds["Deployment"])
	}
	if kinds["StatefulSet"] != "db" {
		t.Errorf("expected StatefulSet 'db', got %s", kinds["StatefulSet"])
	}
}

func TestParseManifestForWorkloads_EmptyManifest(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	workloads := d.parseManifestForWorkloads("")
	if len(workloads) != 0 {
		t.Errorf("expected 0 workloads for empty manifest, got %d", len(workloads))
	}
}

func TestParseManifestForWorkloads_InvalidYAML(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	// Invalid YAML should be skipped
	manifest := `not: valid: yaml: here
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: valid-deploy`

	workloads := d.parseManifestForWorkloads(manifest)

	// Should still find the valid deployment
	if len(workloads) != 1 {
		t.Errorf("expected 1 valid workload, got %d", len(workloads))
	}
}

func TestDiscoverPVCsFromHelmRelease(t *testing.T) {
	manifest := `---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: my-sts
  namespace: default
spec:
  replicas: 1`

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.my-app.v1",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"release": encodeHelmRelease(manifest),
		},
	}

	// Create a pod owned by the StatefulSet with a PVC
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sts-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts"},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-my-sts-0",
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-my-sts-0",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	c := setupTestClient(t, secret, pod, pvc)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: fluxupv1alpha1.ManagedAppSpec{
			HelmReleaseRef: &fluxupv1alpha1.ObjectReference{
				Name:      "my-app",
				Namespace: "default",
			},
		},
	}

	pvcs, err := d.discoverPVCsFromHelmRelease(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pvcs) != 1 {
		t.Errorf("expected 1 PVC, got %d", len(pvcs))
	}
	if pvcs[0].Name != "data-my-sts-0" {
		t.Errorf("expected PVC 'data-my-sts-0', got %s", pvcs[0].Name)
	}
}

func TestDiscoverPVCsFromHelmRelease_NoHelmReleaseRef(t *testing.T) {
	c := setupTestClient(t)
	d := New(c)

	app := &fluxupv1alpha1.ManagedApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		// No HelmReleaseRef
	}

	_, err := d.discoverPVCsFromHelmRelease(context.Background(), app)
	if err == nil {
		t.Fatal("expected error when HelmReleaseRef is not set")
	}
}

func TestIsReplicaSetOwnedByDeployment(t *testing.T) {
	tests := []struct {
		rsName         string
		deploymentName string
		want           bool
	}{
		{"my-deploy-abc123def", "my-deploy", true},
		{"my-deploy-abc123def", "other-deploy", false},
		{"frontend-app-abc123", "frontend-app", true},
		{"frontend-abc123xyz", "frontend", true}, // prefix match works
		{"unrelated-abc123", "frontend", false},
	}

	for _, tt := range tests {
		t.Run(tt.rsName, func(t *testing.T) {
			got := isReplicaSetOwnedByDeployment(tt.rsName, tt.deploymentName)
			if got != tt.want {
				t.Errorf("isReplicaSetOwnedByDeployment(%q, %q) = %v, want %v",
					tt.rsName, tt.deploymentName, got, tt.want)
			}
		})
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	manifest := `---
doc1: value1
---
doc2: value2
---
doc3: value3`

	docs := make([]string, 0, 3)
	for doc := range splitYAMLDocuments(manifest) {
		docs = append(docs, doc)
	}

	if len(docs) != 3 {
		t.Errorf("expected 3 documents, got %d", len(docs))
	}
}

func TestSplitYAMLDocuments_EmptyDocs(t *testing.T) {
	manifest := `---
doc1: value1
---

---
doc2: value2`

	docs := make([]string, 0, 2)
	for doc := range splitYAMLDocuments(manifest) {
		docs = append(docs, doc)
	}

	// Empty document should be skipped
	if len(docs) != 2 {
		t.Errorf("expected 2 non-empty documents, got %d", len(docs))
	}
}
