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
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/identity"
)

type fakeServices struct {
	entries []ingestclient.ServiceEntry
	err     error
}

func (f *fakeServices) List(context.Context) ([]ingestclient.ServiceEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

func mkDepWithAnnotation(name string, ageOffset time.Duration, annotation string) *appsv1.Deployment {
	anno := map[string]string{}
	if annotation != "" {
		anno[identity.ServiceIDAnnotation] = annotation
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "prod",
			Annotations:       anno,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-ageOffset)},
		},
	}
}

func TestReconcile_ClassifiesMatched(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	// Workload created long enough ago that grace period has passed.
	d := mkDepWithAnnotation("payment", time.Hour, "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	svc := &fakeServices{entries: []ingestclient.ServiceEntry{
		{ServiceID: "payment", Instrumented: true},
	}}

	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	matched, ghost, mismatched, newCount := r.Counts()
	if matched != 1 || ghost != 0 || mismatched != 0 || newCount != 0 {
		t.Errorf("counts = matched=%d ghost=%d mismatched=%d new=%d",
			matched, ghost, mismatched, newCount)
	}
}

func TestReconcile_ClassifiesGhost(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	d := mkDepWithAnnotation("analytics", time.Hour, "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	svc := &fakeServices{entries: []ingestclient.ServiceEntry{
		{ServiceID: "analytics", Instrumented: false},
	}}

	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	matched, ghost, _, _ := r.Counts()
	if matched != 0 || ghost != 1 {
		t.Errorf("counts: matched=%d ghost=%d", matched, ghost)
	}
}

func TestReconcile_ClassifiesMismatched(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	// Workload exists, age far exceeds grace period.
	d := mkDepWithAnnotation("payment-svc", time.Hour, "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	// Registered service is "payment-service" — Levenshtein distance 2 from
	// "payment-svc" (delete "-", delete "c", ... actually distance is 3).
	// Use "payment" instead (distance 4 — above threshold).
	svc := &fakeServices{entries: []ingestclient.ServiceEntry{
		{ServiceID: "other-thing", Instrumented: true},
	}}

	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	_, _, mismatched, _ := r.Counts()
	if mismatched != 1 {
		t.Errorf("mismatched = %d, want 1", mismatched)
	}
}

func TestReconcile_ClassifiesNewWorkloadInGracePeriod(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	// Workload just created — within grace period.
	d := mkDepWithAnnotation("brand-new", 10*time.Second, "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	svc := &fakeServices{entries: []ingestclient.ServiceEntry{
		{ServiceID: "other", Instrumented: true},
	}}

	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	_, _, mismatched, newCount := r.Counts()
	if newCount != 1 || mismatched != 0 {
		t.Errorf("new=%d mismatched=%d, want new=1 mismatched=0", newCount, mismatched)
	}
}

func TestReconcile_DormantOnEmptyServices(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	d := mkDepWithAnnotation("web", time.Hour, "")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	svc := &fakeServices{entries: nil}
	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	initialBackoff := r.currentBackoff
	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	// Backoff should have doubled after empty response.
	if r.currentBackoff <= initialBackoff {
		t.Errorf("currentBackoff not increased: %v", r.currentBackoff)
	}
	// No counts should be updated.
	matched, ghost, mismatched, newCount := r.Counts()
	if matched != 0 || ghost != 0 || mismatched != 0 || newCount != 0 {
		t.Errorf("counts should be zero on dormant cycle")
	}
}

func TestReconcile_AnnotationOverride(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	// Workload name is "payment-v2", but annotation overrides to "payment".
	d := mkDepWithAnnotation("payment-v2", time.Hour, "payment")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).Build()

	svc := &fakeServices{entries: []ingestclient.ServiceEntry{
		{ServiceID: "payment", Instrumented: true},
	}}

	loop := NewLoop(c, identity.NewResolver(c), &fakeTopology{}, testr.New(t), Options{})
	r := NewReconciler(c, loop, svc, testr.New(t), time.Minute)

	if err := r.runOnce(ctx); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	matched, _, _, _ := r.Counts()
	if matched != 1 {
		t.Errorf("matched = %d, want 1 (annotation override)", matched)
	}
}

func TestFuzzyMatch(t *testing.T) {
	candidates := []string{"payment-service", "order-api", "inventory-service"}
	cases := []struct {
		in      string
		want    string
		matched bool
	}{
		{"payment-svc", "payment-service", true}, // distance 4 > threshold 2 — should NOT match
		{"payment-servic", "payment-service", true},
		{"order-ap", "order-api", true},
		{"completely-different", "", false},
	}
	for _, tc := range cases {
		got, ok := fuzzyMatch(tc.in, candidates, 2)
		if tc.in == "payment-svc" {
			// "payment-svc" vs "payment-service": p,a,y,m,e,n,t,-,s,v,c vs p,a,y,m,e,n,t,-,s,e,r,v,i,c,e
			// distance is 4 (insert e,r,i, replace v with v... actually the distance is higher)
			// fuzzyMatch should NOT find a match at threshold 2
			if ok {
				t.Errorf("payment-svc should not match at threshold 2, got %q", got)
			}
			continue
		}
		if ok != tc.matched {
			t.Errorf("fuzzyMatch(%q): matched=%v, want %v", tc.in, ok, tc.matched)
		}
		if ok && got != tc.want {
			t.Errorf("fuzzyMatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"same", "same", 0},
		{"payment-service", "payment-servic", 1},
	}
	for _, tc := range cases {
		got := levenshtein(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
