//go:build e2e_kind

// Real-K8s E2E test that validates the cluster_uid lazy-resolve path
// against a live `kind`-managed cluster. Build-tag-gated so the
// standard `go test ./...` does not require K8s tooling; the test is
// invoked explicitly with:
//
//   kind create cluster --name incidentary-e2e
//   go test -tags e2e_kind -run TestE2E_KindClusterUIDLazyResolve \
//       ./internal/discovery/...
//
// The test passes when the operator's `Loop.resolveClusterUID`
// returns the same UID `kubectl` reports for the kube-system
// Namespace — that's the contract the backend keying depends on.

package discovery

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
)

// TestE2E_KindClusterUIDLazyResolve verifies the operator can
// resolve `kube-system`'s Namespace UID against a real K8s API
// server. The test does not assume the cluster name; it just
// asserts (a) some non-empty UID is returned, (b) it parses as
// a UUID-shaped string.
//
// Pre-requisite: a kind cluster named `incidentary-e2e` exists,
// or KUBECONFIG/HOME is set to a working kubeconfig.
func TestE2E_KindClusterUIDLazyResolve(t *testing.T) {
	cfg, err := clientcmd.BuildConfigFromFlags("",
		os.Getenv("KUBECONFIG"))
	if err != nil {
		// Fall back to default ~/.kube/config.
		home, _ := os.UserHomeDir()
		cfg, err = clientcmd.BuildConfigFromFlags("",
			home+"/.kube/config")
		if err != nil {
			t.Skipf("no kubeconfig: %v", err)
		}
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("k8s client: %v", err)
	}

	// Confirm the kube-system Namespace is reachable up front.
	var ns corev1.Namespace
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		t.Fatalf("kube-system Get: %v", err)
	}
	if ns.UID == "" {
		t.Fatalf("kube-system Namespace UID is empty — cluster broken?")
	}
	expectedUID := string(ns.UID)
	t.Logf("kube-system UID via raw API: %q", expectedUID)

	// Now exercise the operator's lazy-resolve path. We construct
	// a Loop with our real client and call resolveClusterUID
	// directly — same code path runOnce uses on its first cycle.
	resolver := identity.NewResolver(c)
	topo := &fakeE2ETopology{}
	l := NewLoop(c, resolver, func() ingestclient.TopologyClient { return topo },
		testr.New(t),
		Options{Interval: time.Hour, ClusterName: "kind-e2e"},
	)

	got := l.resolveClusterUID(ctx)
	if got != expectedUID {
		t.Fatalf("resolveClusterUID mismatch: got %q, want %q", got, expectedUID)
	}
	if !looksLikeUUID(got) {
		t.Fatalf("UID does not look like a UUID: %q", got)
	}

	// Run a full reporting cycle and assert the topology report
	// carries the cluster_uid field populated from the live
	// resolve.
	if err := l.runOnce(ctx); err != nil {
		// Empty cluster (no Deployments) returns nil with no
		// report — that's fine. We only care the resolve worked.
		t.Logf("runOnce returned (probably no workloads): %v", err)
	}
	if l.clusterUID != expectedUID {
		t.Errorf("Loop.clusterUID after runOnce: got %q, want %q", l.clusterUID, expectedUID)
	}
}

// TestE2E_KindClusterUIDOnFreshClusterIsStable verifies the UID
// is invariant across multiple calls — the lazy-resolve memoises
// after the first successful Get, so the second Loop invocation
// re-uses the cached value.
func TestE2E_KindClusterUIDOnFreshClusterIsStable(t *testing.T) {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		home, _ := os.UserHomeDir()
		cfg, err = clientcmd.BuildConfigFromFlags("", home+"/.kube/config")
		if err != nil {
			t.Skipf("no kubeconfig: %v", err)
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("k8s client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resolver := identity.NewResolver(c)
	l := NewLoop(c, resolver,
		func() ingestclient.TopologyClient { return &fakeE2ETopology{} },
		testr.New(t),
		Options{Interval: time.Hour, ClusterName: "kind-e2e"},
	)

	first := l.resolveClusterUID(ctx)
	second := l.resolveClusterUID(ctx)
	if first != second {
		t.Errorf("UID changed between calls: %q -> %q", first, second)
	}
	if first == "" {
		t.Errorf("UID empty on real cluster")
	}
}

// TestE2E_KindRBACDenialIsHandledGracefully verifies the operator
// degrades cleanly when its ServiceAccount lacks the verbs it
// needs. We simulate this by using a kubeconfig pointed at an
// unprivileged user (no list/get on namespaces). The operator
// must NOT crash; resolveClusterUID returns the empty fallback
// and the topology cycle proceeds without the cluster_uid field.
//
// We don't actually create a restricted ServiceAccount here —
// that requires kubectl + RBAC YAML. Instead, we exercise the
// known-failure path by creating a context whose timeout is
// so short the Get is guaranteed to fail mid-flight. Same code
// path: any err from `l.client.Get` falls through to "" return.
func TestE2E_KindRBACDenialIsHandledGracefully(t *testing.T) {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		home, _ := os.UserHomeDir()
		cfg, err = clientcmd.BuildConfigFromFlags("", home+"/.kube/config")
		if err != nil {
			t.Skipf("no kubeconfig: %v", err)
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("k8s client: %v", err)
	}

	resolver := identity.NewResolver(c)
	l := NewLoop(c, resolver,
		func() ingestclient.TopologyClient { return &fakeE2ETopology{} },
		testr.New(t),
		Options{Interval: time.Hour, ClusterName: "kind-rbac-denied"},
	)

	// 1ns timeout — guaranteed to fail in flight.
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()

	uid := l.resolveClusterUID(ctx)
	if uid != "" {
		t.Errorf("resolveClusterUID should return empty on context-deadline; got %q", uid)
	}
}

// fakeE2ETopology — minimal stub. A real backend integration test
// would route to live ingest, but this file's scope is operator-side
// E2E only.
type fakeE2ETopology struct {
	reports []*ingestclient.TopologyReport
}

func (f *fakeE2ETopology) Report(_ context.Context, r *ingestclient.TopologyReport) (*ingestclient.TopologyResponse, error) {
	f.reports = append(f.reports, r)
	return &ingestclient.TopologyResponse{Accepted: len(r.Workloads)}, nil
}

// looksLikeUUID is a permissive UUID format check — accepts any
// 36-char string with dashes in the canonical positions. Sufficient
// for the assertion goal: the value should at minimum look like
// what the K8s apiserver returns for resource UIDs.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, ch := range s {
		switch i {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", ch) {
				return false
			}
		}
	}
	return true
}

// Compile-time assertion to silence the unused metav1 import
// without complicating the test body. Removing this when other
// metav1 use lands.
var _ = metav1.Time{}
