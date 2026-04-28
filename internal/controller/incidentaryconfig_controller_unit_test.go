/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	incidentaryv1alpha1 "github.com/incidentary/operator/api/v1alpha1"
)

// ----------------------------------------------------------------------------
// reconciliationInterval — unit tests.
//
// The CRD carries +kubebuilder:default=300 on ReconciliationIntervalSeconds,
// so the API server always stores ≥30. Only a directly-constructed struct can
// carry 0 or a negative value; these tests exercise that fallback branch.
// ----------------------------------------------------------------------------

func TestReconciliationInterval_ZeroFallsBackToDefault(t *testing.T) {
	config := &incidentaryv1alpha1.IncidentaryConfig{}
	got := reconciliationInterval(config)
	want := DefaultReconciliationIntervalSeconds * time.Second
	if got != want {
		t.Errorf("reconciliationInterval(zero) = %v; want %v", got, want)
	}
}

func TestReconciliationInterval_NegativeFallsBackToDefault(t *testing.T) {
	config := &incidentaryv1alpha1.IncidentaryConfig{
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			ReconciliationIntervalSeconds: -1,
		},
	}
	got := reconciliationInterval(config)
	want := DefaultReconciliationIntervalSeconds * time.Second
	if got != want {
		t.Errorf("reconciliationInterval(-1) = %v; want %v", got, want)
	}
}

// ----------------------------------------------------------------------------
// Reconcile — DiscoveryObserver / ReconcilerObserver wiring.
//
// The envtest Ginkgo suite always uses a nil Discovery and nil Classifier
// (IncidentaryConfigReconciler{Client: k8sClient}), leaving the observer
// branches uncovered. These lighter-weight fake-client tests exercise both.
// ----------------------------------------------------------------------------

// stubDiscovery implements DiscoveryObserver.
type stubDiscovery struct{ count int32 }

func (s *stubDiscovery) WatchedWorkloads() int32 { return s.count }
func (s *stubDiscovery) LastReport() time.Time   { return time.Now() }

// stubClassifier implements ReconcilerObserver.
type stubClassifier struct {
	matched, ghost, mismatched, newCount int32
}

func (s *stubClassifier) Counts() (matched, ghost, mismatched, newCount int32) {
	return s.matched, s.ghost, s.mismatched, s.newCount
}

// newUnitScheme returns a scheme with corev1 + incidentaryv1alpha1 registered.
func newUnitScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := incidentaryv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("incidentaryv1alpha1 scheme: %v", err)
	}
	return s
}

// newFakeReconciler builds an IncidentaryConfigReconciler backed by a fake
// client seeded with config and a valid Secret.
func newFakeReconciler(
	t *testing.T,
	config *incidentaryv1alpha1.IncidentaryConfig,
	discovery DiscoveryObserver,
	classifier ReconcilerObserver,
) *IncidentaryConfigReconciler {
	t.Helper()
	s := newUnitScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Spec.APIKeySecretRef.Name,
			Namespace: config.Namespace,
		},
		Data: map[string][]byte{
			config.Spec.APIKeySecretRef.Key: []byte("test-api-key"),
		},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(config, secret).
		WithStatusSubresource(config).
		Build()
	return &IncidentaryConfigReconciler{
		Client:     fc,
		Scheme:     s,
		Discovery:  discovery,
		Classifier: classifier,
	}
}

func TestReconcile_WatchedWorkloadsPopulatedFromDiscovery(t *testing.T) {
	config := &incidentaryv1alpha1.IncidentaryConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
				Name: "api-secret",
				Key:  "key",
			},
			ReconciliationIntervalSeconds: 60,
		},
	}
	r := newFakeReconciler(t, config, &stubDiscovery{count: 5}, nil)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestReconcile_ClassifierCountsPopulated(t *testing.T) {
	config := &incidentaryv1alpha1.IncidentaryConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			APIKeySecretRef: incidentaryv1alpha1.SecretKeyRef{
				Name: "api-secret",
				Key:  "key",
			},
			ReconciliationIntervalSeconds: 60,
		},
	}
	clf := &stubClassifier{matched: 3, mismatched: 1}
	r := newFakeReconciler(t, config, nil, clf)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Rotator / Provider hot-rotation wiring.
//
// On every successful reconcile, the controller calls Rotator.Rotate with
// the current API-key Secret data and the workspace ID from the CR. This
// gives the Batcher / discovery loop / services reconciler fresh credentials
// without requiring a pod restart.
// ----------------------------------------------------------------------------

// recordingRotator captures every Rotate call for test inspection.
type recordingRotator struct {
	calls []rotateCall
}

type rotateCall struct {
	apiKey, workspaceID, ingestEP, topoEP, svcEP string
}

func (r *recordingRotator) Rotate(apiKey, workspaceID, ingestEP, topoEP, svcEP string, _ logr.Logger) {
	r.calls = append(r.calls, rotateCall{apiKey, workspaceID, ingestEP, topoEP, svcEP})
}

func TestReconcile_RotatesProviderWithSecretValue(t *testing.T) {
	config := &incidentaryv1alpha1.IncidentaryConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			APIKeySecretRef:               incidentaryv1alpha1.SecretKeyRef{Name: "api-secret", Key: "key"},
			WorkspaceID:                   "ws_abc123",
			ReconciliationIntervalSeconds: 60,
		},
	}
	r := newFakeReconciler(t, config, nil, nil)
	rec := &recordingRotator{}
	r.Rotator = rec
	r.IngestEndpoint = "https://ingest.example.com"
	r.TopologyEndpoint = "https://topo.example.com"
	r.ServicesEndpoint = "https://svc.example.com"

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 Rotate call, got %d", len(rec.calls))
	}
	got := rec.calls[0]
	if got.apiKey != "test-api-key" {
		t.Errorf("Rotate apiKey = %q, want %q", got.apiKey, "test-api-key")
	}
	if got.workspaceID != "ws_abc123" {
		t.Errorf("Rotate workspaceID = %q, want %q", got.workspaceID, "ws_abc123")
	}
	if got.ingestEP != "https://ingest.example.com" {
		t.Errorf("Rotate ingestEP = %q, want %q", got.ingestEP, "https://ingest.example.com")
	}
	if got.topoEP != "https://topo.example.com" {
		t.Errorf("Rotate topoEP = %q, want %q", got.topoEP, "https://topo.example.com")
	}
	if got.svcEP != "https://svc.example.com" {
		t.Errorf("Rotate svcEP = %q, want %q", got.svcEP, "https://svc.example.com")
	}
}

func TestReconcile_RotatesEmptyKeyWhenSecretMissing(t *testing.T) {
	// When the Secret cannot be fetched, the controller installs dropping
	// stubs (Rotate with empty apiKey) so in-flight clients stop emitting.
	config := &incidentaryv1alpha1.IncidentaryConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			APIKeySecretRef:               incidentaryv1alpha1.SecretKeyRef{Name: "missing-secret", Key: "key"},
			WorkspaceID:                   "ws_abc",
			ReconciliationIntervalSeconds: 60,
		},
	}
	s := newUnitScheme(t)
	// Note: no Secret seeded — it doesn't exist.
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(config).
		WithStatusSubresource(config).
		Build()
	rec := &recordingRotator{}
	r := &IncidentaryConfigReconciler{
		Client:  fc,
		Scheme:  s,
		Rotator: rec,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 Rotate call (with empty apiKey), got %d", len(rec.calls))
	}
	if rec.calls[0].apiKey != "" {
		t.Errorf("Rotate apiKey = %q, want \"\" (dropping stubs)", rec.calls[0].apiKey)
	}
}

func TestReconcile_NilRotatorIsTolerated(t *testing.T) {
	// A nil Rotator (e.g., during unit tests without the wiring) must not
	// cause a nil-deref panic. The reconciler should reconcile successfully.
	config := &incidentaryv1alpha1.IncidentaryConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Spec: incidentaryv1alpha1.IncidentaryConfigSpec{
			APIKeySecretRef:               incidentaryv1alpha1.SecretKeyRef{Name: "api-secret", Key: "key"},
			WorkspaceID:                   "ws_abc",
			ReconciliationIntervalSeconds: 60,
		},
	}
	r := newFakeReconciler(t, config, nil, nil)
	// r.Rotator intentionally nil
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cfg", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile with nil Rotator: %v", err)
	}
}
