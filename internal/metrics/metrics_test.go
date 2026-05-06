/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// expectedMetrics lists every metric name the operator must expose.
var expectedMetrics = []string{
	"incidentary_operator_events_sent_total",
	"incidentary_operator_events_dropped_total",
	"incidentary_operator_events_filtered_total",
	"incidentary_operator_flush_latency_seconds",
	"incidentary_operator_flush_batch_size",
	"incidentary_operator_topology_reports_total",
	"incidentary_operator_watched_workloads",
	"incidentary_operator_matched_services",
	"incidentary_operator_ghost_services",
	"incidentary_operator_unmatched_workloads",
	"incidentary_operator_reconciliation_duration_seconds",
	"incidentary_operator_informer_cache_size",
	"incidentary_operator_leader_is_leader",
}

func TestAllMetricsRegistered(t *testing.T) {
	// Vec metrics only appear after at least one label combination is
	// initialized. Touch them so Gather returns all 13 families.
	EventsSentTotal.WithLabelValues("_test")
	EventsDroppedTotal.WithLabelValues("_test")
	EventsFilteredTotal.WithLabelValues("_test")
	InformerCacheSize.WithLabelValues("_test")

	gathered, err := metrics.Registry.(*prometheus.Registry).Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	byName := make(map[string]*dto.MetricFamily, len(gathered))
	for _, mf := range gathered {
		byName[mf.GetName()] = mf
	}

	for _, name := range expectedMetrics {
		if _, ok := byName[name]; !ok {
			t.Errorf("expected metric %q not found in registry", name)
		}
	}
}

func TestCounterVecLabels(t *testing.T) {
	// Verify the label dimensions are correct by exercising WithLabelValues.
	EventsSentTotal.WithLabelValues("K8S_OOM_KILL").Inc()
	EventsDroppedTotal.WithLabelValues("AGENT_TYPE_MISMATCH").Inc()
	EventsFilteredTotal.WithLabelValues("tier2").Inc()
	InformerCacheSize.WithLabelValues("pods").Set(42)
}

func TestGaugeDefaults(t *testing.T) {
	// Gauges should start at zero.
	gauges := []prometheus.Gauge{
		WatchedWorkloads,
		MatchedServices,
		GhostServices,
		UnmatchedWorkloads,
		LeaderIsLeader,
	}
	for _, g := range gauges {
		m := &dto.Metric{}
		if err := g.Write(m); err != nil {
			t.Fatalf("failed to write gauge: %v", err)
		}
		if v := m.GetGauge().GetValue(); v != 0 {
			t.Errorf("gauge should start at 0, got %f", v)
		}
	}
}

func TestHistogramBuckets(t *testing.T) {
	// Observe a value and verify the histogram accepts it without panic.
	FlushLatencySeconds.Observe(0.123)
	FlushBatchSize.Observe(50)
	ReconciliationDurationSeconds.Observe(1.5)
	TopologyReportsTotal.Inc()
}

func TestLeaderGaugeToggle(t *testing.T) {
	LeaderIsLeader.Set(1)
	m := &dto.Metric{}
	if err := LeaderIsLeader.Write(m); err != nil {
		t.Fatal(err)
	}
	if v := m.GetGauge().GetValue(); v != 1 {
		t.Errorf("leader gauge = %f, want 1", v)
	}

	LeaderIsLeader.Set(0)
	if err := LeaderIsLeader.Write(m); err != nil {
		t.Fatal(err)
	}
	if v := m.GetGauge().GetValue(); v != 0 {
		t.Errorf("leader gauge = %f, want 0", v)
	}
}
