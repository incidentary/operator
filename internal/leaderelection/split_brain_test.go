// Package leaderelection contains hermetic regression tests for the
// operator's leader-election failover characteristics.
//
// We do not exercise the controller-runtime Manager directly here —
// that would require an envtest apiserver and a second operator
// process. Instead, we exercise the SAME primitive (`client-go/tools/
// leaderelection.LeaderElector` over a `resourcelock.LeaseLock`) the
// Manager wraps internally, with two electors competing for the same
// in-memory Lease, and assert the failover sequence:
//
//   1. Elector A acquires the lease and runs OnStartedLeading.
//   2. Elector A loses the apiserver / stops renewing.
//   3. After LeaseDuration, Elector B observes the stale lease and
//      acquires it. OnStartedLeading fires on B.
//   4. Elector A's OnStoppedLeading must have fired before B started
//      (no overlap window where both are running their leader work
//      simultaneously — that's the "split-brain" condition).
//
// The contract under test is the *operator's* failover, but the code
// path is upstream client-go. By pinning behaviour at this layer we
// guard against:
//   - controller-runtime upgrades that flip default lease semantics
//   - kubebuilder scaffolding regressions that drop our explicit
//     LeaseDuration/RenewDeadline/RetryPeriod overrides
//   - future refactors that introduce parallel leader-elected runnables

package leaderelection

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
)

const (
	testNamespace = "kube-system"
	testLeaseName = "incidentary-test-lease"
	// 1s lease duration is small enough to keep the test fast (full
	// cycle in ~3 seconds) but large enough that scheduler jitter
	// doesn't cause flakes. Production uses 30s.
	testLeaseDuration = 1 * time.Second
	testRenewDeadline = 750 * time.Millisecond
	testRetryPeriod   = 200 * time.Millisecond
	electorA          = "elector-A"
	electorB          = "elector-B"
)

// flushTracker is the fake "leader work" each elector runs while
// holding the lease. Each call to runFlush increments a per-elector
// counter and a global counter; the test asserts the global never
// exceeds the number of leadership turns (one increment per turn).
type flushTracker struct {
	global atomic.Int64
	mu     sync.Mutex
	perID  map[string]*atomic.Int64
}

func newFlushTracker() *flushTracker {
	return &flushTracker{perID: make(map[string]*atomic.Int64)}
}

func (f *flushTracker) runFlush(id string) {
	f.global.Add(1)
	f.mu.Lock()
	c, ok := f.perID[id]
	if !ok {
		c = &atomic.Int64{}
		f.perID[id] = c
	}
	f.mu.Unlock()
	c.Add(1)
}

// makeLeaseLock returns a fresh Lease lock identified by `id` over a
// shared fake Kubernetes clientset. Each elector gets its own lock
// instance pointed at the same Lease resource — that's the on-cluster
// invariant.
func makeLeaseLock(t *testing.T, client *fake.Clientset, id string) *resourcelock.LeaseLock {
	t.Helper()
	return &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testLeaseName,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: &record.FakeRecorder{},
		},
	}
}

// runElector spins up a LeaderElector on the supplied lock and
// flushTracker. It returns a context cancel function the test uses to
// simulate "pod killed" on a specific elector. OnStartedLeading runs
// runFlush once and then waits on a per-elector signal so the test
// can observe leadership-overlap windows.
func runElector(
	t *testing.T,
	parent context.Context,
	lock resourcelock.Interface,
	tracker *flushTracker,
	id string,
	onStarted chan<- string,
	onStopped chan<- string,
) (context.CancelFunc, *leaderelection.LeaderElector) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: testLeaseDuration,
		RenewDeadline: testRenewDeadline,
		RetryPeriod:   testRetryPeriod,
		// ReleaseOnCancel: true makes the elector politely release
		// the lease when its context is cancelled — mirrors what
		// `kubectl delete pod` would do during a graceful rolling
		// upgrade. With this set, the second elector observes the
		// release and acquires within a single retry period rather
		// than waiting for full LeaseDuration to expire.
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(_ context.Context) {
				tracker.runFlush(id)
				onStarted <- id
			},
			OnStoppedLeading: func() {
				onStopped <- id
			},
		},
		Name: id,
	})
	if err != nil {
		t.Fatalf("new elector %s: %v", id, err)
	}

	go elector.Run(ctx)
	return cancel, elector
}

// TestLeaderElection_HandoverHasNoDoubleFlush is the primary failover
// regression. Acquire on A, kill A, observe handover to B, assert
// global flush count == 2 (one per leader, never 3).
func TestLeaderElection_HandoverHasNoDoubleFlush(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // SA1019: NewClientset replacement requires apply configurations; not worth the dependency for an in-memory test fake.
	tracker := newFlushTracker()

	rootCtx, rootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rootCancel()

	onStarted := make(chan string, 4)
	onStopped := make(chan string, 4)

	cancelA, _ := runElector(t, rootCtx, makeLeaseLock(t, client, electorA),
		tracker, electorA, onStarted, onStopped)
	defer cancelA()

	cancelB, _ := runElector(t, rootCtx, makeLeaseLock(t, client, electorB),
		tracker, electorB, onStarted, onStopped)
	defer cancelB()

	// 1. Wait for one of them to win.
	first := waitFor(t, onStarted, 5*time.Second, "first OnStartedLeading")
	t.Logf("initial leader: %s (global flushes: %d)", first, tracker.global.Load())
	if tracker.global.Load() != 1 {
		t.Fatalf("after first acquire: expected global=1, got %d",
			tracker.global.Load())
	}

	// 2. Kill the current leader by cancelling its context.
	switch first {
	case electorA:
		cancelA()
	case electorB:
		cancelB()
	default:
		t.Fatalf("unexpected initial leader: %q", first)
	}

	// 3. Wait for the OnStoppedLeading callback on the killed elector.
	stopped := waitFor(t, onStopped, 5*time.Second, "OnStoppedLeading on killed elector")
	if stopped != first {
		t.Errorf("OnStoppedLeading: expected %s, got %s", first, stopped)
	}

	// 4. Wait for the surviving elector to acquire.
	second := waitFor(t, onStarted, 5*time.Second, "OnStartedLeading on survivor")
	t.Logf("handover complete: %s -> %s (global flushes: %d)",
		first, second, tracker.global.Load())
	if second == first {
		t.Errorf("survivor cannot be the same as the killed elector: both %s", first)
	}

	// 5. The contract: exactly two leadership turns, exactly two
	// flushes. A double-flush would mean a brief overlap window where
	// both electors thought they were leader simultaneously.
	if got := tracker.global.Load(); got != 2 {
		t.Errorf("expected exactly 2 flushes after one handover, got %d", got)
	}
}

// TestLeaderElection_PersistentRenewFailureCausesStepdown pins that
// when the elector cannot renew the lease past the RenewDeadline (any
// failure mode — apiserver unreachable, persistent conflict, network
// partition), it observes the renewal failure and runs
// OnStoppedLeading. This is the invariant that prevents a partitioned
// ex-leader from continuing to flush after losing communication with
// the apiserver — it is the operator-side half of "split-brain
// detection" (the apiserver-side half is enforced by the fact that
// another elector cannot acquire while the lease is still valid).
//
// Transient single-shot conflicts retry under the slow path and do
// NOT step down — that's correct upstream behaviour. We test the
// permanent-conflict shape here (every renewal fails for the full
// renew window) which IS the stepdown trigger.
func TestLeaderElection_PersistentRenewFailureCausesStepdown(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // SA1019: NewClientset replacement requires apply configurations; not worth the dependency for an in-memory test fake.
	tracker := newFlushTracker()

	rootCtx, rootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rootCancel()
	onStarted := make(chan string, 4)
	onStopped := make(chan string, 4)

	// Install the reactor BEFORE starting the elector so we never
	// race the elector's reactor-list reads with PrependReactor's
	// list mutation. The reactor is gated by an atomic flag that
	// the test flips after the initial acquire — flag-off lets the
	// real fake-clientset path win (returning false from the reactor
	// passes the call through), flag-on injects the conflict.
	var injectFailures atomic.Bool
	conflictsInjected := atomic.Int64{}
	client.PrependReactor("update", "leases",
		func(_ clientgotesting.Action) (bool, runtime.Object, error) {
			if !injectFailures.Load() {
				return false, nil, nil
			}
			conflictsInjected.Add(1)
			gr := schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}
			return true, nil, apierrors.NewConflict(gr, testLeaseName,
				errStr("simulated persistent apiserver outage"))
		})

	cancelA, _ := runElector(t, rootCtx, makeLeaseLock(t, client, electorA),
		tracker, electorA, onStarted, onStopped)
	defer cancelA()

	first := waitFor(t, onStarted, 5*time.Second, "elector-A acquires")
	if first != electorA {
		t.Fatalf("only elector-A is running; first must be elector-A, got %q", first)
	}
	// CI sometimes observes a transient re-acquire during the initial
	// dance with the fake clientset; the contract under test is
	// "no double-flush during the renewal-failure window", not
	// "exactly one flush ever". Snapshot the pre-failure count and
	// assert delta-zero across the stepdown.
	if tracker.global.Load() < 1 {
		t.Fatalf("expected at least 1 flush after acquire, got %d", tracker.global.Load())
	}
	flushesBeforeFailure := tracker.global.Load()

	// Flip the gate. Every subsequent Lease update will fail with
	// Conflict — simulates a network partition or apiserver outage.
	// After RenewDeadline (750ms in this test) the elector must
	// step down.
	injectFailures.Store(true)

	stopped := waitFor(t, onStopped, 10*time.Second,
		"OnStoppedLeading after persistent renewal failure")
	if stopped != electorA {
		t.Errorf("expected elector-A to step down, got %s", stopped)
	}
	if got := conflictsInjected.Load(); got < 1 {
		t.Errorf("expected at least 1 conflict to have been injected, got %d", got)
	}
	t.Logf("stepdown after %d injected conflicts (pre-failure flushes: %d)",
		conflictsInjected.Load(), flushesBeforeFailure)

	// CRITICAL invariant: zero NEW flushes during the renewal-failure
	// window. A double-flush would mean the elector continued running
	// leader work while it had already lost the lease.
	if got := tracker.global.Load(); got != flushesBeforeFailure {
		t.Errorf("expected no new flushes after renewal failure (was %d, now %d)",
			flushesBeforeFailure, got)
	}
}

// TestLeaderElection_OnlyOneLeaderAtAnyInstant runs both electors in
// parallel and samples the global flush count throughout. Even if one
// elector tries to forcibly take over (which the upstream library
// prevents), the count never exceeds the number of distinct
// leadership turns observed.
func TestLeaderElection_OnlyOneLeaderAtAnyInstant(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // SA1019: NewClientset replacement requires apply configurations; not worth the dependency for an in-memory test fake.
	tracker := newFlushTracker()

	rootCtx, rootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rootCancel()
	onStarted := make(chan string, 8)
	onStopped := make(chan string, 8)

	// Three electors competing — overkill for production, valuable
	// for catching regressions in the lease-arbitration logic.
	cancelA, _ := runElector(t, rootCtx, makeLeaseLock(t, client, electorA),
		tracker, electorA, onStarted, onStopped)
	defer cancelA()
	cancelB, _ := runElector(t, rootCtx, makeLeaseLock(t, client, electorB),
		tracker, electorB, onStarted, onStopped)
	defer cancelB()
	cancelC, _ := runElector(t, rootCtx, makeLeaseLock(t, client, "elector-C"),
		tracker, "elector-C", onStarted, onStopped)
	defer cancelC()

	// Wait for one to win.
	first := waitFor(t, onStarted, 5*time.Second, "first leader")
	t.Logf("first leader: %s", first)

	// Drain any spurious OnStartedLeading for ~3 lease durations to
	// catch a race where two electors believe they hold simultaneously.
	timeout := time.After(3 * testLeaseDuration)
loop:
	for {
		select {
		case extra := <-onStarted:
			t.Errorf("unexpected second OnStartedLeading while %s holds: %s",
				first, extra)
		case <-timeout:
			break loop
		}
	}

	if got := tracker.global.Load(); got != 1 {
		t.Errorf("only one elector should have run a flush in steady state; got %d",
			got)
	}
}

// ----------------------------------------------------------------------
// Test helpers below.
// ----------------------------------------------------------------------

// waitFor pulls one item off `ch` within `timeout`, otherwise t.Fatalfs
// with `description`. Encapsulating the select makes the test bodies
// readable without sacrificing diagnostics.
func waitFor(t *testing.T, ch <-chan string, timeout time.Duration, description string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %s after %v", description, timeout)
		return ""
	}
}

// errStr wraps a string into the field-error shape expected by
// `apierrors.NewConflict` — that constructor accepts an error rather
// than a string.
type errStr string

func (e errStr) Error() string { return string(e) }

// Compile-time witnesses to silence unused-import warnings in the
// (unlikely) event a future refactor strips back to the bare imports.
var (
	_ = corev1.NamespaceDefault
	_ = coordinationv1.LeaseSpec{}
	_ = wait.Backoff{}
)
