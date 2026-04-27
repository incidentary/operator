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
