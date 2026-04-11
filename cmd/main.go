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
	"context"
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// pipelineHandler is the Phase-3 informers.Handler that routes cluster events
// through the mapper and enqueues the resulting wire format events on the
// batcher. Unknown object types produce no events (the mapper returns an
// empty slice).
type pipelineHandler struct {
	mapper  *mapper.Mapper
	batcher *batch.Batcher
	log     logr.Logger
}

func (h *pipelineHandler) OnAdd(ctx context.Context, obj client.Object) {
	h.emit(ctx, nil, obj)
}

func (h *pipelineHandler) OnUpdate(ctx context.Context, oldObj, newObj client.Object) {
	h.emit(ctx, oldObj, newObj)
}

func (h *pipelineHandler) OnDelete(ctx context.Context, obj client.Object) {
	h.emit(ctx, obj, nil)
}

func (h *pipelineHandler) emit(ctx context.Context, oldObj, newObj client.Object) {
	events, err := h.mapper.Dispatch(ctx, oldObj, newObj)
	if err != nil {
		h.log.Error(err, "mapper dispatch failed")
		return
	}
	if len(events) == 0 {
		return
	}
	h.batcher.Enqueue(events...)
}

// droppingIngestClient implements ingestclient.IngestClient by silently
// discarding every batch. It is installed when INCIDENTARY_API_KEY is not
// set at startup so the operator can still run (watching, reconciling,
// exposing metrics) without emitting events.
type droppingIngestClient struct {
	log logr.Logger
}

func (d *droppingIngestClient) Flush(_ context.Context, batch *wireformat.IngestBatch) (ingestclient.FlushResult, error) {
	d.log.V(1).Info("ingest disabled; dropping batch",
		"events", len(batch.Events),
	)
	return ingestclient.FlushResult{
		IngestResponse: ingestclient.IngestResponse{Accepted: 0, Dropped: len(batch.Events)},
	}, nil
}

// droppingTopologyClient discards topology reports when the API key is unset.
type droppingTopologyClient struct {
	log logr.Logger
}

func (d *droppingTopologyClient) Report(_ context.Context, report *ingestclient.TopologyReport) (*ingestclient.TopologyResponse, error) {
	d.log.V(1).Info("topology disabled; dropping report",
		"workloads", len(report.Workloads),
	)
	return &ingestclient.TopologyResponse{Accepted: 0}, nil
}

// emptyServicesClient returns an empty services list when no API key is
// configured, which keeps the reconciliation loop in its dormant state.
type emptyServicesClient struct{}

func (emptyServicesClient) List(context.Context) ([]ingestclient.ServiceEntry, error) {
	return nil, nil
}

// getEnvOrDefault returns the value of an environment variable or fallback
// when unset or empty.
func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseDurationEnv reads an integer-seconds duration from an environment
// variable, falling back to the supplied default when unset or invalid.
func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return fallback
	}
	return time.Duration(secs) * time.Second
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
	opts := zap.Options{
		Development: true,
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
		LeaderElectionID:       "140b099f.incidentary.io",
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
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Controller is constructed before the discovery loop because the
	// reconciler accepts a nil DiscoveryObserver; the field is wired below
	// once the loop exists.
	reconciler := &controller.IncidentaryConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "IncidentaryConfig")
		os.Exit(1)
	}

	// Phase 3: build the event-processing pipeline.
	// Resolver → Mapper → Batcher → IngestClient.
	//
	// Phase 3b reads the workspace API key from INCIDENTARY_API_KEY at
	// startup. Phase 4 will switch to resolving it from the Secret referenced
	// in the IncidentaryConfig CR on every reconcile, so the operator can
	// pick up key rotations without a restart.
	apiKey := os.Getenv("INCIDENTARY_API_KEY")
	ingestEndpoint := os.Getenv("INCIDENTARY_INGEST_ENDPOINT")
	if apiKey == "" {
		setupLog.Info("INCIDENTARY_API_KEY is not set; running without an ingest client. " +
			"The operator will still watch resources but no events will be sent.")
	}

	var ingest ingestclient.IngestClient
	if apiKey != "" {
		ingest = ingestclient.NewHTTPClient(apiKey, ingestclient.WithEndpoint(ingestEndpoint))
	} else {
		ingest = &droppingIngestClient{log: ctrl.Log.WithName("ingest")}
	}

	resource := func() wireformat.Resource {
		return wireformat.Resource{
			Attributes: map[string]string{
				"k8s.cluster.name":   getEnvOrDefault("K8S_CLUSTER_NAME", "unknown"),
				"k8s.namespace.name": getEnvOrDefault("K8S_NAMESPACE_NAME", ""),
			},
		}
	}
	agent := func() wireformat.Agent {
		return wireformat.Agent{
			Type:        wireformat.AgentTypeK8sOperator,
			Version:     ingestclient.DefaultAgentVersion,
			WorkspaceID: getEnvOrDefault("INCIDENTARY_WORKSPACE_ID", ""),
		}
	}

	batcher := batch.NewBatcher(ingest, resource, agent, ctrl.Log.WithName("batcher"))
	if err := mgr.Add(batcher); err != nil {
		setupLog.Error(err, "Failed to add batcher to controller manager")
		os.Exit(1)
	}

	resolver := identity.NewResolver(mgr.GetClient())
	mpr := mapper.NewMapper(resolver)
	handler := &pipelineHandler{
		mapper:  mpr,
		batcher: batcher,
		log:     ctrl.Log.WithName("pipeline"),
	}

	infMgr := informers.NewManager(mgr, handler, ctrl.Log.WithName("informers"))
	if err := mgr.Add(infMgr); err != nil {
		setupLog.Error(err, "Failed to add informer manager to controller manager")
		os.Exit(1)
	}

	// Phase 4: discovery + topology reporter. Runs on the elected leader
	// only, scans workloads every 5 minutes (default), and POSTs a topology
	// report to the v2 API. When INCIDENTARY_API_KEY is unset the topology
	// client is also replaced with a dropping stub.
	var topologyClient ingestclient.TopologyClient
	if apiKey != "" {
		topologyClient = ingestclient.NewTopologyClient(apiKey,
			ingestclient.WithTopologyEndpoint(os.Getenv("INCIDENTARY_TOPOLOGY_ENDPOINT")))
	} else {
		topologyClient = &droppingTopologyClient{log: ctrl.Log.WithName("topology")}
	}

	reconciliationInterval := parseDurationEnv("INCIDENTARY_RECONCILIATION_INTERVAL_SECONDS", 300*time.Second)
	discoveryLoop := discovery.NewLoop(
		mgr.GetClient(),
		resolver,
		topologyClient,
		ctrl.Log.WithName("discovery"),
		discovery.Options{
			ClusterName: getEnvOrDefault("K8S_CLUSTER_NAME", "unknown"),
			Interval:    reconciliationInterval,
		},
	)
	if err := mgr.Add(discoveryLoop); err != nil {
		setupLog.Error(err, "Failed to add discovery loop to controller manager")
		os.Exit(1)
	}
	reconciler.Discovery = discoveryLoop

	// Phase 6: services-list reconciliation loop.
	var servicesClient ingestclient.ServicesClient
	if apiKey != "" {
		servicesClient = ingestclient.NewServicesClient(apiKey,
			ingestclient.WithServicesEndpoint(os.Getenv("INCIDENTARY_SERVICES_ENDPOINT")))
	} else {
		servicesClient = &emptyServicesClient{}
	}
	reconcilerLoop := discovery.NewReconciler(
		mgr.GetClient(),
		discoveryLoop,
		servicesClient,
		ctrl.Log.WithName("reconciler"),
		reconciliationInterval,
	)
	if err := mgr.Add(reconcilerLoop); err != nil {
		setupLog.Error(err, "Failed to add reconciliation loop to controller manager")
		os.Exit(1)
	}
	reconciler.Classifier = reconcilerLoop
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
