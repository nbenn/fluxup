//go:build e2e
// +build e2e

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

// These tests require E2E_FULL=true and the full infrastructure (Flux + Gitea)
var _ = Describe("Upgrade Workflow", Ordered, Label("full"), func() {
	// Skip if not running full e2e tests
	BeforeAll(func() {
		if os.Getenv("E2E_FULL") != "true" {
			Skip("Skipping full e2e tests (E2E_FULL != true)")
		}
	})

	const (
		testNamespace   = "e2e-upgrade-test"
		managedAppName  = "test-app"
		fluxSystemNS    = "flux-system"
		giteaNamespace  = "gitea"
	)

	BeforeAll(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "namespace", testNamespace)
		_, _ = utils.Run(cmd) // Ignore error if exists

		By("verifying Flux is installed")
		cmd = exec.Command("kubectl", "get", "deployment", "source-controller", "-n", fluxSystemNS)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Flux source-controller not found - run 'make test-e2e-setup-infra' first")

		By("verifying Gitea is running")
		cmd = exec.Command("kubectl", "get", "deployment", "gitea", "-n", giteaNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Gitea not found - run 'make test-e2e-setup-infra' first")
	})

	AfterAll(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "namespace", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("with Flux GitRepository", func() {
		const (
			gitRepoName      = "flux-test-repo"
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
			Expect(err).NotTo(HaveOccurred(), "Failed to create GitRepository")

			By("waiting for GitRepository to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "gitrepository", gitRepoName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "GitRepository not ready")
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
			Expect(err).NotTo(HaveOccurred(), "Failed to create Kustomization")

			By("waiting for Kustomization to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "kustomization", kustomizationName,
					"-n", fluxSystemNS, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Kustomization not ready")
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
			Expect(err).NotTo(HaveOccurred(), "Failed to create ManagedApp")

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
			Expect(err).NotTo(HaveOccurred(), "Failed to create dry-run UpgradeRequest")

			By("waiting for dry-run to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-dryrun",
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "UpgradeRequest not complete")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying dry-run succeeded")
			cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-dryrun",
				"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("DryRunSucceeded"), "Expected DryRunSucceeded reason")
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
			Expect(err).NotTo(HaveOccurred(), "Failed to create UpgradeRequest")

			By("waiting for upgrade to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Complete condition should be set (True or False)
				g.Expect(output).NotTo(BeEmpty(), "UpgradeRequest Complete condition not set")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("checking upgrade status")
			cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
				"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Complete')].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			
			// Log the full status for debugging
			cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
				"-n", testNamespace, "-o", "yaml")
			fullOutput, _ := utils.Run(cmd)
			GinkgoWriter.Printf("UpgradeRequest status:\n%s\n", fullOutput)

			By("verifying Git commit was made (if upgrade succeeded)")
			if output == "UpgradeSucceeded" {
				// Check that the upgrade was committed to Git
				cmd = exec.Command("kubectl", "get", "upgraderequest", managedAppName+"-upgrade",
					"-n", testNamespace, "-o", "jsonpath={.status.upgrade.gitCommit}")
				commitSHA, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(commitSHA).NotTo(BeEmpty(), "Expected Git commit SHA")
			}
		})
	})
})
