/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
)

type fakeTopology struct {
	reports []*ingestclient.TopologyReport
	err     error
}

func (f *fakeTopology) Report(_ context.Context, r *ingestclient.TopologyReport) (*ingestclient.TopologyResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.reports = append(f.reports, r)
	return &ingestclient.TopologyResponse{
		Accepted:             len(r.Workloads),
		CreatedGhostServices: len(r.Workloads),
		UpdatedServices:      0,
	}, nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("appsv1 AddToScheme: %v", err)
	}
	return s
}

func mkDeployment(name, ns string, replicas int32, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            labels,
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "ghcr.io/incidentary/" + name + ":v1"},
					},
				},
			},
		},
	}
}

func TestRunOnce_EmitsWorkloads(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	depA := mkDeployment("web", "prod", 3, nil)
	depB := mkDeployment("payments", "prod", 2, nil)
	system := mkDeployment("kube-proxy", "kube-system", 1, map[string]string{"k8s-app": "kube-proxy"})

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(depA, depB, system).Build()
	resolver := identity.NewResolver(c)
	topo := &fakeTopology{}

	l := NewLoop(c, resolver, topo, testr.New(t), Options{
		Interval:    time.Hour,
		ClusterName: "unit-test",
	})
	if err := l.runOnce(ctx); err != nil {
		t.Fatalf("runOnce err: %v", err)
	}

	if len(topo.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(topo.reports))
	}
	r := topo.reports[0]
	if r.ClusterName != "unit-test" {
		t.Errorf("cluster_name = %q", r.ClusterName)
	}
	// Should have web and payments but not kube-proxy (system workload) and
	// not anything in kube-system (excluded namespace).
	if len(r.Workloads) != 2 {
		t.Fatalf("workloads = %d, want 2 (web, payments)", len(r.Workloads))
	}
	names := map[string]bool{}
	for _, w := range r.Workloads {
		names[w.ServiceID] = true
		if w.ServiceIDSource == "" {
			t.Errorf("workload %q missing service_id_source", w.ServiceID)
		}
		if w.Image == "" {
			t.Errorf("workload %q missing image", w.ServiceID)
		}
	}
	if !names["web"] || !names["payments"] {
		t.Errorf("missing expected workloads, got %+v", names)
	}
	if names["kube-proxy"] {
		t.Errorf("system workload should not appear in report")
	}

	if l.WatchedWorkloads() != 2 {
		t.Errorf("WatchedWorkloads = %d, want 2", l.WatchedWorkloads())
	}
	if l.LastReport().IsZero() {
		t.Errorf("LastReport should be set after successful run")
	}
}

func TestRunOnce_ExcludesNamespaces(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	ignored := mkDeployment("worker", "ignored-ns", 1, nil)
	kept := mkDeployment("web", "prod", 1, nil)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ignored, kept).Build()
	resolver := identity.NewResolver(c)
	topo := &fakeTopology{}

	l := NewLoop(c, resolver, topo, testr.New(t), Options{
		Interval:          time.Hour,
		ExcludeNamespaces: []string{"ignored-ns"},
	})
	if err := l.runOnce(ctx); err != nil {
		t.Fatalf("runOnce err: %v", err)
	}
	if len(topo.reports) != 1 {
		t.Fatalf("reports = %d", len(topo.reports))
	}
	if len(topo.reports[0].Workloads) != 1 || topo.reports[0].Workloads[0].ServiceID != "web" {
		t.Errorf("expected only web, got %+v", topo.reports[0].Workloads)
	}
}

func TestRunOnce_NoWorkloads(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	topo := &fakeTopology{}
	l := NewLoop(c, identity.NewResolver(c), topo, testr.New(t), Options{Interval: time.Hour})

	if err := l.runOnce(ctx); err != nil {
		t.Fatalf("runOnce err: %v", err)
	}
	if len(topo.reports) != 0 {
		t.Errorf("empty cluster should not emit a report, got %d", len(topo.reports))
	}
}

func TestRunOnce_TopologyError(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	d := mkDeployment("web", "prod", 1, nil)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	topo := &fakeTopology{err: errors.New("boom")}
	l := NewLoop(c, identity.NewResolver(c), topo, testr.New(t), Options{Interval: time.Hour})

	if err := l.runOnce(ctx); err == nil {
		t.Fatal("expected error")
	}
	// LastReport should remain zero because the cycle did not succeed.
	if !l.LastReport().IsZero() {
		t.Errorf("LastReport should be zero after failed report")
	}
}

func TestLoop_NeedLeaderElection(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	l := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	if !l.NeedLeaderElection() {
		t.Error("NeedLeaderElection should be true")
	}
}

// -----------------------------------------------------------------------------
// StatefulSet and DaemonSet collection
// -----------------------------------------------------------------------------

func mkStatefulSet(name, ns string, image string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: image}},
				},
			},
		},
	}
}

func mkDaemonSet(name, ns string, image string, labels map[string]string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: image}},
				},
			},
		},
	}
}

func TestRunOnce_IncludesStatefulSetAndDaemonSet(t *testing.T) {
	s := newScheme(t)
	dep := mkDeployment("web", "prod", 2, nil)
	sts := mkStatefulSet("db", "prod", "postgres:15")
	ds := mkDaemonSet("fluentd", "prod", "fluent/fluentd:v1", nil)
	// kube-proxy should be filtered by isSystemWorkload (k8s-app label).
	sysDaemon := mkDaemonSet("kube-proxy", "kube-system", "kube-proxy:v1",
		map[string]string{"k8s-app": "kube-proxy"})

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, sts, ds, sysDaemon).Build()
	topo := &fakeTopology{}
	l := NewLoop(c, identity.NewResolver(c), topo, testr.New(t), Options{
		Interval: time.Hour,
	})

	if err := l.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce error: %v", err)
	}

	if len(topo.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(topo.reports))
	}
	workloads := topo.reports[0].Workloads

	kinds := map[string]string{}
	for _, w := range workloads {
		kinds[w.ServiceID] = w.Kind
	}

	if kinds["web"] != "Deployment" {
		t.Errorf("web kind = %q, want Deployment", kinds["web"])
	}
	if kinds["db"] != "StatefulSet" {
		t.Errorf("db kind = %q, want StatefulSet", kinds["db"])
	}
	if kinds["fluentd"] != "DaemonSet" {
		t.Errorf("fluentd kind = %q, want DaemonSet", kinds["fluentd"])
	}
	if _, found := kinds["kube-proxy"]; found {
		t.Errorf("system workload kube-proxy should not appear in report")
	}
}

// -----------------------------------------------------------------------------
// isSystemWorkload
// -----------------------------------------------------------------------------

func TestIsSystemWorkload(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"no relevant labels", map[string]string{"app": "web"}, false},
		{"k8s-app set", map[string]string{"k8s-app": "kube-proxy"}, true},
		{"controller-manager component", map[string]string{"app.kubernetes.io/component": "controller-manager"}, true},
		{"other component", map[string]string{"app.kubernetes.io/component": "scheduler"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSystemWorkload(tc.labels); got != tc.want {
				t.Errorf("isSystemWorkload(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// primaryImage
// -----------------------------------------------------------------------------

func TestPrimaryImage_EmptyContainers(t *testing.T) {
	if got := primaryImage(nil); got != "" {
		t.Errorf("primaryImage(nil) = %q, want empty", got)
	}
	if got := primaryImage([]corev1.Container{}); got != "" {
		t.Errorf("primaryImage([]) = %q, want empty", got)
	}
}

func TestPrimaryImage_FirstContainer(t *testing.T) {
	containers := []corev1.Container{
		{Image: "main-image:v1"},
		{Image: "sidecar:v2"},
	}
	if got := primaryImage(containers); got != "main-image:v1" {
		t.Errorf("primaryImage = %q, want main-image:v1", got)
	}
}

// -----------------------------------------------------------------------------
// fallbackName / sourceString
// -----------------------------------------------------------------------------

func TestFallbackName_PreferredUsedWhenSet(t *testing.T) {
	if got := fallbackName("preferred", "fallback"); got != "preferred" {
		t.Errorf("fallbackName = %q, want preferred", got)
	}
}

func TestFallbackName_FallbackUsedWhenPreferredEmpty(t *testing.T) {
	if got := fallbackName("", "fallback"); got != "fallback" {
		t.Errorf("fallbackName = %q, want fallback", got)
	}
}

func TestSourceString_EmptyDefaultsToWorkloadName(t *testing.T) {
	if got := sourceString(""); got != "workload_name" {
		t.Errorf("sourceString(\"\") = %q, want workload_name", got)
	}
}

// -----------------------------------------------------------------------------
// deref
// -----------------------------------------------------------------------------

func TestDeref_Nil(t *testing.T) {
	if got := deref(nil); got != 0 {
		t.Errorf("deref(nil) = %d, want 0", got)
	}
}

func TestDeref_NonNil(t *testing.T) {
	n := int32(5)
	if got := deref(&n); got != 5 {
		t.Errorf("deref(&5) = %d, want 5", got)
	}
}
