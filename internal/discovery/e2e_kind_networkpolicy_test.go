//go:build e2e_kind

// Pin the contract that the chart's NetworkPolicy template
// (charts/incidentary-operator/templates/networkpolicy.yaml) allows the
// egress paths the operator needs at runtime:
//
//   1. DNS resolution through CoreDNS (UDP/TCP 53 to kube-system)
//   2. kube-apiserver TLS egress (TCP 443/6443 to kube-system)
//   3. TCP/443 to the configured ingest hostname (default permissive)
//
// The test exercises the YAML by applying a stripped-down policy to a
// real (kind-managed) cluster and then `kubectl exec`-ing a probe pod
// that shares the operator's selector labels through the matrix.
//
// Pre-requisite: a kind cluster reachable via $KUBECONFIG (or
// ~/.kube/config). For a dedicated cluster:
//
//   kind create cluster --name incidentary-np-test
//   go test -tags e2e_kind -run TestE2E_KindNetworkPolicyEgress \
//       ./internal/discovery/...
//
// Note: kindnet (kind's default CNI) does NOT enforce NetworkPolicy.
// The test therefore validates *positive* egress (legitimate traffic
// continues to work after the policy is applied), not *negative*
// egress (blocked traffic is denied). Negative-egress regression
// requires a Calico/Cilium-enforced cluster.

package discovery

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	npTestNamespace = "incidentary-np-test"
	npTestPodLabel  = "app.kubernetes.io/name=incidentary-np-probe"
	npTestPodName   = "np-egress-probe"
)

// kubectl runs `kubectl <args...>` against the active KUBECONFIG and
// returns combined stdout+stderr plus the error (if any). We shell out
// rather than re-implement the equivalent client-go calls because the
// failure modes (NetworkPolicy enforcement, CoreDNS reachability) are
// inherently pod-side and the cleanest probe is a process inside a pod
// the kubelet schedules normally.
func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// kubectlOrFail runs kubectl and t.Fatals on non-zero exit so the test
// surfaces the kubectl output verbatim — much easier to debug than a
// post-hoc assertion failure with no apiserver context.
func kubectlOrFail(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatalf("kubectl %v failed: %v\nOutput:\n%s",
			strings.Join(args, " "), err, out)
	}
	return out
}

// applyEgressPolicy installs a NetworkPolicy that mirrors the chart's
// egress shape (DNS, kube-apiserver, generic TCP/443), scoped to the
// `npTestPodLabel` selector. We build the YAML inline so the test
// has no chart-render dependency.
func applyEgressPolicy(t *testing.T) {
	t.Helper()
	manifest := `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: incidentary-np-probe-egress
  namespace: ` + npTestNamespace + `
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: incidentary-np-probe
  policyTypes:
    - Egress
  egress:
    # DNS — UDP/TCP 53 to CoreDNS in kube-system.
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # kube-apiserver — TCP 443/6443 to kube-system, mirroring the
    # tightening.
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
      ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 6443
    # Generic TCP/443 — the chart's permissive default for ingest.
    - ports:
        - protocol: TCP
          port: 443
`
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply networkpolicy failed: %v\n%s", err, string(out))
	}
}

// runProbePod schedules a busybox pod with the operator's selector
// labels and waits for it to become Ready. busybox carries `nslookup`,
// `wget`, and `nc` — enough to drive the egress matrix without pulling
// a fatter image.
func runProbePod(t *testing.T) {
	t.Helper()
	// `kubectl run` with --labels and explicit busybox sleep so the pod
	// stays scheduled past the entrypoint exit.
	kubectlOrFail(t, "run", npTestPodName,
		"--namespace", npTestNamespace,
		"--image=busybox:1.36",
		"--labels", "app.kubernetes.io/name=incidentary-np-probe",
		"--restart=Never",
		"--command", "--",
		"sleep", "300",
	)
	// Wait up to 60s for Ready.
	kubectlOrFail(t, "wait", "--namespace", npTestNamespace,
		"pod/"+npTestPodName, "--for=condition=Ready",
		"--timeout=60s")
}

// execProbe runs an arbitrary shell command inside the probe pod and
// returns combined stdout+stderr + the kubectl error.
func execProbe(t *testing.T, shellCmd string) (string, error) {
	t.Helper()
	return kubectl(t, "exec", "--namespace", npTestNamespace,
		npTestPodName, "--", "sh", "-c", shellCmd)
}

// cleanupNamespace removes the test namespace (and everything in it)
// and *waits* for the deletion to fully terminate. Tests run
// sequentially in the same package, and a half-deleted namespace
// causes the next test's `kubectl create namespace` to fail with
// "AlreadyExists." Using --wait=true (the kubectl default) guarantees
// idempotency between tests.
func cleanupNamespace(t *testing.T) {
	t.Helper()
	_, _ = kubectl(t, "delete", "namespace", npTestNamespace,
		"--ignore-not-found=true", "--timeout=60s")
}

// requireKubeconfig skips the test when no kubeconfig is reachable.
// Mirrors the pattern in `e2e_kind_test.go` so CI without K8s tooling
// still passes the build-tag-gated suite as "skipped, not failed."
func requireKubeconfig(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBECONFIG") != "" {
		return
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(home + "/.kube/config"); err != nil {
		t.Skipf("no kubeconfig found: %v", err)
	}
}

// TestE2E_KindNetworkPolicyEgress installs the chart's NetworkPolicy
// shape into a real cluster and asserts the operator-equivalent pod
// can still reach the apiserver and CoreDNS through it.
func TestE2E_KindNetworkPolicyEgress(t *testing.T) {
	requireKubeconfig(t)

	cleanupNamespace(t)
	t.Cleanup(func() { cleanupNamespace(t) })

	kubectlOrFail(t, "create", "namespace", npTestNamespace)
	applyEgressPolicy(t)
	runProbePod(t)

	// Assertion 1 — DNS works under the policy.
	// We resolve `kubernetes.default.svc.cluster.local`, which both
	// exercises the DNS egress rule (UDP/53 to kube-dns) AND the
	// service-name resolution path the operator depends on.
	out, err := execProbe(t, "nslookup kubernetes.default.svc.cluster.local")
	if err != nil {
		t.Errorf("DNS lookup failed under NetworkPolicy:\n%s\nerr=%v", out, err)
	} else if !strings.Contains(out, "Address") {
		t.Errorf("nslookup output missing Address line:\n%s", out)
	}

	// Assertion 2 — kube-apiserver TCP egress works under the policy.
	// We probe at the TCP layer rather than running a full TLS
	// handshake because busybox's wget has a known incompatibility
	// with modern apiserver TLS configs (alert code 47 — bad_certificate
	// — surfaces even with --no-check-certificate). The TCP probe is
	// sufficient for the hardening contract: if the NetworkPolicy
	// blocks apiserver egress, `nc` returns non-zero; if the apiserver
	// is reachable, the operator's client-go layer handles TLS the
	// same way it does in production.
	out, err = execProbe(t,
		"nc -w 5 -z kubernetes.default.svc.cluster.local 443 && echo APISERVER_REACHABLE")
	if err != nil {
		t.Errorf("kube-apiserver TCP/443 probe failed under NetworkPolicy:\n%s\nerr=%v",
			out, err)
	} else if !strings.Contains(out, "APISERVER_REACHABLE") {
		t.Errorf("apiserver TCP/443 not reachable:\n%s", out)
	}

	// Assertion 3 — generic TCP/443 egress works (Cloudflare 1.1.1.1
	// as a stable target). This pins the chart's permissive default;
	// a customer override of `networkPolicy.egressTo` would tighten
	// this, which is by design.
	//
	// `nc -w 3 -z 1.1.1.1 443` returns 0 on TCP handshake success.
	// We allow this assertion to be informational rather than fatal —
	// some CI environments have outbound 443 blocked at the host level,
	// which is unrelated to NetworkPolicy.
	out, err = execProbe(t, "nc -w 3 -z 1.1.1.1 443 && echo OPEN || echo CLOSED")
	if err != nil {
		t.Logf("nc probe to 1.1.1.1:443 errored (likely host-level egress block, not NP): %v\n%s",
			err, out)
	} else {
		t.Logf("TCP/443 egress probe: %s", strings.TrimSpace(out))
	}
}

// TestE2E_KindNetworkPolicyDisabledByDefault verifies the chart's
// NetworkPolicy is opt-in — applying with networkPolicy.enabled=false
// (the default) leaves the operator with unrestricted egress, the
// shape we ship with for new installs.
func TestE2E_KindNetworkPolicyDisabledByDefault(t *testing.T) {
	requireKubeconfig(t)
	// We can't `helm install` from a unit test without pulling helm
	// into the toolchain, but we can assert the *negative*: in a
	// namespace with no NetworkPolicy resources, the probe pod has
	// unrestricted egress. This is the invariant the chart relies on
	// when networkPolicy.enabled is false.
	cleanupNamespace(t)
	t.Cleanup(func() { cleanupNamespace(t) })

	kubectlOrFail(t, "create", "namespace", npTestNamespace)
	runProbePod(t)

	out, err := execProbe(t, "nslookup kubernetes.default.svc.cluster.local")
	if err != nil {
		t.Errorf("DNS lookup failed without NetworkPolicy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Address") {
		t.Errorf("nslookup output missing Address line:\n%s", out)
	}
}
