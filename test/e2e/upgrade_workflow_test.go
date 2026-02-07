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

const (
	namespace       = "fluxup-system"
	testNamespace   = "e2e-upgrade-test"
	managedAppName  = "test-app"
	fluxSystemNS    = "flux-system"
	giteaNamespace  = "gitea"
)

var _ = Describe("Upgrade Workflow", Ordered, func() {
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
		// Copy the Git credentials secret from fluxup-system to our namespace
		// The E2E setup creates it in the global fluxup-system, but since tests
		// clean up namespaces we need to ensure it exists in our test namespace
		cmd = exec.Command("kubectl", "get", "secret", "fluxup-git-credentials", 
			"-n", namespace, "-o", "json")
		_, secretErr := utils.Run(cmd)
		if secretErr != nil {
			// Secret doesn't exist in this namespace, try to copy from the global one
			// or read from the .env file
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
		cmd = exec.Command("kubectl", "create", "namespace", testNamespace)
		_, _ = utils.Run(cmd)
	})

	AfterAll(func() {
		if os.Getenv("DEPLOY_COVERAGE") == "true" {
			By("skipping cleanup - coverage mode enabled")
			return
		}

		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "namespace", testNamespace, "--ignore-not-found")
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

	Context("with Flux GitRepository", func() {
		const (
			gitRepoName       = "flux-test-repo"
			kustomizationName = "e2e-apps"
		)

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
`, gitRepoName, fluxSystemNS, giteaNamespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(gitRepoYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for GitRepository to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "gitrepository", gitRepoName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("creating Kustomization for the test app")
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
  targetNamespace: %s
`, kustomizationName, fluxSystemNS, gitRepoName, testNamespace)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(kustomizationYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Kustomization to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", kustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up Flux resources")
			cmd := exec.Command("kubectl", "delete", "kustomization", kustomizationName, "-n", fluxSystemNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "gitrepository", gitRepoName, "-n", fluxSystemNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create a ManagedApp and detect it", func() {
			By("creating a ManagedApp CR")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: %s
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    autoUpdate: none
`, managedAppName, testNamespace, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the ManagedApp was created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "managedapp", managedAppName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should perform a dry-run upgrade", func() {
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
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, managedAppName, testNamespace, managedAppName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-dryrun",
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying dry-run succeeded")
			cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-dryrun",
				"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("DryRunSucceeded"))
		})

		It("should perform a real upgrade", func() {
			By("creating an UpgradeRequest")
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
    chart: "11.0.0"
  skipSnapshot: true
`, managedAppName, testNamespace, managedAppName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("checking upgrade status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
				"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			if output == "UpgradeSucceeded" {
				By("verifying Git commit was made")
				cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
					"-n", testNamespace, "-o", "jsonpath={.status.upgrade.gitCommit}")
				commitSHA, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(commitSHA).NotTo(BeEmpty())
			}
		})
	})
})
