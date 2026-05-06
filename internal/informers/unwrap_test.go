/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package informers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcache "k8s.io/client-go/tools/cache"
)

func TestUnwrapTombstone_RealObjectPassesThrough(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
	got, ok := unwrapTombstone(pod)
	if !ok {
		t.Fatal("expected ok=true for real client.Object")
	}
	if got.GetName() != "real" {
		t.Errorf("name = %q, want %q", got.GetName(), "real")
	}
}

func TestUnwrapTombstone_TombstoneWrappedObjectReturnsInner(t *testing.T) {
	// client-go wraps deleted-while-disconnected objects in
	// DeletedFinalStateUnknown. The inner Obj is the real object the
	// informer last saw; unwrapTombstone must return it transparently.
	inner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "kube-system"}}
	tombstone := clientcache.DeletedFinalStateUnknown{Key: "kube-system/ghost", Obj: inner}

	got, ok := unwrapTombstone(tombstone)
	if !ok {
		t.Fatal("expected ok=true after unwrapping tombstone with valid inner")
	}
	if got.GetName() != "ghost" {
		t.Errorf("name = %q, want %q (inner object should be exposed)", got.GetName(), "ghost")
	}
}

func TestUnwrapTombstone_GarbageReturnsNotOK(t *testing.T) {
	// Anything that's neither a client.Object nor a DeletedFinalStateUnknown
	// containing one must yield ok=false so the caller drops the event
	// instead of nil-derefing inside the handler.
	got, ok := unwrapTombstone("not an object")
	if ok {
		t.Errorf("expected ok=false for non-object input, got=%v", got)
	}
}

func TestUnwrapTombstone_TombstoneWithGarbageInnerReturnsNotOK(t *testing.T) {
	// A DeletedFinalStateUnknown whose Obj is not a client.Object (shouldn't
	// happen in practice, but must not crash the handler).
	tombstone := clientcache.DeletedFinalStateUnknown{Key: "k", Obj: "not an object"}
	got, ok := unwrapTombstone(tombstone)
	if ok {
		t.Errorf("expected ok=false for garbage-wrapped tombstone, got=%v", got)
	}
}

func TestUnwrapTombstone_NilReturnsNotOK(t *testing.T) {
	got, ok := unwrapTombstone(nil)
	if ok {
		t.Errorf("expected ok=false for nil input, got=%v", got)
	}
}
