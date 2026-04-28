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
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	incidentaryv1alpha1 "github.com/incidentary/operator/api/v1alpha1"
	"github.com/incidentary/operator/internal/batch"
	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/controller"
	"github.com/incidentary/operator/internal/discovery"
	"github.com/incidentary/operator/internal/filter"
	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/informers"
	"github.com/incidentary/operator/internal/mapper"
	"github.com/incidentary/operator/internal/wireformat"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(incidentaryv1alpha1.AddToScheme(scheme))
	// informers.AddToScheme is idempotent with clientgoscheme, but we call it
	// explicitly to document every resource type the operator watches.
	utilruntime.Must(informers.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
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
	// Development defaults to false so the production logger uses JSON output
	// with stacktraces only at Error level. Operators who need verbose dev-mode
	// logging during debugging can opt in with --zap-devel=true at runtime
	// (the flag is registered by opts.BindFlags below).
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
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
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
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
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
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
		LeaderElectionID:       "140b099f.incidentary.com",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Credential precedence:
	//   1. CR-referenced Secret (post-first-reconcile, hot-rotated by the
	//      controller). Source of truth in steady state.
	//   2. INCIDENTARY_API_KEY / INCIDENTARY_WORKSPACE_ID env vars
	//      (bootstrap only — used for the brief window before the controller
	//      reconciles for the first time).
	//   3. Dropping stubs (when neither (1) nor (2) is configured — operator
	//      runs read-only without emitting events).
	apiKey := os.Getenv("INCIDENTARY_API_KEY")
	workspaceID := getEnvOrDefault("INCIDENTARY_WORKSPACE_ID", "")
	ingestEndpoint := os.Getenv("INCIDENTARY_INGEST_ENDPOINT")
	topologyEndpoint := os.Getenv("INCIDENTARY_TOPOLOGY_ENDPOINT")
	servicesEndpoint := os.Getenv("INCIDENTARY_SERVICES_ENDPOINT")
	if apiKey == "" {
		setupLog.Info("INCIDENTARY_API_KEY is not set; running with dropping stubs. " +
			"The operator will still watch resources but no events will be sent " +
			"until the controller reconciles an IncidentaryConfig CR with a valid Secret.")
	}
	if warnIfMisconfigured(apiKey, workspaceID) {
		setupLog.Error(nil,
			"INCIDENTARY_WORKSPACE_ID is not set but INCIDENTARY_API_KEY is; "+
				"the server will reject every batch with WORKSPACE_MISMATCH (422). "+
				"Set INCIDENTARY_WORKSPACE_ID to the workspace that owns the API key.")
	}
	clusterName := getEnvOrDefault("K8S_CLUSTER_NAME", "unknown")
	if warnIfClusterNameUnset(clusterName) {
		setupLog.Info("K8S_CLUSTER_NAME is not set; resource.k8s.cluster.name will be \"unknown\". " +
			"§5.1 of wire format v2 requires this field for k8s_operator agents — set it to the " +
			"actual cluster name for accurate event correlation.")
	}

	// Build the Provider with bootstrap credentials. The controller's
	// IncidentaryConfigReconciler will Rotate() on every reconcile, replacing
	// these with whatever the CR-referenced Secret contains.
	provider := buildInitialProvider(apiKey, workspaceID, ingestEndpoint, topologyEndpoint, servicesEndpoint, ctrl.Log)

	// Controller is constructed early so the reconciler is ready when CRs
	// arrive. Discovery and Classifier observers are wired below once the
	// corresponding loops exist. Provider + endpoints are wired now so the
	// reconciler can call Rotate on the first reconciliation.
	reconciler := &controller.IncidentaryConfigReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Rotator:          provider,
		IngestEndpoint:   ingestEndpoint,
		TopologyEndpoint: topologyEndpoint,
		ServicesEndpoint: servicesEndpoint,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "IncidentaryConfig")
		os.Exit(1)
	}

	resource := func() wireformat.Resource {
		attrs := map[string]string{
			"k8s.cluster.name": clusterName,
		}
		if ns := getEnvOrDefault("K8S_NAMESPACE_NAME", ""); ns != "" {
			attrs["k8s.namespace.name"] = ns
		}
		return wireformat.Resource{Attributes: attrs}
	}
	agent := func() wireformat.Agent {
		return wireformat.Agent{
			Type:        wireformat.AgentTypeK8sOperator,
			Version:     ingestclient.DefaultAgentVersion,
			WorkspaceID: provider.WorkspaceID(),
		}
	}

	batcher := batch.NewBatcher(provider.Ingest, resource, agent, ctrl.Log.WithName("batcher"))
	if err := mgr.Add(batcher); err != nil {
		setupLog.Error(err, "Failed to add batcher to controller manager")
		os.Exit(1)
	}

	minSeverity := getEnvOrDefault("INCIDENTARY_MIN_SEVERITY", "warning")
	severityFilter := filter.NewFromString(minSeverity)

	resolver := identity.NewResolver(mgr.GetClient())
	mpr := mapper.NewMapper(resolver)
	handler := &pipelineHandler{
		mapper:  mpr,
		filter:  severityFilter,
		batcher: batcher,
		log:     ctrl.Log.WithName("pipeline"),
	}

	infMgr := informers.NewManager(mgr, handler, ctrl.Log.WithName("informers"))
	if err := mgr.Add(infMgr); err != nil {
		setupLog.Error(err, "Failed to add informer manager to controller manager")
		os.Exit(1)
	}

	// Discovery + topology reporter. Runs on the elected leader only, scans
	// workloads every 5 minutes (default), and POSTs a topology report. The
	// topology client is resolved on every cycle from the Provider, so
	// credential rotation takes effect immediately.
	reconciliationInterval := parseDurationEnv("INCIDENTARY_RECONCILIATION_INTERVAL_SECONDS", 300*time.Second)
	excludeNamespaces := parseStringSliceEnv("INCIDENTARY_EXCLUDE_NAMESPACES")
	discoveryLoop := discovery.NewLoop(
		mgr.GetClient(),
		resolver,
		provider.Topology,
		ctrl.Log.WithName("discovery"),
		discovery.Options{
			ClusterName:       clusterName,
			Interval:          reconciliationInterval,
			ExcludeNamespaces: excludeNamespaces,
		},
	)
	if err := mgr.Add(discoveryLoop); err != nil {
		setupLog.Error(err, "Failed to add discovery loop to controller manager")
		os.Exit(1)
	}
	reconciler.Discovery = discoveryLoop

	// Services-list reconciliation loop. Same Provider-driven hot-rotation.
	reconcilerLoop := discovery.NewReconciler(
		mgr.GetClient(),
		discoveryLoop,
		provider.Services,
		ctrl.Log.WithName("reconciler"),
		reconciliationInterval,
	)
	if err := mgr.Add(reconcilerLoop); err != nil {
		setupLog.Error(err, "Failed to add reconciliation loop to controller manager")
		os.Exit(1)
	}
	reconciler.Classifier = reconcilerLoop

	// LeaderIsLeader metric: a trivial runnable that sets the gauge to 1 when
	// the manager elects this instance and back to 0 on context cancellation.
	if err := mgr.Add(&leaderMetricRunnable{}); err != nil {
		setupLog.Error(err, "Failed to add leader metric runnable")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
