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

package discovery

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/metrics"
)

// Classification labels a workload as one of four states based on the
// server's registered services list. Used for metrics and CRD status.
type Classification string

const (
	ClassMatched    Classification = "matched"    // registered + instrumented
	ClassGhost      Classification = "ghost"      // registered, no SDK events yet
	ClassMismatched Classification = "mismatched" // derived id not registered, grace period expired
	ClassNew        Classification = "new"        // derived id not registered, still within grace window
)

// ReconcilerDefaults — tunables for the reconciliation loop.
const (
	// FuzzyMatchThreshold is the maximum Levenshtein distance for a
	// mismatched workload to suggest an annotation value.
	FuzzyMatchThreshold = 2

	// NewWorkloadGraceExtra is additional time past the reconciliation
	// interval we wait before warning about a mismatched workload.
	NewWorkloadGraceExtra = 30 * time.Second

	// ReconcilerInitialBackoff is the first backoff duration applied when
	// the services list is empty (day-zero case).
	ReconcilerInitialBackoff = 5 * time.Minute
	// ReconcilerMaxBackoff caps the exponential backoff ceiling.
	ReconcilerMaxBackoff = 30 * time.Minute
)

// ServicesClientProvider returns the active services-list client at call
// time. Resolved on every poll so credential rotation takes effect
// immediately.
type ServicesClientProvider func() ingestclient.ServicesClient

// Reconciler classifies discovered workloads against the registered services
// list and surfaces mismatches via logs and CRD status. It runs in the same
// LeaderElectionRunnable as the discovery Loop (registered separately).
type Reconciler struct {
	client     client.Reader
	loop       *Loop
	servicesFn ServicesClientProvider
	log        logr.Logger
	interval   time.Duration

	matchedGauge    atomic.Int32
	ghostGauge      atomic.Int32
	mismatchedGauge atomic.Int32
	newGauge        atomic.Int32

	// currentBackoff tracks the exponential wait when the services list is
	// empty. Reset to interval on the first non-empty response.
	currentBackoff time.Duration
}

// NewReconciler constructs a Reconciler. servicesFn returns the active
// services-list client; loop is the discovery Loop the reconciler runs
// alongside.
func NewReconciler(
	c client.Reader,
	loop *Loop,
	servicesFn ServicesClientProvider,
	log logr.Logger,
	interval time.Duration,
) *Reconciler {
	if c == nil {
		panic("discovery.NewReconciler: client must not be nil")
	}
	if servicesFn == nil {
		panic("discovery.NewReconciler: servicesFn must not be nil")
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Reconciler{
		client:         c,
		loop:           loop,
		servicesFn:     servicesFn,
		log:            log,
		interval:       interval,
		currentBackoff: interval,
	}
}

// Compile-time assertion: *Reconciler is a controller-runtime Runnable that
// runs on the elected leader only.
var _ interface {
	Start(context.Context) error
	NeedLeaderElection() bool
} = (*Reconciler)(nil)

// NeedLeaderElection — reconciliation reads state from one source of truth.
func (r *Reconciler) NeedLeaderElection() bool { return true }

// Counts returns the current gauge values (matched, ghost, mismatched, new).
// Used by the controller to populate Status fields.
func (r *Reconciler) Counts() (matched, ghost, mismatched, newCount int32) {
	return r.matchedGauge.Load(),
		r.ghostGauge.Load(),
		r.mismatchedGauge.Load(),
		r.newGauge.Load()
}

// Start runs the reconciliation loop until ctx is cancelled. It does an
// initial tick immediately, then every currentBackoff/interval.
func (r *Reconciler) Start(ctx context.Context) error {
	r.log.Info("reconciliation loop starting", "interval", r.interval)

	for {
		if err := r.runOnce(ctx); err != nil {
			r.log.Error(err, "reconciliation cycle failed")
		}

		wait := r.currentBackoff
		select {
		case <-ctx.Done():
			r.log.Info("reconciliation loop stopped")
			return nil
		case <-time.After(wait):
		}
	}
}

// runOnce performs one reconciliation cycle: fetch services, classify
// workloads, update gauges, log mismatches.
func (r *Reconciler) runOnce(ctx context.Context) error {
	start := time.Now()
	defer func() {
		metrics.ReconciliationDurationSeconds.Observe(time.Since(start).Seconds())
	}()

	registered, err := r.servicesFn().List(ctx)
	if err != nil {
		return fmt.Errorf("reconciler: list services: %w", err)
	}

	if len(registered) == 0 {
		r.log.V(1).Info("services list empty; deferring reconciliation",
			"backoff", r.currentBackoff)
		// Exponential backoff: grow until ReconcilerMaxBackoff.
		r.currentBackoff *= 2
		if r.currentBackoff > ReconcilerMaxBackoff {
			r.currentBackoff = ReconcilerMaxBackoff
		}
		return nil
	}

	// First non-empty response — reset to the configured interval.
	r.currentBackoff = r.interval

	registeredByID := make(map[string]ingestclient.ServiceEntry, len(registered))
	registeredIDs := make([]string, 0, len(registered))
	for _, s := range registered {
		registeredByID[s.ServiceID] = s
		registeredIDs = append(registeredIDs, s.ServiceID)
	}

	workloads, err := r.listWorkloads(ctx)
	if err != nil {
		return err
	}

	var matched, ghost, mismatched, newCount int32
	gracePeriod := r.interval + NewWorkloadGraceExtra

	for _, w := range workloads {
		cls := r.classify(w, registeredByID, registeredIDs, gracePeriod)
		switch cls {
		case ClassMatched:
			matched++
		case ClassGhost:
			ghost++
		case ClassMismatched:
			mismatched++
		case ClassNew:
			newCount++
		}
	}

	r.matchedGauge.Store(matched)
	r.ghostGauge.Store(ghost)
	r.mismatchedGauge.Store(mismatched)
	r.newGauge.Store(newCount)

	metrics.MatchedServices.Set(float64(matched))
	metrics.GhostServices.Set(float64(ghost))
	metrics.UnmatchedWorkloads.Set(float64(mismatched))

	r.log.V(1).Info("reconciliation cycle complete",
		"registered", len(registered),
		"workloads", len(workloads),
		"matched", matched,
		"ghost", ghost,
		"mismatched", mismatched,
		"new", newCount,
	)
	return nil
}

// workloadInfo is the minimal projection of a workload used by the
// reconciler — the exact Go type depends on which informer list produced it.
type workloadInfo struct {
	Kind      string
	Namespace string
	Name      string
	ServiceID string
	CreatedAt time.Time
}

// listWorkloads enumerates Deployments, StatefulSets, and DaemonSets and
// returns their resolved service IDs.
func (r *Reconciler) listWorkloads(ctx context.Context) ([]workloadInfo, error) {
	var out []workloadInfo

	var deps appsv1.DeploymentList
	if err := r.client.List(ctx, &deps); err != nil {
		return nil, err
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		serviceID := resolvedServiceID(d.Annotations, d.Name)
		out = append(out, workloadInfo{
			Kind:      "Deployment",
			Namespace: d.Namespace,
			Name:      d.Name,
			ServiceID: serviceID,
			CreatedAt: d.CreationTimestamp.Time,
		})
	}

	var sts appsv1.StatefulSetList
	if err := r.client.List(ctx, &sts); err != nil {
		return nil, err
	}
	for i := range sts.Items {
		s := &sts.Items[i]
		out = append(out, workloadInfo{
			Kind:      "StatefulSet",
			Namespace: s.Namespace,
			Name:      s.Name,
			ServiceID: resolvedServiceID(s.Annotations, s.Name),
			CreatedAt: s.CreationTimestamp.Time,
		})
	}

	var ds appsv1.DaemonSetList
	if err := r.client.List(ctx, &ds); err != nil {
		return nil, err
	}
	for i := range ds.Items {
		d := &ds.Items[i]
		out = append(out, workloadInfo{
			Kind:      "DaemonSet",
			Namespace: d.Namespace,
			Name:      d.Name,
			ServiceID: resolvedServiceID(d.Annotations, d.Name),
			CreatedAt: d.CreationTimestamp.Time,
		})
	}

	return out, nil
}

// resolvedServiceID mirrors identity.Resolver's annotation-or-name rule
// without the owner-walking cost (we are classifying owning workloads
// directly so the ReplicaSet detour is not needed).
func resolvedServiceID(annotations map[string]string, fallback string) string {
	if v := annotations[identity.ServiceIDAnnotation]; v != "" {
		return v
	}
	return fallback
}

// classify returns the Classification for a single workload.
func (r *Reconciler) classify(
	w workloadInfo,
	registeredByID map[string]ingestclient.ServiceEntry,
	registeredIDs []string,
	gracePeriod time.Duration,
) Classification {
	entry, ok := registeredByID[w.ServiceID]
	if ok {
		if entry.Instrumented {
			return ClassMatched
		}
		return ClassGhost
	}
	age := time.Since(w.CreatedAt)
	if age < gracePeriod {
		return ClassNew
	}
	// Fuzzy match suggestion (logged but does not change classification).
	if suggestion, ok := fuzzyMatch(w.ServiceID, registeredIDs, FuzzyMatchThreshold); ok {
		r.log.Info("workload service_id does not match any registered service",
			"workload", fmt.Sprintf("%s/%s", w.Namespace, w.Name),
			"derivedServiceID", w.ServiceID,
			"suggestion", suggestion,
			"hint", fmt.Sprintf("add annotation %s=%s to %s", identity.ServiceIDAnnotation, suggestion, w.Name),
		)
	}
	return ClassMismatched
}

// fuzzyMatch returns the best single-candidate match within maxDist
// Levenshtein distance, or ("", false) when no candidate qualifies.
func fuzzyMatch(target string, candidates []string, maxDist int) (string, bool) {
	best := ""
	bestDist := maxDist + 1
	for _, c := range candidates {
		d := levenshtein(target, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist > maxDist {
		return "", false
	}
	return best, true
}

// levenshtein returns the edit distance between two strings. Classic
// iterative two-row DP.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = min3(del, ins, sub)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
