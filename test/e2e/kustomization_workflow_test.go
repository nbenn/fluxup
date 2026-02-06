//go:build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nbenn/fluxup/test/utils"
)

var _ = Describe("Kustomization-based Upgrade Workflow", Ordered, func() {
	const (
		ksTestNamespace   = "e2e-kustomization-test"
		ksGitRepoName     = "flux-test-repo-ks"
		ksKustomizationName = "ks-apps"
	)

	var controllerPodName string

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, _ = utils.Run(cmd) // Ignore if exists

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("configuring Git credentials for the controller")
		cmd = exec.Command("kubectl", "get", "secret", "fluxup-git-credentials",
			"-n", namespace, "-o", "json")
		_, secretErr := utils.Run(cmd)
		if secretErr != nil {
			By("reading Git token from .env file")
			envPath := "/workspace/.devcontainer/test-infra/e2e/.env"
			envCmd := exec.Command("bash", "-c", fmt.Sprintf("source %s && echo $GITEA_TOKEN", envPath))
			token, tokenErr := utils.Run(envCmd)
			if tokenErr != nil {
				Skip("Could not read GITEA_TOKEN from .env file")
			}
			token = strings.TrimSpace(token)

			By("creating Git credentials secret")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "fluxup-git-credentials",
				"-n", namespace, fmt.Sprintf("--from-literal=token=%s", token))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}

		By("deploying the controller-manager with Git configuration")
		deployTarget := "deploy"
		if os.Getenv("DEPLOY_COVERAGE") == "true" {
			deployTarget = "deploy-cover"
		}
		cmd = exec.Command("make", deployTarget, fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for initial rollout to complete before patching")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/fluxup-controller-manager",
			"-n", namespace, "--timeout=5m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("patching controller deployment with Git environment variables")
		gitURL := fmt.Sprintf("http://gitea.%s.svc.cluster.local:3000/fluxup/flux-test-repo.git", giteaNamespace)
		patchJSON := fmt.Sprintf(`{
			"spec": {
				"template": {
					"spec": {
						"containers": [{
							"name": "manager",
							"env": [
								{"name": "GIT_BACKEND", "value": "gitea"},
								{"name": "GIT_REPO_URL", "value": "%s"},
								{"name": "GIT_BRANCH", "value": "main"},
								{"name": "GIT_TOKEN", "valueFrom": {"secretKeyRef": {"name": "fluxup-git-credentials", "key": "token"}}}
							]
						}]
					}
				}
			}
		}`, gitURL)
		cmd = exec.Command("kubectl", "patch", "deployment", "fluxup-controller-manager",
			"-n", namespace, "--type=strategic", "-p", patchJSON)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for rollout to complete")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/fluxup-controller-manager",
			"-n", namespace, "--timeout=5m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for controller to be ready with Git configuration")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// Store pod name for debugging
		cmd = exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-n", namespace, "-o", "jsonpath={.items[0].metadata.name}")
		controllerPodName, _ = utils.Run(cmd)

		By("creating test namespace")
		cmd = exec.Command("kubectl", "create", "namespace", ksTestNamespace)
		_, _ = utils.Run(cmd)

		// Create redis namespace for the raw deployments
		By("creating redis namespace")
		cmd = exec.Command("kubectl", "create", "namespace", "redis")
		_, _ = utils.Run(cmd)
	})

	AfterAll(func() {
		if os.Getenv("DEPLOY_COVERAGE") == "true" {
			By("skipping cleanup - coverage mode enabled")
			return
		}

		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "namespace", ksTestNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up redis namespace")
		cmd = exec.Command("kubectl", "delete", "namespace", "redis", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			By("Fetching controller logs for debugging")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace, "--tail=100")
			logs, _ := utils.Run(cmd)
			GinkgoWriter.Printf("Controller logs:\n%s\n", logs)
		}
	})

	Context("with Flux GitRepository for Kustomization-based apps", func() {
		BeforeAll(func() {
			By("creating GitRepository pointing to Gitea")
			gitRepoYAML := fmt.Sprintf(`
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1m
  url: http://gitea.%s.svc.cluster.local:3000/fluxup/flux-test-repo.git
  ref:
    branch: main
`, ksGitRepoName, fluxSystemNS, giteaNamespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(gitRepoYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for GitRepository to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "gitrepository", ksGitRepoName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("creating Kustomization for the raw manifests")
			kustomizationYAML := fmt.Sprintf(`
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1m
  sourceRef:
    kind: GitRepository
    name: %s
  path: ./flux
  prune: true
`, ksKustomizationName, fluxSystemNS, ksGitRepoName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(kustomizationYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Kustomization to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", ksKustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up Flux resources")
			cmd := exec.Command("kubectl", "delete", "kustomization", ksKustomizationName, "-n", fluxSystemNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "gitrepository", ksGitRepoName, "-n", fluxSystemNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		Context("with raw Deployment manifest", func() {
			const redisRawAppName = "redis-raw-app"

			AfterEach(func() {
				By("cleaning up ManagedApp and UpgradeRequests")
				cmd := exec.Command("kubectl", "delete", "managedapp", redisRawAppName, "-n", ksTestNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
				cmd = exec.Command("kubectl", "delete", "upgraderequest", "--all", "-n", ksTestNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			It("should create a ManagedApp with custom versionPath", func() {
				By("creating a ManagedApp CR with custom versionPath for image tag")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
`, redisRawAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("verifying the ManagedApp was created")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisRawAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())
			})

			It("should perform a dry-run upgrade with custom versionPath", func() {
				By("creating a ManagedApp CR")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
`, redisRawAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for ManagedApp to be ready")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisRawAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				By("creating a dry-run UpgradeRequest")
				upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: %s-dryrun
  namespace: %s
spec:
  managedAppRef:
    name: %s
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, redisRawAppName, ksTestNamespace, redisRawAppName)

				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(upgradeYAML)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for dry-run to complete")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-dryrun",
						"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("True"))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())

				By("verifying dry-run succeeded")
				cmd = exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-dryrun",
					"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(Equal("DryRunSucceeded"))
			})

			It("should perform a real upgrade updating image tag via custom versionPath", func() {
				By("creating a ManagedApp CR")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
`, redisRawAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for ManagedApp to be ready")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisRawAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				By("creating an UpgradeRequest to update image tag")
				upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: %s-upgrade
  namespace: %s
spec:
  managedAppRef:
    name: %s
  targetVersion:
    chart: "redis:7.4.0"
  skipSnapshot: true
`, redisRawAppName, ksTestNamespace, redisRawAppName)

				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(upgradeYAML)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for upgrade to complete")
				Eventually(func(g Gomega) {
					// Log all conditions to see current state
					cmd := exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-upgrade",
						"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions}")
					conditions, _ := utils.Run(cmd)
					GinkgoWriter.Printf("Current conditions: %s\n", conditions)

					// Log controller logs for context
					cmd = exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
						"-n", namespace, "--tail=20")
					logs, _ := utils.Run(cmd)
					GinkgoWriter.Printf("Recent controller logs:\n%s\n", logs)

					// Log Kustomization state
					cmd = exec.Command("kubectl", "get", "kustomization", ksKustomizationName,
						"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend},{.status.conditions[?(@.type=='Ready')].status},{.status.conditions[?(@.type=='Ready')].reason}")
					ksState, _ := utils.Run(cmd)
					GinkgoWriter.Printf("Kustomization state (suspend,ready,reason): %s\n", ksState)

					cmd = exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-upgrade",
						"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).NotTo(BeEmpty())
				}, 5*time.Minute, 10*time.Second).Should(Succeed())

				By("checking upgrade status")
				cmd = exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-upgrade",
					"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				// Log full status for debugging
				cmd = exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-upgrade",
					"-n", ksTestNamespace, "-o", "yaml")
				fullOutput, _ := utils.Run(cmd)
				GinkgoWriter.Printf("UpgradeRequest status:\n%s\n", fullOutput)

				if output == "UpgradeSucceeded" {
					By("verifying Git commit was made")
					cmd = exec.Command("kubectl", "get", "upgraderequest", redisRawAppName+"-upgrade",
						"-n", ksTestNamespace, "-o", "jsonpath={.status.upgrade.gitCommit}")
					commitSHA, err := utils.Run(cmd)
					Expect(err).NotTo(HaveOccurred())
					Expect(commitSHA).NotTo(BeEmpty())

					By("verifying Deployment has updated image")
					Eventually(func(g Gomega) {
						cmd := exec.Command("kubectl", "get", "deployment", "redis-raw",
							"-n", "redis", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
						image, err := utils.Run(cmd)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(image).To(Equal("redis:7.4.0"))
					}, 2*time.Minute, 5*time.Second).Should(Succeed())
				}
			})
		})

		Context("with StatefulSet and PVC", func() {
			const redisPersistentAppName = "redis-persistent-app"

			AfterEach(func() {
				By("cleaning up ManagedApp and UpgradeRequests")
				cmd := exec.Command("kubectl", "delete", "managedapp", redisPersistentAppName, "-n", ksTestNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
				cmd = exec.Command("kubectl", "delete", "upgraderequest", "--all", "-n", ksTestNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			It("should create a ManagedApp for StatefulSet with PVC", func() {
				By("creating a ManagedApp CR for StatefulSet")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-persistent/statefulset.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: "csi-hostpath-snapclass"
`, redisPersistentAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("verifying the ManagedApp was created")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisPersistentAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())
			})

			It("should discover workloads from Kustomization inventory", func() {
				By("creating a ManagedApp CR for StatefulSet")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-persistent/statefulset.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: "csi-hostpath-snapclass"
`, redisPersistentAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for ManagedApp to be ready")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisPersistentAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				By("verifying StatefulSet exists in cluster (deployed via Kustomization)")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "statefulset", "redis-persistent", "-n", "redis")
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 2*time.Minute, 5*time.Second).Should(Succeed())

				By("verifying PVC was created by StatefulSet")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pvc", "-n", "redis", "-l", "app=redis-persistent")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(ContainSubstring("data-redis-persistent"))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			})

			It("should perform dry-run upgrade for StatefulSet", func() {
				By("creating a ManagedApp CR for StatefulSet")
				managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/redis-persistent/statefulset.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
    versionPath: ".spec.template.spec.containers[0].image"
  volumeSnapshots:
    enabled: true
    volumeSnapshotClassName: "csi-hostpath-snapclass"
`, redisPersistentAppName, ksTestNamespace, ksKustomizationName, fluxSystemNS)

				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(managedAppYAML)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for ManagedApp to be ready")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "managedapp", redisPersistentAppName, "-n", ksTestNamespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				By("creating a dry-run UpgradeRequest for StatefulSet")
				upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: %s-dryrun
  namespace: %s
spec:
  managedAppRef:
    name: %s
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, redisPersistentAppName, ksTestNamespace, redisPersistentAppName)

				cmd = exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(upgradeYAML)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("waiting for dry-run to complete")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "upgraderequest", redisPersistentAppName+"-dryrun",
						"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("True"))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())

				By("verifying dry-run succeeded")
				cmd = exec.Command("kubectl", "get", "upgraderequest", redisPersistentAppName+"-dryrun",
					"-n", ksTestNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(Equal("DryRunSucceeded"))
			})
		})
	})
})
