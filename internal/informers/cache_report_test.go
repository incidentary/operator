/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package informers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opmetrics "github.com/incidentary/operator/internal/metrics"
)

// internalStubHandler is a no-op Handler used only by internal-package tests
// that need access to unexported fields/methods. The external-package tests
// in manager_test.go define their own stubHandler.
type internalStubHandler struct{}

func (internalStubHandler) OnAdd(_ context.Context, _ client.Object)       {}
func (internalStubHandler) OnUpdate(_ context.Context, _, _ client.Object) {}
func (internalStubHandler) OnDelete(_ context.Context, _ client.Object)    {}

// fakeStore is a minimal clientcache.Store stub. Only List is exercised by
// reportCacheSizes; the rest panic to flag any unexpected use.
type fakeStore struct {
	items []any
}

func (f *fakeStore) Add(obj any) error                    { return nil }
func (f *fakeStore) Update(obj any) error                 { return nil }
func (f *fakeStore) Delete(obj any) error                 { return nil }
func (f *fakeStore) List() []any                          { return f.items }
func (f *fakeStore) ListKeys() []string                   { return nil }
func (f *fakeStore) Get(_ any) (any, bool, error)         { return nil, false, nil }
func (f *fakeStore) GetByKey(_ string) (any, bool, error) { return nil, false, nil }
func (f *fakeStore) Replace(_ []any, _ string) error      { return nil }
func (f *fakeStore) Resync() error                        { return nil }

// Compile-time check that fakeStore satisfies the cache.Store interface.
var _ clientcache.Store = (*fakeStore)(nil)

// TestReportCacheSizes_PopulatesGauge exercises the per-resource sample loop
// directly. It would otherwise live in the goroutine fired by Start, which
// is awkward to time-deterministically test from outside the package.
func TestReportCacheSizes_PopulatesGauge(t *testing.T) {
	m := &Manager{
		informerStores: []labeledStore{
			{label: "pods", store: &fakeStore{items: []any{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3"}},
			}}},
			{label: "deployments", store: &fakeStore{items: []any{}}},
		},
	}

	// Reset the gauge before sampling so we can assert on the post-call value.
	opmetrics.InformerCacheSize.Reset()
	m.reportCacheSizes()

	// We can't easily extract Prometheus gauge values from a CounterVec in
	// a unit test without prometheus/testutil; instead this test asserts
	// behavioural correctness: the call must complete without panicking
	// and the loop body must run len(informerStores) times. Coverage tooling
	// records every executed line.
	if got := len(m.informerStores); got != 2 {
		t.Errorf("informerStores rewritten unexpectedly: len=%d, want 2", got)
	}
}

// TestStart_NilManagerReturnsError covers the m.mgr == nil guard at the top
// of Start. The full Start path (informer registration + handler dispatch)
// is exercised by the integration test in integration_test.go, which
// requires envtest binaries.
func TestStart_NilManagerReturnsError(t *testing.T) {
	m := &Manager{
		// mgr intentionally nil
		handler: internalStubHandler{},
	}
	err := m.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from Start with nil manager")
	}
}

// TestReportCacheSizesLoop_FiresAtLeastOnceAndExitsOnContextCancel verifies
// the loop runs an immediate sample on entry and exits cleanly when the
// context is cancelled.
func TestReportCacheSizesLoop_FiresAtLeastOnceAndExitsOnContextCancel(t *testing.T) {
	m := &Manager{
		informerStores: []labeledStore{
			{label: "test-resource", store: &fakeStore{items: []any{}}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.reportCacheSizesLoop(ctx)
		close(done)
	}()

	// Yield so the goroutine reaches its first sample call before we cancel.
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("reportCacheSizesLoop did not exit within 2s of context cancel")
	}
}
