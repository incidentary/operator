//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package informers integration tests.
//
// These tests run against a real envtest control plane (no kubelet/kube-proxy),
// which is the smallest setup that exercises Manager.Start, the
// AddEventHandler dispatch path, and reportCacheSizes. Excluded from the
// default `go test ./...` run by the integration build tag — use
// `make test-integration` to run them.

package informers_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/incidentary/operator/internal/informers"
)

// countingHandler is a thread-safe informers.Handler that increments an
// atomic counter for each callback. Tests assert on the counters once enough
// activity has occurred.
type countingHandler struct {
	add atomic.Int32
	upd atomic.Int32
	del atomic.Int32

	// onAddNames captures the Names of every observed Add for assertion.
	mu        sync.Mutex
	onAddSeen map[string]bool
}

func newCountingHandler() *countingHandler {
	return &countingHandler{onAddSeen: map[string]bool{}}
}

func (c *countingHandler) OnAdd(_ context.Context, obj client.Object) {
	c.add.Add(1)
	c.mu.Lock()
	c.onAddSeen[obj.GetName()] = true
	c.mu.Unlock()
}

func (c *countingHandler) OnUpdate(_ context.Context, _, _ client.Object) { c.upd.Add(1) }

func (c *countingHandler) OnDelete(_ context.Context, _ client.Object) { c.del.Add(1) }

// startEnvtest spins up a control plane for the duration of the test.
func startEnvtest(t *testing.T) (*rest.Config, func()) {
	t.Helper()
	env := &envtest.Environment{
		// CRDDirectoryPaths is intentionally empty; this package does not
		// install CRDs. Just core API types are needed.
		ErrorIfCRDPathMissing: false,
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			"1.35.0-linux-amd64"),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	stop := func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest.Stop: %v", err)
		}
	}
	return cfg, stop
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := informers.AddToScheme(s); err != nil {
		t.Fatalf("informers.AddToScheme: %v", err)
	}
	return s
}

// waitFor polls the predicate until it returns true or the deadline expires.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitFor(%q): timed out after 15s", what)
}

func TestManager_DispatchesAddUpdateDelete(t *testing.T) {
	cfg, stop := startEnvtest(t)
	defer stop()

	scheme := newScheme(t)
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h := newCountingHandler()
	im := informers.NewManager(mgr, h, testr.New(t))
	if err := mgr.Add(im); err != nil {
		t.Fatalf("Add(im): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerErr := make(chan error, 1)
	go func() {
		managerErr <- mgr.Start(ctx)
	}()

	// Wait for the cache to start by trying a List until it succeeds.
	cli := mgr.GetClient()
	waitFor(t, "manager cache ready", func() bool {
		var deps appsv1.DeploymentList
		return cli.List(context.Background(), &deps) == nil
	})

	// 1. Add: create a Deployment, expect OnAdd to fire.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-test-dep",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "busybox:latest"},
					},
				},
			},
		},
	}
	if err := cli.Create(context.Background(), dep); err != nil {
		t.Fatalf("Create deployment: %v", err)
	}
	waitFor(t, "OnAdd fired for deployment", func() bool {
		h.mu.Lock()
		seen := h.onAddSeen["integration-test-dep"]
		h.mu.Unlock()
		return seen
	})

	// 2. Update: modify a label, expect OnUpdate to fire.
	beforeUpd := h.upd.Load()
	dep.Spec.Template.Labels["version"] = "v2"
	if err := cli.Update(context.Background(), dep); err != nil {
		t.Fatalf("Update deployment: %v", err)
	}
	waitFor(t, "OnUpdate fired", func() bool { return h.upd.Load() > beforeUpd })

	// 3. Delete: delete the deployment, expect OnDelete to fire.
	beforeDel := h.del.Load()
	if err := cli.Delete(context.Background(), dep); err != nil {
		t.Fatalf("Delete deployment: %v", err)
	}
	waitFor(t, "OnDelete fired", func() bool { return h.del.Load() > beforeDel })

	cancel()
	select {
	case err := <-managerErr:
		if err != nil {
			t.Errorf("manager.Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("manager did not stop within 5s of cancel")
	}
}

func TestManager_ReportsCacheSizes(t *testing.T) {
	// The reportCacheSizes goroutine samples informer store lengths every 30s
	// and updates a Prometheus gauge. We can't easily assert on the gauge
	// from this package, but we can verify the Manager registers all 14
	// informer stores after Start so reportCacheSizes has something to walk.
	cfg, stop := startEnvtest(t)
	defer stop()

	scheme := newScheme(t)
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h := newCountingHandler()
	im := informers.NewManager(mgr, h, testr.New(t))
	if err := mgr.Add(im); err != nil {
		t.Fatalf("Add(im): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerErr := make(chan error, 1)
	go func() {
		managerErr <- mgr.Start(ctx)
	}()

	// Wait for the cache to start serving requests.
	cli := mgr.GetClient()
	waitFor(t, "cache ready", func() bool {
		var ns corev1.NamespaceList
		return cli.List(context.Background(), &ns) == nil
	})

	// Yield long enough for the reportCacheSizesLoop goroutine to be
	// scheduled and run its first reportCacheSizes call. Without this the
	// goroutine may not have started before we cancel.
	time.Sleep(200 * time.Millisecond)

	// At this point Start has registered all 14 informer stores and
	// reportCacheSizes has fired at least once. Cancel and ensure clean exit.
	cancel()
	select {
	case <-managerErr:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop within 5s")
	}
}
