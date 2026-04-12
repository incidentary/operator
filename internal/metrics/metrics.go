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

// Package metrics exposes operator Prometheus metrics. All metrics are
// registered with the controller-runtime metrics registry so they are served
// at the manager's /metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Ingest pipeline metrics — recorded by the batcher and ingest client.
var (
	EventsSentTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "incidentary_operator_events_sent_total",
			Help: "Total v2 events accepted by the ingest endpoint.",
		},
		[]string{"kind"},
	)

	EventsDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "incidentary_operator_events_dropped_total",
			Help: "Events dropped by the ingest endpoint.",
		},
		[]string{"drop_reason"},
	)

	EventsFilteredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "incidentary_operator_events_filtered_total",
			Help: "Events filtered by the severity policy before sending.",
		},
		[]string{"tier"},
	)

	FlushLatencySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "incidentary_operator_flush_latency_seconds",
			Help:    "Ingest flush round-trip latency in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	FlushBatchSize = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "incidentary_operator_flush_batch_size",
			Help:    "Number of events per flush batch.",
			Buckets: prometheus.LinearBuckets(10, 50, 20),
		},
	)
)

// Discovery and topology metrics — recorded by the discovery loop.
var (
	TopologyReportsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "incidentary_operator_topology_reports_total",
			Help: "Total topology reports sent to the Incidentary API.",
		},
	)

	WatchedWorkloads = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_watched_workloads",
			Help: "Current count of discovered workloads (Deployments + StatefulSets + DaemonSets).",
		},
	)
)

// Reconciliation metrics — recorded by the services-list reconciler.
var (
	MatchedServices = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_matched_services",
			Help: "Workloads matched to SDK-registered services.",
		},
	)

	GhostServices = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_ghost_services",
			Help: "Discovered workloads with no SDK event history (ghost services).",
		},
	)

	UnmatchedWorkloads = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_unmatched_workloads",
			Help: "Workloads whose derived service_id does not match any registered service.",
		},
	)

	ReconciliationDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "incidentary_operator_reconciliation_duration_seconds",
			Help:    "Duration of a single reconciliation cycle in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		},
	)
)

// Informer and leader election metrics.
var (
	InformerCacheSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_informer_cache_size",
			Help: "Number of objects in the informer cache, by resource type.",
		},
		[]string{"resource"},
	)

	LeaderIsLeader = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "incidentary_operator_leader_is_leader",
			Help: "1 if this instance is the active leader, 0 otherwise.",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		EventsSentTotal,
		EventsDroppedTotal,
		EventsFilteredTotal,
		FlushLatencySeconds,
		FlushBatchSize,
		TopologyReportsTotal,
		WatchedWorkloads,
		MatchedServices,
		GhostServices,
		UnmatchedWorkloads,
		ReconciliationDurationSeconds,
		InformerCacheSize,
		LeaderIsLeader,
	)
}
