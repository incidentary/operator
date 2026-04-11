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

// Package discovery runs the periodic service-discovery and ghost-population
// loop. On each tick, the loop enumerates Deployments/StatefulSets/DaemonSets
// across the cluster, resolves a service_id for each, and POSTs a topology
// report to the Incidentary API. The server uses the report to upsert ghost
// services and populate the coverage dashboard.
package discovery

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
)

// DefaultInterval is used when Loop is constructed with a zero interval.
const DefaultInterval = 5 * time.Minute

// DefaultExcludedNamespaces are skipped unless the operator is configured
// to include them explicitly.
var DefaultExcludedNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// Loop is a controller-runtime Runnable that periodically scans the cluster
// for workloads and sends a topology report to the Incidentary API.
type Loop struct {
	client       client.Reader
	resolver     *identity.Resolver
	topology     ingestclient.TopologyClient
	log          logr.Logger
	clusterName  string
	interval     time.Duration
	excludes     map[string]bool
	lastReport   atomic.Value // stores time.Time
	workloadsGauge atomic.Int32
}

// Options configure a Loop via the constructor.
type Options struct {
	Interval          time.Duration
	ClusterName       string
	ExcludeNamespaces []string
}

// NewLoop constructs a discovery Loop. client is typically the manager's
// cached reader; resolver uses the same cache.
func NewLoop(
	c client.Reader,
	resolver *identity.Resolver,
	topology ingestclient.TopologyClient,
	log logr.Logger,
	opts Options,
) *Loop {
	if c == nil {
		panic("discovery.NewLoop: client must not be nil")
	}
	if resolver == nil {
		panic("discovery.NewLoop: resolver must not be nil")
	}
	if topology == nil {
		panic("discovery.NewLoop: topology must not be nil")
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	excludes := make(map[string]bool, len(DefaultExcludedNamespaces)+len(opts.ExcludeNamespaces))
	for k, v := range DefaultExcludedNamespaces {
		excludes[k] = v
	}
	for _, ns := range opts.ExcludeNamespaces {
		excludes[ns] = true
	}

	l := &Loop{
		client:      c,
		resolver:    resolver,
		topology:    topology,
		log:         log,
		clusterName: opts.ClusterName,
		interval:    interval,
		excludes:    excludes,
	}
	l.lastReport.Store(time.Time{})
	return l
}

// Compile-time assertion: *Loop is a controller-runtime Runnable that runs
// on the elected leader only.
var _ interface {
	Start(context.Context) error
	NeedLeaderElection() bool
} = (*Loop)(nil)

// NeedLeaderElection reports that the discovery loop must run on the elected
// leader only — duplicate topology reports would inflate ghost service
// counts and confuse the coverage dashboard.
func (l *Loop) NeedLeaderElection() bool { return true }

// Start runs the discovery loop until ctx is cancelled. It runs one
// discovery cycle immediately, then every Interval after that.
func (l *Loop) Start(ctx context.Context) error {
	l.log.Info("discovery loop starting",
		"interval", l.interval,
		"clusterName", l.clusterName,
	)

	if err := l.runOnce(ctx); err != nil {
		l.log.Error(err, "initial discovery cycle failed")
	}

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.log.Info("discovery loop stopped")
			return nil
		case <-ticker.C:
			if err := l.runOnce(ctx); err != nil {
				l.log.Error(err, "discovery cycle failed")
			}
		}
	}
}

// runOnce performs a single discovery + topology-report cycle.
func (l *Loop) runOnce(ctx context.Context) error {
	workloads, err := l.collectWorkloads(ctx)
	if err != nil {
		return err
	}
	l.workloadsGauge.Store(int32(len(workloads))) //nolint:gosec // count is bounded

	if len(workloads) == 0 {
		l.log.V(1).Info("no workloads discovered")
		return nil
	}

	report := &ingestclient.TopologyReport{
		ClusterName: l.clusterName,
		ObservedAt:  time.Now().UnixNano(),
		Workloads:   workloads,
	}
	res, err := l.topology.Report(ctx, report)
	if err != nil {
		return err
	}

	l.lastReport.Store(time.Now())
	l.log.Info("topology reported",
		"workloads", len(workloads),
		"created_ghosts", res.CreatedGhostServices,
		"updated", res.UpdatedServices,
	)
	return nil
}

// WatchedWorkloads returns the most recent workload count observed by the
// loop. Used by the CRD status reporter.
func (l *Loop) WatchedWorkloads() int32 {
	return l.workloadsGauge.Load()
}

// LastReport returns the timestamp of the most recent successful report, or
// the zero time if no report has succeeded yet.
func (l *Loop) LastReport() time.Time {
	v, _ := l.lastReport.Load().(time.Time)
	return v
}

// collectWorkloads enumerates Deployments, StatefulSets, and DaemonSets and
// converts each to a TopologyWorkload.
func (l *Loop) collectWorkloads(ctx context.Context) ([]ingestclient.TopologyWorkload, error) {
	var out []ingestclient.TopologyWorkload

	var deps appsv1.DeploymentList
	if err := l.client.List(ctx, &deps); err != nil {
		return nil, err
	}
	for i := range deps.Items {
		if l.excludes[deps.Items[i].Namespace] {
			continue
		}
		if isSystemWorkload(deps.Items[i].Labels) {
			continue
		}
		out = append(out, l.deploymentToWorkload(ctx, &deps.Items[i]))
	}

	var sts appsv1.StatefulSetList
	if err := l.client.List(ctx, &sts); err != nil {
		return nil, err
	}
	for i := range sts.Items {
		if l.excludes[sts.Items[i].Namespace] {
			continue
		}
		if isSystemWorkload(sts.Items[i].Labels) {
			continue
		}
		out = append(out, l.statefulSetToWorkload(ctx, &sts.Items[i]))
	}

	var ds appsv1.DaemonSetList
	if err := l.client.List(ctx, &ds); err != nil {
		return nil, err
	}
	for i := range ds.Items {
		if l.excludes[ds.Items[i].Namespace] {
			continue
		}
		if isSystemWorkload(ds.Items[i].Labels) {
			continue
		}
		out = append(out, l.daemonSetToWorkload(ctx, &ds.Items[i]))
	}

	return out, nil
}

func (l *Loop) deploymentToWorkload(ctx context.Context, d *appsv1.Deployment) ingestclient.TopologyWorkload {
	res, _ := l.resolver.ResolveRef(ctx, "Deployment", d.Namespace, d.Name)
	return ingestclient.TopologyWorkload{
		ServiceID:         fallbackName(res.ServiceID, d.Name),
		ServiceIDSource:   sourceString(res.Source),
		Kind:              "Deployment",
		Namespace:         d.Namespace,
		Replicas:          deref(d.Spec.Replicas),
		AvailableReplicas: d.Status.AvailableReplicas,
		Image:             primaryImage(d.Spec.Template.Spec.Containers),
		Labels:            d.Labels,
		Annotations:       d.Annotations,
		CreatedAt:         d.CreationTimestamp.UnixNano(),
		Conditions:        conditionsFromDeployment(d.Status.Conditions),
	}
}

func (l *Loop) statefulSetToWorkload(ctx context.Context, s *appsv1.StatefulSet) ingestclient.TopologyWorkload {
	res, _ := l.resolver.ResolveRef(ctx, "StatefulSet", s.Namespace, s.Name)
	return ingestclient.TopologyWorkload{
		ServiceID:         fallbackName(res.ServiceID, s.Name),
		ServiceIDSource:   sourceString(res.Source),
		Kind:              "StatefulSet",
		Namespace:         s.Namespace,
		Replicas:          deref(s.Spec.Replicas),
		AvailableReplicas: s.Status.ReadyReplicas,
		Image:             primaryImage(s.Spec.Template.Spec.Containers),
		Labels:            s.Labels,
		Annotations:       s.Annotations,
		CreatedAt:         s.CreationTimestamp.UnixNano(),
	}
}

func (l *Loop) daemonSetToWorkload(ctx context.Context, d *appsv1.DaemonSet) ingestclient.TopologyWorkload {
	res, _ := l.resolver.ResolveRef(ctx, "DaemonSet", d.Namespace, d.Name)
	return ingestclient.TopologyWorkload{
		ServiceID:         fallbackName(res.ServiceID, d.Name),
		ServiceIDSource:   sourceString(res.Source),
		Kind:              "DaemonSet",
		Namespace:         d.Namespace,
		Replicas:          d.Status.DesiredNumberScheduled,
		AvailableReplicas: d.Status.NumberReady,
		Image:             primaryImage(d.Spec.Template.Spec.Containers),
		Labels:            d.Labels,
		Annotations:       d.Annotations,
		CreatedAt:         d.CreationTimestamp.UnixNano(),
	}
}

// isSystemWorkload returns true for infrastructure workloads we should not
// report as ghost services (control-plane, CNI, monitoring daemon labels).
func isSystemWorkload(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	if component := labels["app.kubernetes.io/component"]; component == "controller-manager" {
		return true
	}
	if k8sApp := labels["k8s-app"]; k8sApp != "" {
		// kube-proxy, kube-dns, metrics-server, etc. all use k8s-app.
		return true
	}
	return false
}

func conditionsFromDeployment(conds []appsv1.DeploymentCondition) []ingestclient.WorkloadCond {
	out := make([]ingestclient.WorkloadCond, 0, len(conds))
	for _, c := range conds {
		out = append(out, ingestclient.WorkloadCond{
			Type:   string(c.Type),
			Status: string(c.Status),
		})
	}
	return out
}

func fallbackName(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

func sourceString(s identity.Source) string {
	if s == "" {
		return string(identity.SourceWorkloadName)
	}
	return string(s)
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// primaryImage returns the image of the first container in the given slice,
// or empty when the slice is empty. The operator treats the first container
// as the "main" container for reporting purposes — multi-container workloads
// still show a representative image.
func primaryImage(containers []corev1.Container) string {
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}
