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

// Package informers wires the operator's multi-resource watch set into the
// controller-runtime shared cache.
package informers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientcache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
)

// WatchSet is the list of every resource kind the operator watches.
// WatchSet is intentionally exported so that RBAC generation and test doubles
// can enumerate it.
//
// The operator is read-only on every entry: informers use list+watch only.
var WatchSet = []client.Object{
	&corev1.Pod{},
	&corev1.Event{},
	&eventsv1.Event{},
	&corev1.Node{},
	&corev1.Service{},
	&corev1.Namespace{},
	&appsv1.Deployment{},
	&appsv1.StatefulSet{},
	&appsv1.DaemonSet{},
	&appsv1.ReplicaSet{},
	&autoscalingv2.HorizontalPodAutoscaler{},
	&batchv1.Job{},
	&batchv1.CronJob{},
	&networkingv1.Ingress{},
}

// Handler receives notifications about every tracked K8s object.
// Implementations are called from informer goroutines and MUST be safe for
// concurrent invocation.
type Handler interface {
	OnAdd(ctx context.Context, obj client.Object)
	OnUpdate(ctx context.Context, oldObj, newObj client.Object)
	OnDelete(ctx context.Context, obj client.Object)
}

// Manager wires WatchSet informers into a controller-runtime manager and
// forwards events to a Handler.
//
// Manager is a controller-runtime manager.Runnable; register it with
// ctrl.Manager.Add so it starts after the shared cache is ready.
type Manager struct {
	mgr     ctrl.Manager
	handler Handler
	log     logr.Logger
}

// Compile-time assertion that *Manager satisfies the controller-runtime
// Runnable + LeaderElectionRunnable contract.
var _ interface {
	Start(context.Context) error
	NeedLeaderElection() bool
} = (*Manager)(nil)

// NewManager constructs a Manager. The handler must be non-nil.
func NewManager(mgr ctrl.Manager, handler Handler, log logr.Logger) *Manager {
	if handler == nil {
		panic("informers.NewManager: handler must not be nil")
	}
	return &Manager{mgr: mgr, handler: handler, log: log}
}

// NeedLeaderElection ensures the informer Manager runs on the elected leader
// only. Event processing must be singleton to avoid duplicate uploads.
func (m *Manager) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable. It registers cache event handlers for
// every entry in WatchSet and blocks until ctx is done.
//
// Start is called by the controller-runtime manager after the shared cache
// has been started, which guarantees that cache.GetInformer will not block
// forever waiting for cache startup.
func (m *Manager) Start(ctx context.Context) error {
	if m.mgr == nil {
		return fmt.Errorf("informers.Manager: controller-runtime manager is nil")
	}
	cache := m.mgr.GetCache()
	if cache == nil {
		return fmt.Errorf("informers.Manager: controller-runtime cache is nil")
	}

	for _, obj := range WatchSet {
		informer, err := cache.GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("get informer for %T: %w", obj, err)
		}

		// Capture obj type for logging; the handler receives the concrete
		// runtime type from the informer.
		handler := m.handler
		if _, err := informer.AddEventHandler(clientcache.ResourceEventHandlerFuncs{
			AddFunc: func(raw any) {
				co, ok := raw.(client.Object)
				if !ok {
					return
				}
				handler.OnAdd(ctx, co)
			},
			UpdateFunc: func(oldRaw, newRaw any) {
				oldObj, oldOK := oldRaw.(client.Object)
				newObj, newOK := newRaw.(client.Object)
				if !oldOK || !newOK {
					return
				}
				handler.OnUpdate(ctx, oldObj, newObj)
			},
			DeleteFunc: func(raw any) {
				// When a final state is unknown, client-go wraps the object
				// in a DeletedFinalStateUnknown sentinel; unwrap it first.
				if tombstone, ok := raw.(clientcache.DeletedFinalStateUnknown); ok {
					raw = tombstone.Obj
				}
				co, ok := raw.(client.Object)
				if !ok {
					return
				}
				handler.OnDelete(ctx, co)
			},
		}); err != nil {
			return fmt.Errorf("add event handler for %T: %w", obj, err)
		}
	}

	m.log.Info("informers registered", "count", len(WatchSet))
	<-ctx.Done()
	return nil
}

// AddToScheme registers every WatchSet object type's GroupVersion with the
// provided scheme. Call this from cmd/main.go before creating the manager.
//
// Most of these types are already registered by clientgoscheme.AddToScheme,
// but registering them again is idempotent and documents intent.
func AddToScheme(s *runtime.Scheme) error {
	adders := []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		autoscalingv2.AddToScheme,
		batchv1.AddToScheme,
		networkingv1.AddToScheme,
		eventsv1.AddToScheme,
	}
	for _, add := range adders {
		if err := add(s); err != nil {
			return err
		}
	}
	return nil
}
