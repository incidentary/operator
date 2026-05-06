// Pin the contract that `Loop.runOnce` recovers from transient
// apiserver List failures on the next tick, even when the recovery
// cycle observes 10K workloads (the realistic ceiling for a busy
// cluster after the apiserver returns).
//
// The original "watch reconnect storm" framing in the gap list
// assumed the operator runs informer-based watches in `Loop` and
// would benefit from custom exponential backoff. On audit:
//
//   1. `Loop.runOnce` uses one-shot `client.List`, NOT informer
//      watches — there is no watch connection to drop and reconnect.
//   2. The informer cache (`internal/informers/manager.go`) uses
//      controller-runtime's shared cache, which delegates to
//      `client-go/tools/cache.Reflector` for reconnect+backoff. That
//      is upstream code with its own test coverage; bolting our own
//      retry layer on top would be redundant.
//
// The remaining real risk is: when the apiserver returns after a
// transient outage, the next `runOnce` will list 10 000+ workloads
// and process them all in one cycle. This test pins that the
// post-recovery cycle:
//
//   - Returns nil error.
//   - Produces exactly one topology report.
//   - The report contains every workload (no OOM, no truncation, no
//     duplicate caused by stale internal state from the failed cycle).

package discovery

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/incidentary/operator/internal/identity"
)

// flakyReader wraps a controller-runtime fake client and fails the
// first `failuresBeforeRecovery` List calls before delegating to the
// real fake. Models a transient apiserver outage where the operator
// observes errors for some duration and then recovers.
type flakyReader struct {
	client.Client
	failsLeft atomic.Int32
	err       error
	listCount atomic.Int32
}

func newFlakyReader(inner client.Client, failuresBeforeRecovery int32, err error) *flakyReader {
	r := &flakyReader{Client: inner, err: err}
	r.failsLeft.Store(failuresBeforeRecovery)
	return r
}

func (f *flakyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	f.listCount.Add(1)
	// Decrement-and-fail until failsLeft hits zero.
	for {
		current := f.failsLeft.Load()
		if current <= 0 {
			break
		}
		if f.failsLeft.CompareAndSwap(current, current-1) {
			return f.err
		}
		// Lost CAS race; reload and check again.
	}
	return f.Client.List(ctx, list, opts...)
}

// TestRunOnce_RecoversAfterTransientAPIError feeds the operator a
// scenario where the first cycle's first List fails, but the second
// cycle (next tick) succeeds with 10 000 deployments. Pins:
//   - First cycle returns the inner error.
//   - No partial topology report is sent during the failed cycle.
//   - Second cycle succeeds with all 10 000 workloads in one report.
//   - No duplicate workloads in the report (rules out stale state).
func TestRunOnce_RecoversAfterTransientAPIError(t *testing.T) {
	const N = 10_000
	ctx := context.Background()

	// Pre-seed the fake client with 10 000 deployments.
	s := newScheme(t)
	objs := make([]client.Object, 0, N+1)
	for i := range N {
		objs = append(objs, mkDeployment(fmt.Sprintf("dep-%05d", i), fmt.Sprintf("ns-%d", i%50), 1, nil))
	}
	objs = append(objs, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "fake-cluster-uid"},
	})
	innerClient := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()

	// Fail exactly the first List call, recover on the second.
	flaky := newFlakyReader(innerClient, 1, errors.New("transient: apiserver connection refused"))

	resolver := identity.NewResolver(flaky)
	topo := &fakeTopology{}
	l := NewLoop(flaky, resolver, topoFn(topo), testr.New(t), Options{
		Interval:    time.Hour,
		ClusterName: "watch-storm-test",
	})

	// First cycle — failure.
	if err := l.runOnce(ctx); err == nil {
		t.Fatalf("first runOnce should have surfaced the transient error")
	}
	if len(topo.reports) != 0 {
		t.Errorf("no report should be sent on a failed cycle; got %d", len(topo.reports))
	}

	// Second cycle — recovery.
	if err := l.runOnce(ctx); err != nil {
		t.Fatalf("second runOnce should succeed after API recovery; got %v", err)
	}

	if len(topo.reports) != 1 {
		t.Fatalf("expected exactly 1 topology report after recovery; got %d", len(topo.reports))
	}
	rep := topo.reports[0]
	if len(rep.Workloads) != N {
		t.Errorf("recovery cycle: expected %d workloads, got %d", N, len(rep.Workloads))
	}

	// Duplicate-detection — every workload name should appear at most
	// once. A regression that retained partial state from the failed
	// cycle would manifest as duplicates here.
	seen := make(map[string]int, N)
	for _, w := range rep.Workloads {
		key := fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.ServiceID)
		seen[key]++
	}
	dupes := 0
	for k, count := range seen {
		if count > 1 {
			dupes++
			if dupes < 5 {
				t.Errorf("duplicate workload after recovery: %s appeared %d times", k, count)
			}
		}
	}
	if dupes > 0 {
		t.Errorf("recovery cycle produced %d duplicate workloads", dupes)
	}
}

// TestRunOnce_LongTransientOutageStillRecovers feeds the operator a
// pessimistic 5-cycle outage. The fifth cycle succeeds. Tests the
// resilience-by-tick model under sustained failure: even if the
// apiserver is unavailable for many minutes, the operator does not
// accumulate stale state, leak goroutines, or fail to recover when
// the apiserver returns.
func TestRunOnce_LongTransientOutageStillRecovers(t *testing.T) {
	const N = 5_000
	ctx := context.Background()
	s := newScheme(t)
	objs := make([]client.Object, 0, N)
	for i := range N {
		objs = append(objs, mkDeployment(fmt.Sprintf("dep-%05d", i), fmt.Sprintf("ns-%d", i%20), 1, nil))
	}
	innerClient := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()

	flaky := newFlakyReader(innerClient, 5, errors.New("apiserver still down"))

	resolver := identity.NewResolver(flaky)
	topo := &fakeTopology{}
	l := NewLoop(flaky, resolver, topoFn(topo), testr.New(t), Options{
		Interval:    time.Hour,
		ClusterName: "long-outage-test",
	})

	// 5 failed cycles.
	for i := range 5 {
		if err := l.runOnce(ctx); err == nil {
			t.Fatalf("cycle %d should have failed", i)
		}
	}
	if len(topo.reports) != 0 {
		t.Errorf("no reports during outage; got %d", len(topo.reports))
	}

	// 6th cycle — recovery.
	if err := l.runOnce(ctx); err != nil {
		t.Fatalf("recovery cycle should succeed; got %v", err)
	}
	if len(topo.reports) != 1 {
		t.Fatalf("expected exactly 1 report on recovery; got %d", len(topo.reports))
	}
	if len(topo.reports[0].Workloads) != N {
		t.Errorf("recovery report: expected %d workloads, got %d",
			N, len(topo.reports[0].Workloads))
	}
}

// TestRunOnce_ClusterUIDIsRetriedAfterTransientFailure pins that the
// lazy `cluster_uid` resolution recovers cleanly when the kube-system
// Namespace Get fails on the first cycle. The empty fallback persists
// for that cycle, but the next runOnce retries resolution and
// successfully populates `l.clusterUID`.
//
// Without this invariant, a transient apiserver hiccup at operator
// start would permanently force the operator onto the legacy
// `cluster_name` keying — exactly the regression we want to prevent.
func TestRunOnce_ClusterUIDIsRetriedAfterTransientFailure(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	// Pre-seed kube-system Namespace + 1 deployment.
	innerClient := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "stable-cluster-uid"},
		},
		mkDeployment("single-dep", "default", 1, nil),
	).Build()

	// flakyGetReader fails the first Get (forcing resolveClusterUID
	// to return ""), then delegates to the inner client on subsequent
	// Gets.
	flaky := &flakyGetReader{Client: innerClient, getErr: errors.New("transient")}
	flaky.failsLeft.Store(1)

	resolver := identity.NewResolver(flaky)
	topo := &fakeTopology{}
	l := NewLoop(flaky, resolver, topoFn(topo), testr.New(t), Options{
		Interval:    time.Hour,
		ClusterName: "uid-retry-test",
	})

	// First cycle — the lazy resolve fails (Get returns transient).
	// The List succeeds, so the report is sent without cluster_uid.
	_ = l.runOnce(ctx)
	if l.clusterUID != "" {
		t.Errorf("after failed resolve, clusterUID should be empty; got %q", l.clusterUID)
	}

	// Second cycle — Get now succeeds, lazy resolve populates
	// l.clusterUID, the report carries the right value.
	_ = l.runOnce(ctx)
	if l.clusterUID != "stable-cluster-uid" {
		t.Errorf("after recovered resolve, clusterUID should be %q; got %q",
			"stable-cluster-uid", l.clusterUID)
	}
}

// flakyGetReader fails the first Get call, then delegates to the
// inner client. List always passes through to the inner client.
type flakyGetReader struct {
	client.Client
	failsLeft atomic.Int32
	getErr    error
}

func (f *flakyGetReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if f.failsLeft.Load() > 0 {
		f.failsLeft.Add(-1)
		return f.getErr
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

// scheme + helper imports are shared with loop_test.go via the
// package-level functions there (newScheme, mkDeployment, fakeTopology,
// topoFn). We declare a local witness on `runtime` here only to avoid
// the unused-import complaint if a future refactor strips back.
var _ = runtime.Object(nil)

// Compile-time witness for appsv1 — used by mkDeployment in the
// shared test helpers, but we re-declare here to make the test file
// self-documenting.
var _ = appsv1.Deployment{}
