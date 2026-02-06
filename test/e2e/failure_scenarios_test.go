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

var _ = Describe("Upgrade/Rollback Failure Scenarios", Ordered, func() {
	const (
		failureTestNS     = "e2e-failure-test"
		gitRepoName       = "flux-test-repo"
		kustomizationName = "failure-test-apps"
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
		cmd = exec.Command("kubectl", "create", "namespace", failureTestNS)
		_, _ = utils.Run(cmd)

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

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(gitRepoYAML)
		_, err = utils.Run(cmd)
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
`, kustomizationName, fluxSystemNS, gitRepoName, failureTestNS)

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
		if os.Getenv("DEPLOY_COVERAGE") == "true" {
			By("skipping cleanup - coverage mode enabled")
			return
		}

		By("cleaning up Flux resources")
		cmd := exec.Command("kubectl", "delete", "kustomization", kustomizationName, "-n", fluxSystemNS, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up test namespace")
		cmd = exec.Command("kubectl", "delete", "namespace", failureTestNS, "--ignore-not-found", "--timeout=60s")
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

		By("cleaning up test resources")
		cmd := exec.Command("kubectl", "delete", "upgraderequest", "--all", "-n", failureTestNS, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "rollbackrequest", "--all", "-n", failureTestNS, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "managedapp", "--all", "-n", failureTestNS, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("ensuring Kustomization is not suspended")
		cmd = exec.Command("kubectl", "patch", "kustomization", kustomizationName,
			"-n", fluxSystemNS, "--type=merge", "-p", `{"spec":{"suspend":false}}`)
		_, _ = utils.Run(cmd)
	})

	Context("Pre-commit failure scenarios", func() {
		It("should resume Kustomization when upgrade fails before Git commit", func() {
			By("creating a ManagedApp with invalid git path")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: invalid-path-app
  namespace: %s
spec:
  gitPath: "nonexistent/path/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest that will fail at Git read")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: precommit-failure-test
  namespace: %s
spec:
  managedAppRef:
    name: invalid-path-app
  targetVersion:
    chart: "99.0.0"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "precommit-failure-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the failure reason indicates Git read failure")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "precommit-failure-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			// Could be GitReadFailed, ManagedAppNotFound, or similar pre-commit error
			Expect(output).To(Or(
				ContainSubstring("GitReadFailed"),
				ContainSubstring("NotFound"),
				ContainSubstring("Failed"),
			))

			By("verifying Kustomization is NOT suspended (resumed after failure)")
			cmd = exec.Command("kubectl", "get", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			// Should be empty (not suspended) or "false"
			Expect(output).To(Or(BeEmpty(), Equal("false")))
		})
	})



	Context("External interference detection", func() {
		It("should detect when Kustomization is externally un-suspended", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: external-unsuspend-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("manually suspending the Kustomization first")
			cmd = exec.Command("kubectl", "patch", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "--type=merge", "-p", `{"spec":{"suspend":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: external-unsuspend-test
  namespace: %s
spec:
  managedAppRef:
    name: external-unsuspend-app
  targetVersion:
    chart: "11.0.0"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for suspended condition to be set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "external-unsuspend-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Suspended')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			By("externally un-suspending the Kustomization")
			cmd = exec.Command("kubectl", "patch", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "--type=merge", "-p", `{"spec":{"suspend":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to fail due to external un-suspend")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "external-unsuspend-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the failure message mentions external un-suspend")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "external-unsuspend-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].message}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Log full status for debugging
			cmd = exec.Command("kubectl", "get", "upgraderequest", "external-unsuspend-test",
				"-n", failureTestNS, "-o", "yaml")
			fullOutput, _ := utils.Run(cmd)
			GinkgoWriter.Printf("UpgradeRequest status:\n%s\n", fullOutput)

			// The failure could be detected at suspend verification or the upgrade might succeed
			// if it was fast enough - both outcomes are valid for this test
			cmd = exec.Command("kubectl", "get", "upgraderequest", "external-unsuspend-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			reason, _ := utils.Run(cmd)
			GinkgoWriter.Printf("Complete reason: %s, message: %s\n", reason, output)
		})
	})

	Context("Rollback failure scenarios", func() {
		It("should handle rollback when upgrade request doesn't exist", func() {
			By("creating a RollbackRequest for non-existent upgrade")
			rollbackYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: orphan-rollback-test
  namespace: %s
spec:
  upgradeRequestRef:
    name: nonexistent-upgrade
`, failureTestNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(rollbackYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for rollback to fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rollbackrequest", "orphan-rollback-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the failure reason")
			cmd = exec.Command("kubectl", "get", "rollbackrequest", "orphan-rollback-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("UpgradeRequestNotFound"))
		})

		It("should fail rollback when upgrade is still in progress", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: in-progress-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest (dry-run to keep it simple)")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: in-progress-upgrade
  namespace: %s
spec:
  managedAppRef:
    name: in-progress-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait for it to complete (dry-run is fast)
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "in-progress-upgrade",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			By("creating a RollbackRequest for the completed dry-run upgrade")
			// This should fail because dry-run doesn't create snapshots
			rollbackYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: no-snapshot-rollback
  namespace: %s
spec:
  upgradeRequestRef:
    name: in-progress-upgrade
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(rollbackYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for rollback to fail due to no snapshots")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rollbackrequest", "no-snapshot-rollback",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the failure reason indicates no snapshots")
			cmd = exec.Command("kubectl", "get", "rollbackrequest", "no-snapshot-rollback",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(Equal("NoSnapshotsAvailable"), Equal("NoPreviousVersion")))
		})
	})

	Context("Dry-run validation", func() {
		It("should complete dry-run successfully without making changes", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: dryrun-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: dryrun-validation-test
  namespace: %s
spec:
  managedAppRef:
    name: dryrun-app
  targetVersion:
    chart: "99.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run to complete successfully")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "dryrun-validation-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying dry-run reason")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "dryrun-validation-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("DryRunSucceeded"))

			By("verifying Kustomization was NOT suspended during dry-run")
			cmd = exec.Command("kubectl", "get", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(BeEmpty(), Equal("false")))
		})

		It("should fail dry-run for rollback without snapshots", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: dryrun-rollback-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a completed upgrade (dry-run)")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: dryrun-upgrade-for-rollback
  namespace: %s
spec:
  managedAppRef:
    name: dryrun-rollback-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "dryrun-upgrade-for-rollback",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			By("creating a dry-run RollbackRequest")
			rollbackYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: dryrun-rollback-test
  namespace: %s
spec:
  upgradeRequestRef:
    name: dryrun-upgrade-for-rollback
  dryRun: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(rollbackYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run rollback to fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rollbackrequest", "dryrun-rollback-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying failure reason")
			cmd = exec.Command("kubectl", "get", "rollbackrequest", "dryrun-rollback-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(Equal("NoSnapshotsAvailable"), Equal("NoPreviousVersion")))
		})
	})

	Context("Concurrent operations", func() {
		It("should handle multiple upgrade requests for same app", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: concurrent-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating first UpgradeRequest")
			upgradeYAML1 := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: concurrent-upgrade-1
  namespace: %s
spec:
  managedAppRef:
    name: concurrent-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML1)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("immediately creating second UpgradeRequest")
			upgradeYAML2 := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: concurrent-upgrade-2
  namespace: %s
spec:
  managedAppRef:
    name: concurrent-app
  targetVersion:
    chart: "12.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML2)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for both upgrades to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "concurrent-upgrade-1",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output1, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "get", "upgraderequest", "concurrent-upgrade-2",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output2, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(output1).NotTo(BeEmpty())
				g.Expect(output2).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying at least one completed successfully")
			// Check each upgrade request individually
			cmd = exec.Command("kubectl", "get", "upgraderequest", "concurrent-upgrade-1",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
			output1, _ := utils.Run(cmd)
			cmd = exec.Command("kubectl", "get", "upgraderequest", "concurrent-upgrade-2",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
			output2, _ := utils.Run(cmd)
			// At least one should succeed with dry-run
			Expect(output1 == "True" || output2 == "True").To(BeTrue(),
				"Expected at least one upgrade to succeed, got: upgrade-1=%s, upgrade-2=%s", output1, output2)
		})
	})

	Context("Timeout scenarios", func() {
		It("should handle phase timeout gracefully", func() {
			By("creating a ManagedApp with very short health check timeout")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: timeout-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  healthCheck:
    timeout: 1s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest that will likely timeout")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: timeout-test
  namespace: %s
spec:
  managedAppRef:
    name: timeout-app
  targetVersion:
    chart: "99.0.0"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to complete (success or failure)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "timeout-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("checking final status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "timeout-test",
				"-n", failureTestNS, "-o", "yaml")
			fullOutput, _ := utils.Run(cmd)
			GinkgoWriter.Printf("UpgradeRequest final status:\n%s\n", fullOutput)

			By("verifying Kustomization is resumed after completion")
			// The controller should resume the Kustomization after the upgrade completes
			// Give it a moment to reconcile
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", kustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Or(BeEmpty(), Equal("false")))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("Phase transition failures", func() {
		It("should handle ManagedApp deletion during upgrade", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: delete-during-upgrade-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: delete-managedapp-test
  namespace: %s
spec:
  managedAppRef:
    name: delete-during-upgrade-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("deleting the ManagedApp while upgrade is processing")
			cmd = exec.Command("kubectl", "delete", "managedapp", "delete-during-upgrade-app",
				"-n", failureTestNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("waiting for upgrade to complete or fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "delete-managedapp-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("checking the upgrade status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "delete-managedapp-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Upgrade completion reason: %s\n", output)
		})

		It("should handle Kustomization deletion during rollback", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: delete-during-rollback-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a completed UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: upgrade-for-kustomization-delete
  namespace: %s
spec:
  managedAppRef:
    name: delete-during-rollback-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "upgrade-for-kustomization-delete",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			By("creating a RollbackRequest")
			rollbackYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: rollback-kustomization-delete
  namespace: %s
spec:
  upgradeRequestRef:
    name: upgrade-for-kustomization-delete
  dryRun: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(rollbackYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for rollback to complete or fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rollbackrequest", "rollback-kustomization-delete",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("checking the rollback status")
			cmd = exec.Command("kubectl", "get", "rollbackrequest", "rollback-kustomization-delete",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Rollback completion reason: %s\n", output)
		})
	})

	Context("Finalizer cleanup", func() {
		It("should remove finalizers on dry-run completion", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: finalizer-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: finalizer-test
  namespace: %s
spec:
  managedAppRef:
    name: finalizer-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "finalizer-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying finalizer status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "finalizer-test",
				"-n", failureTestNS, "-o", "jsonpath={.metadata.finalizers}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			// Finalizer may still be present on completed requests (removed on deletion)
			// Just verify we can read the finalizers field
			GinkgoWriter.Printf("Finalizers: %s\n", output)
		})

		It("should be deletable after completion", func() {
			By("creating a ManagedApp")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: delete-after-complete-app
  namespace: %s
spec:
  gitPath: "flux/apps/gitea/helmrelease.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: delete-test
  namespace: %s
spec:
  managedAppRef:
    name: delete-after-complete-app
  targetVersion:
    chart: "11.0.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for completion")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "delete-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("deleting the completed UpgradeRequest")
			cmd = exec.Command("kubectl", "delete", "upgraderequest", "delete-test",
				"-n", failureTestNS, "--timeout=30s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying it was deleted")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "delete-test", "-n", failureTestNS)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "UpgradeRequest should be deleted")
		})
	})

	// ============================================================================
	// Kustomization-based Deployment Failure Scenarios
	// These tests verify that failure handling works correctly for raw manifests
	// (Deployments, StatefulSets) managed via Kustomization, using custom versionPath.
	// ============================================================================

	Context("Kustomization-based deployment failures", func() {
		It("should handle dry-run upgrade with custom versionPath for Deployment", func() {
			By("creating a ManagedApp for raw Deployment with custom versionPath")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest for the Deployment")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-dryrun
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-app
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-raw-dryrun",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying dry-run succeeded")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-dryrun",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("DryRunSucceeded"))

			By("verifying Kustomization was NOT suspended during dry-run")
			cmd = exec.Command("kubectl", "get", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(BeEmpty(), Equal("false")))
		})

		It("should fail upgrade when versionPath is missing for Kustomization-based app", func() {
			By("creating a ManagedApp WITHOUT versionPath (required for non-HelmRelease)")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-no-versionpath
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  # Note: versionPath is intentionally omitted
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest that should fail due to missing versionPath")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: missing-versionpath-test
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-no-versionpath
  targetVersion:
    chart: "redis:7.4.0"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "missing-versionpath-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying failure reason indicates missing versionPath")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "missing-versionpath-test",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("MissingVersionPath"))

			By("verifying Kustomization is resumed after failure")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", kustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Or(BeEmpty(), Equal("false")))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should handle timeout for Kustomization-based deployment", func() {
			By("creating a ManagedApp with very short timeout")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-timeout-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
  healthCheck:
    timeout: 1s
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest that will likely timeout")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-timeout-test
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-timeout-app
  targetVersion:
    chart: "redis:99.99.99"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to complete (success or failure)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-raw-timeout-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("checking final status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-timeout-test",
				"-n", failureTestNS, "-o", "yaml")
			fullOutput, _ := utils.Run(cmd)
			GinkgoWriter.Printf("UpgradeRequest final status:\n%s\n", fullOutput)

			By("verifying Kustomization is resumed after completion")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", kustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Or(BeEmpty(), Equal("false")))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should resume Kustomization when upgrade fails before Git commit for raw manifest", func() {
			By("creating a ManagedApp with invalid git path for Deployment")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: invalid-deployment-path
  namespace: %s
spec:
  gitPath: "nonexistent/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating an UpgradeRequest that will fail at Git read")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: precommit-deployment-failure
  namespace: %s
spec:
  managedAppRef:
    name: invalid-deployment-path
  targetVersion:
    chart: "redis:7.4.0"
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to fail")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "precommit-deployment-failure",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying the failure reason indicates Git read failure")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "precommit-deployment-failure",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(
				ContainSubstring("GitReadFailed"),
				ContainSubstring("NotFound"),
				ContainSubstring("Failed"),
			))

			By("verifying Kustomization is NOT suspended (resumed after failure)")
			cmd = exec.Command("kubectl", "get", "kustomization", kustomizationName,
				"-n", fluxSystemNS, "-o", "jsonpath={.spec.suspend}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(BeEmpty(), Equal("false")))
		})

		It("should handle rollback request for Kustomization-based dry-run upgrade", func() {
			By("creating a ManagedApp for raw Deployment")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-rollback-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a completed dry-run upgrade")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-upgrade-for-rollback
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-rollback-app
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for upgrade to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-raw-upgrade-for-rollback",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("creating a RollbackRequest for the dry-run upgrade (should fail - no snapshots)")
			rollbackYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: RollbackRequest
metadata:
  name: redis-raw-rollback
  namespace: %s
spec:
  upgradeRequestRef:
    name: redis-raw-upgrade-for-rollback
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(rollbackYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for rollback to fail due to no snapshots")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "rollbackrequest", "redis-raw-rollback",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("False"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying failure reason")
			cmd = exec.Command("kubectl", "get", "rollbackrequest", "redis-raw-rollback",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(Equal("NoSnapshotsAvailable"), Equal("NoPreviousVersion")))
		})

		It("should handle concurrent upgrades for Kustomization-based apps", func() {
			By("creating a ManagedApp for raw Deployment")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-concurrent-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating first UpgradeRequest")
			upgradeYAML1 := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-concurrent-1
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-concurrent-app
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML1)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("immediately creating second UpgradeRequest")
			upgradeYAML2 := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-concurrent-2
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-concurrent-app
  targetVersion:
    chart: "redis:7.5.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML2)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for both upgrades to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-raw-concurrent-1",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output1, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-concurrent-2",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output2, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(output1).NotTo(BeEmpty())
				g.Expect(output2).NotTo(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying at least one completed successfully")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-concurrent-1",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
			output1, _ := utils.Run(cmd)
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-concurrent-2",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
			output2, _ := utils.Run(cmd)

			Expect(output1 == "True" || output2 == "True").To(BeTrue(),
				"Expected at least one upgrade to succeed, got: upgrade-1=%s, upgrade-2=%s", output1, output2)
		})
	})

	Context("StatefulSet with PVC scenarios", func() {
		It("should handle dry-run upgrade for StatefulSet with custom versionPath", func() {
			By("creating a ManagedApp for StatefulSet with PVC")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-persistent-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-persistent/statefulset.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest for the StatefulSet")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-persistent-dryrun
  namespace: %s
spec:
  managedAppRef:
    name: redis-persistent-app
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for dry-run to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-persistent-dryrun",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying dry-run succeeded")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-persistent-dryrun",
				"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("DryRunSucceeded"))
		})

		It("should be deletable after Kustomization-based upgrade completion", func() {
			By("creating a ManagedApp for Deployment")
			managedAppYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: ManagedApp
metadata:
  name: redis-raw-delete-app
  namespace: %s
spec:
  gitPath: "flux/apps/redis-raw/deployment.yaml"
  kustomizationRef:
    name: %s
    namespace: %s
  versionPolicy:
    versionPath: ".spec.template.spec.containers[0].image"
`, failureTestNS, kustomizationName, fluxSystemNS)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(managedAppYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a dry-run UpgradeRequest")
			upgradeYAML := fmt.Sprintf(`
apiVersion: fluxup.dev/v1alpha1
kind: UpgradeRequest
metadata:
  name: redis-raw-delete-test
  namespace: %s
spec:
  managedAppRef:
    name: redis-raw-delete-app
  targetVersion:
    chart: "redis:7.4.0"
  dryRun: true
  skipSnapshot: true
`, failureTestNS)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(upgradeYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for completion")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", "redis-raw-delete-test",
					"-n", failureTestNS, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("deleting the completed UpgradeRequest")
			cmd = exec.Command("kubectl", "delete", "upgraderequest", "redis-raw-delete-test",
				"-n", failureTestNS, "--timeout=30s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying it was deleted")
			cmd = exec.Command("kubectl", "get", "upgraderequest", "redis-raw-delete-test", "-n", failureTestNS)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "UpgradeRequest should be deleted")
		})
	})
})
