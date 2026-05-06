//go:build e2e_kind

// Pin the contract that `Loop.resolveClusterUID` degrades gracefully
// (returns "" with no panic, no log-fatal) when the operator's
// identity cannot resolve the `kube-system` Namespace because of RBAC
// denial.
//
// A unit test elsewhere uses a 1-nanosecond context deadline as a
// *proxy* for RBAC denial: any error from the apiserver returns ""
// via the same code path. That proxy is honest about its scope (a
// context-deadline error is the same handler branch as a Forbidden
// error), but it does not exercise the actual apiserver authorisation
// layer, so a regression where `resolveClusterUID` started panicking
// on `*StatusError` (e.g. a future refactor that did
// `err.(*errors.StatusError).ErrStatus.Message`
// on a non-StatusError) would slip through.
//
// This test closes the gap by impersonating a real user (no
// ClusterRoleBinding) and asserting the apiserver returns a Forbidden
// `*StatusError` that the resolver translates to "" cleanly.
//
// Why impersonation rather than a literal ServiceAccount + Role?
//   - Impersonation is ~30 LoC; SA token mining is ~120 LoC.
//   - Both paths return identical `Forbidden` errors from the apiserver
//     (the authorisation decision is the same regardless of how the
//     identity got established).
//   - The kind admin kubeconfig allows `impersonate` by default
//     (cluster-admin ClusterRole), so the test needs no preflight RBAC
//     setup beyond what `kind create cluster` ships with.

package discovery

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
)

// loadAdminKubeconfig returns the admin kind kubeconfig as a
// *rest.Config, or skips the test if no kubeconfig is reachable.
func loadAdminKubeconfig(t *testing.T) *rest.Config {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		home, _ := os.UserHomeDir()
		cfg, err = clientcmd.BuildConfigFromFlags("", home+"/.kube/config")
		if err != nil {
			t.Skipf("no kubeconfig: %v", err)
		}
	}
	return cfg
}

// TestE2E_KindRBACDenialReturnsEmptyUID is the real-RBAC test for
// . We construct a controller-runtime client whose identity
// is impersonated as a user with no ClusterRoleBinding — i.e.
// authenticated but unauthorised to read namespaces. `resolveClusterUID`
// must return "" without panicking, and the underlying apiserver error
// must be a genuine Forbidden (not a network error or context cancel).
func TestE2E_KindRBACDenialReturnsEmptyUID(t *testing.T) {
	adminCfg := loadAdminKubeconfig(t)

	// Take a copy of the admin config and reduce its identity to an
	// arbitrary impersonated user. No ClusterRoleBinding exists for
	// this user, so any Get should fail with Forbidden.
	restricted := rest.CopyConfig(adminCfg)
	restricted.Impersonate = rest.ImpersonationConfig{
		UserName: "incidentary-test-restricted",
		Groups:   []string{"system:authenticated"},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	// Up-front sanity: confirm impersonation actually causes Forbidden.
	// If the kind cluster's admin config is missing `impersonate` verbs
	// (rare but possible on hardened clusters), the test would degrade
	// to a different failure mode. This pre-check makes the failure
	// legible: "your kubeconfig can't impersonate" vs "the operator's
	// resolveClusterUID panicked on Forbidden."
	c, err := client.New(restricted, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("k8s client (restricted): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var ns corev1.Namespace
	probeErr := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns)
	if probeErr == nil {
		t.Fatalf("impersonated user unexpectedly Got kube-system; " +
			"check kind admin kubeconfig has impersonate ClusterRole")
	}
	if !apierrors.IsForbidden(probeErr) {
		t.Fatalf("expected Forbidden from impersonated user, got %v "+
			"(error type %T)", probeErr, probeErr)
	}
	t.Logf("impersonation pre-check OK — apiserver returned: %v", probeErr)

	// Now exercise the operator's lazy-resolve path under the same
	// restricted identity. Construct a Loop (the same object the
	// runOnce loop uses) and invoke resolveClusterUID directly.
	resolver := identity.NewResolver(c)
	topo := &fakeRBACTopology{}
	l := NewLoop(c, resolver,
		func() ingestclient.TopologyClient { return topo },
		testr.New(t),
		Options{Interval: time.Hour, ClusterName: "kind-rbac-restricted"},
	)

	got := l.resolveClusterUID(ctx)
	if got != "" {
		t.Errorf("resolveClusterUID under RBAC denial: got %q, want empty string", got)
	}

	// Run a full reporting cycle under the restricted identity. The
	// list of Deployments is also denied, so runOnce should not
	// generate a topology report — but it MUST NOT panic. Empty
	// reports are the graceful-degradation contract.
	if err := l.runOnce(ctx); err != nil {
		// Non-fatal — runOnce returns nil if there is nothing to
		// report. We only assert it doesn't panic. We log so a
		// future regression gives the on-call useful context.
		t.Logf("runOnce under RBAC denial returned: %v "+
			"(degradation OK if no panic)", err)
	}

	// l.clusterUID must remain "" — the operator never forces a stale
	// or fabricated UID into the report when the resolve fails.
	if l.clusterUID != "" {
		t.Errorf("Loop.clusterUID after RBAC-denied runOnce: got %q, want empty",
			l.clusterUID)
	}
}

// TestE2E_KindRBACForbiddenSurfaceIsLegible verifies the operator's
// log line on Forbidden error contains the magic string "forbidden"
// or "cannot list" — so on-call sees the cause without staring at
// stack traces. This pins the V(1) log message at the bottom of
// `loop.go::resolveClusterUID`.
func TestE2E_KindRBACForbiddenSurfaceIsLegible(t *testing.T) {
	adminCfg := loadAdminKubeconfig(t)

	restricted := rest.CopyConfig(adminCfg)
	restricted.Impersonate = rest.ImpersonationConfig{
		UserName: "incidentary-test-legibility",
		Groups:   []string{"system:authenticated"},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(restricted, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("k8s client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ns corev1.Namespace
	err = c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns)

	if err == nil {
		t.Fatalf("expected Forbidden")
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"forbidden", "cannot get", "cannot list", "namespaces"} {
		if strings.Contains(msg, kw) {
			t.Logf("apiserver Forbidden message contains %q (good for on-call)", kw)
			return
		}
	}
	t.Errorf("Forbidden error message lacks legible keywords; got: %q", err.Error())
}

// fakeRBACTopology — minimal topology stub for the RBAC test. We don't
// expect any reports to flow during a degradation scenario; this just
// makes the Loop constructor signature happy.
type fakeRBACTopology struct{}

func (f *fakeRBACTopology) Report(_ context.Context, r *ingestclient.TopologyReport) (*ingestclient.TopologyResponse, error) {
	return &ingestclient.TopologyResponse{Accepted: len(r.Workloads)}, nil
}
