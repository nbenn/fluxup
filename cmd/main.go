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

package main

import (
	"crypto/tls"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime/coverage"
	"syscall"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	fluxupv1alpha1 "github.com/nbenn/fluxup/api/v1alpha1"
	"github.com/nbenn/fluxup/internal/controller"
	"github.com/nbenn/fluxup/internal/flux"
	"github.com/nbenn/fluxup/internal/git"
	"github.com/nbenn/fluxup/internal/logging"
	"github.com/nbenn/fluxup/internal/renovate"
	"github.com/nbenn/fluxup/internal/snapshot"
	yamlpkg "github.com/nbenn/fluxup/internal/yaml"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

// setupCoverageSignalHandler sets up a signal handler to write coverage data
// when SIGUSR1 is received. This enables coverage collection for e2e tests
// where the binary runs in a container. The coverage data is written to the
// directory specified by GOCOVERDIR environment variable.
func setupCoverageSignalHandler() {
	coverDir, exists := os.LookupEnv("GOCOVERDIR")
	if !exists {
		return
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	go func() {
		for range c {
			_, _ = os.Stderr.WriteString("coverage: received SIGUSR1, writing coverage data to " + coverDir + "\n")
			// WriteCounters writes both meta and counter data needed for coverage
			if err := coverage.WriteCountersDir(coverDir); err != nil {
				_, _ = os.Stderr.WriteString("coverage: failed to write counters: " + err.Error() + "\n")
			} else {
				_, _ = os.Stderr.WriteString("coverage: successfully wrote coverage data\n")
			}
		}
	}()
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(fluxupv1alpha1.AddToScheme(scheme))
	utilruntime.Must(helmv2.AddToScheme(scheme))
	utilruntime.Must(kustomizev1.AddToScheme(scheme))
	utilruntime.Must(snapshotv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	// Set up coverage signal handler for e2e tests (no-op if GOCOVERDIR not set)
	setupCoverageSignalHandler()
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	// Git configuration flags
	var gitBackend string
	var gitRepoURL string
	var gitBranch string

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// Git configuration flags
	flag.StringVar(&gitBackend, "git-backend", "",
		"Git backend type (gitea, github, gitlab). Can also be set via GIT_BACKEND env var.")
	flag.StringVar(&gitRepoURL, "git-repo-url", "",
		"Git repository URL. Can also be set via GIT_REPO_URL env var.")
	flag.StringVar(&gitBranch, "git-branch", "main", "Git branch to operate on. Can also be set via GIT_BRANCH env var.")

	// Logging configuration flags
	var logLevel string
	var developmentMode bool
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.BoolVar(&developmentMode, "development", true, "Enable development mode (text output, debug level)")
	flag.Parse()

	// Parse log level
	var level slog.Level
	switch logLevel {
	case "debug":
		level = logging.LevelDebug
	case "info":
		level = logging.LevelInfo
	case "warn":
		level = logging.LevelWarn
	case "error":
		level = logging.LevelError
	default:
		level = logging.LevelInfo
	}

	// Setup slog-based logging
	logger := logging.NewLogger(logging.Options{
		Level:       level,
		Development: developmentMode,
	})
	ctrl.SetLogger(logging.NewLogrLogger(logger))

	// Create setup logger for startup messages
	setupLog := logging.WithName(logger, "setup")

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "fae8bd14.fluxup.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error("unable to start manager", "error", err)
		os.Exit(1)
	}

	if err := (&controller.ManagedAppReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error("unable to create controller", "controller", "ManagedApp", "error", err)
		os.Exit(1)
	}

	// Setup Renovate updates controller
	statusUpdater := renovate.NewStatusUpdater(
		mgr.GetClient(),
		controller.UpdatesConfigMapNamespace,
		controller.UpdatesConfigMapName,
	)
	if err := (&controller.UpdatesConfigMapReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		StatusUpdater: statusUpdater,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error("unable to create controller", "controller", "UpdatesConfigMap", "error", err)
		os.Exit(1)
	}

	// Setup UpgradeRequest controller (Phase 2)
	// Git configuration from flags or environment
	if gitBackend == "" {
		gitBackend = os.Getenv("GIT_BACKEND")
	}
	if gitRepoURL == "" {
		gitRepoURL = os.Getenv("GIT_REPO_URL")
	}
	if gitBranch == "" || gitBranch == "main" {
		if envBranch := os.Getenv("GIT_BRANCH"); envBranch != "" {
			gitBranch = envBranch
		}
	}
	gitToken := os.Getenv("GIT_TOKEN")

	// Only setup UpgradeRequest controller if Git is configured
	if gitBackend != "" && gitRepoURL != "" && gitToken != "" {
		gitConfig := git.Config{
			Backend: gitBackend,
			RepoURL: gitRepoURL,
			Branch:  gitBranch,
			Token:   gitToken,
		}

		gitManager, err := git.NewManager(gitConfig)
		if err != nil {
			setupLog.Error("failed to create git manager", "error", err)
			os.Exit(1)
		}

		if err := (&controller.UpgradeRequestReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			GitManager:      gitManager,
			SnapshotManager: snapshot.NewManager(mgr.GetClient()),
			FluxHelper:      flux.NewHelper(mgr.GetClient()),
			YAMLEditor:      yamlpkg.NewEditor(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error("unable to create controller", "controller", "UpgradeRequest", "error", err)
			os.Exit(1)
		}
		setupLog.Info("UpgradeRequest controller enabled", "backend", gitBackend, "repo", gitRepoURL)
	} else {
		setupLog.Info("UpgradeRequest controller disabled (Git not configured)")
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up health check", "error", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up ready check", "error", err)
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error("problem running manager", "error", err)
		os.Exit(1)
	}
}
