/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package informers_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	"github.com/incidentary/operator/internal/informers"
)

// TestWatchSet verifies WatchSet contains all 14 expected resource kinds.
func TestWatchSet(t *testing.T) {
	if len(informers.WatchSet) != 14 {
		t.Fatalf("WatchSet length = %d, want 14", len(informers.WatchSet))
	}

	expectedGVKs := map[string]bool{
		"/v1, Kind=Pod":                                false,
		"/v1, Kind=Event":                              false,
		"events.k8s.io/v1, Kind=Event":                 false,
		"/v1, Kind=Node":                               false,
		"/v1, Kind=Service":                            false,
		"/v1, Kind=Namespace":                          false,
		"apps/v1, Kind=Deployment":                     false,
		"apps/v1, Kind=StatefulSet":                    false,
		"apps/v1, Kind=DaemonSet":                      false,
		"apps/v1, Kind=ReplicaSet":                     false,
		"autoscaling/v2, Kind=HorizontalPodAutoscaler": false,
		"batch/v1, Kind=Job":                           false,
		"batch/v1, Kind=CronJob":                       false,
		"networking.k8s.io/v1, Kind=Ingress":           false,
	}

	// Build a scheme with all types registered so we can resolve GVKs.
	scheme := runtime.NewScheme()
	if err := informers.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	for i, obj := range informers.WatchSet {
		if obj == nil {
			t.Fatalf("WatchSet[%d] is nil", i)
		}
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("WatchSet[%d] (%T): ObjectKinds error: %v", i, obj, err)
		}
		if len(gvks) == 0 {
			t.Fatalf("WatchSet[%d] (%T): no GVKs registered", i, obj)
		}
		key := gvks[0].String()
		if _, ok := expectedGVKs[key]; !ok {
			t.Errorf("WatchSet[%d] (%T) has unexpected GVK %s", i, obj, key)
			continue
		}
		expectedGVKs[key] = true
	}

	for gvk, found := range expectedGVKs {
		if !found {
			t.Errorf("expected GVK %q not found in WatchSet", gvk)
		}
	}
}

// TestAddToScheme verifies that every WatchSet type's GroupVersion is
// registered in the scheme.
func TestAddToScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := informers.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	wantGroups := []schema.GroupVersion{
		corev1.SchemeGroupVersion,
		appsv1.SchemeGroupVersion,
		autoscalingv2.SchemeGroupVersion,
		batchv1.SchemeGroupVersion,
		networkingv1.SchemeGroupVersion,
		eventsv1.SchemeGroupVersion,
	}

	for _, gv := range wantGroups {
		if !scheme.IsVersionRegistered(gv) {
			t.Errorf("group-version %s not registered in scheme", gv)
		}
	}

	// Every WatchSet entry must resolve to at least one GVK in the scheme.
	for i, obj := range informers.WatchSet {
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Errorf("WatchSet[%d] (%T) not resolvable after AddToScheme: %v", i, obj, err)
			continue
		}
		if len(gvks) == 0 {
			t.Errorf("WatchSet[%d] (%T) resolves to zero GVKs", i, obj)
		}
	}
}

// TestAddToSchemeIdempotent verifies that AddToScheme can be called twice
// without error.
func TestAddToSchemeIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := informers.AddToScheme(scheme); err != nil {
		t.Fatalf("first AddToScheme failed: %v", err)
	}
	if err := informers.AddToScheme(scheme); err != nil {
		t.Fatalf("second AddToScheme failed: %v", err)
	}
}

// TestNewManagerRejectsNilHandler ensures the constructor does not accept
// nil handlers, which would cause nil-dereferences inside the informer
// callbacks.
func TestNewManagerRejectsNilHandler(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewManager(nil handler) should panic to fail fast")
		}
	}()
	// We pass nil manager + nil handler; only the handler nil-check should fire.
	_ = informers.NewManager(nil, nil, logr.Discard())
}

// TestManagerNeedLeaderElection pins the Manager as a leader-elected
// Runnable so that event processing is singleton. Phase 3 depends on this
// contract to guarantee at-most-once delivery to Incidentary.
func TestManagerNeedLeaderElection(t *testing.T) {
	m := informers.NewManager(nil, stubHandler{}, logr.Discard())
	if !m.NeedLeaderElection() {
		t.Error("informers.Manager.NeedLeaderElection() = false, want true")
	}
}

// stubHandler is a zero-work implementation of informers.Handler used by the
// unit tests. It exists only to satisfy the non-nil-handler precondition.
type stubHandler struct{}

func (stubHandler) OnAdd(_ context.Context, _ client.Object)                     {}
func (stubHandler) OnUpdate(_ context.Context, _ client.Object, _ client.Object) {}
func (stubHandler) OnDelete(_ context.Context, _ client.Object)                  {}
