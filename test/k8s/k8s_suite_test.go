//go:build k8s

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

package k8s

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nbenn/fluxup/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	// Can be overridden via IMG environment variable.
	managerImage = "example.com/fluxup:v0.0.1"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

func init() {
	// Allow overriding the manager image via environment variable
	if img := os.Getenv("IMG"); img != "" {
		managerImage = img
	}
}

// TestK8s runs the k8s test suite to validate the controller in Kind.
func TestK8s(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting fluxup k8s test suite\n")
	RunSpecs(t, "K8s Suite")
}

var _ = BeforeSuite(func() {
	// When running with coverage, the Makefile already builds and loads the coverage image
	if os.Getenv("DEPLOY_COVERAGE") != "true" {
		By("building the manager image")
		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

		By("loading the manager image on Kind")
		err = utils.LoadImageToKindClusterWithName(managerImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")
	} else {
		By("skipping image build - coverage mode uses pre-built image")
	}

	setupCertManager()
})

var _ = AfterSuite(func() {
	teardownCertManager()
})

// setupCertManager installs CertManager if needed for webhook tests.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by this suite.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
