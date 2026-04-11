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

// Package mapper translates Kubernetes objects into Incidentary wire format
// v2 events.
//
// The mapper is a pure function: given a K8s object (and optionally its
// previous state), it returns zero or more wireformat.Event values. It does
// not interact with the Kubernetes API directly — all API lookups happen
// through the identity.Resolver, which is backed by the shared informer
// cache. The mapper is safe to call concurrently from informer goroutines
// provided each caller has its own context.
package mapper

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/wireformat"
)

// Mapper translates K8s objects into wire format v2 events.
type Mapper struct {
	Resolver *identity.Resolver
}

// NewMapper constructs a Mapper backed by the provided identity resolver.
func NewMapper(resolver *identity.Resolver) *Mapper {
	if resolver == nil {
		panic("mapper.NewMapper: resolver must not be nil")
	}
	return &Mapper{Resolver: resolver}
}

// Dispatch routes an informer add/update/delete event to the appropriate
// mapper function based on the runtime type of the object. Unknown types
// return (nil, nil) — it is explicitly not an error to receive objects the
// mapper does not know how to translate.
//
// Semantics:
//   - OnAdd events: oldObj is nil, newObj is the added object.
//   - OnUpdate events: both are non-nil.
//   - OnDelete events: newObj is nil, oldObj is the deleted object.
func (m *Mapper) Dispatch(ctx context.Context, oldObj, newObj client.Object) ([]wireformat.Event, error) {
	// A delete event: we only care about deployment rollouts being cancelled
	// mid-flight. Everything else is handled on create/update.
	if newObj == nil && oldObj != nil {
		return m.dispatchDelete(ctx, oldObj)
	}
	if newObj == nil {
		return nil, nil
	}

	switch n := newObj.(type) {
	case *eventsv1.Event:
		return m.dispatchK8sEvent(ctx, n)
	case *corev1.Event:
		return m.dispatchCoreEvent(ctx, n)
	case *corev1.Node:
		// Node events require a previous state to detect condition transitions.
		old, _ := oldObj.(*corev1.Node)
		return m.FromNodeConditionChange(ctx, old, n)
	case *corev1.Pod:
		old, _ := oldObj.(*corev1.Pod)
		return m.FromPodStatusChange(ctx, old, n)
	case *autoscalingv2.HorizontalPodAutoscaler:
		old, _ := oldObj.(*autoscalingv2.HorizontalPodAutoscaler)
		ev, ok, err := m.FromHPAScale(ctx, old, n)
		if err != nil || !ok {
			return nil, err
		}
		return []wireformat.Event{ev}, nil
	case *appsv1.Deployment:
		old, _ := oldObj.(*appsv1.Deployment)
		infra, err := m.FromDeploymentRollout(ctx, old, n)
		if err != nil {
			return nil, err
		}
		deploy, err := m.FromDeploymentChange(ctx, old, n)
		if err != nil {
			return nil, err
		}
		return append(infra, deploy...), nil
	case *appsv1.StatefulSet:
		old, _ := oldObj.(*appsv1.StatefulSet)
		return m.FromStatefulSetChange(ctx, old, n)
	case *appsv1.DaemonSet:
		old, _ := oldObj.(*appsv1.DaemonSet)
		return m.FromDaemonSetChange(ctx, old, n)
	default:
		return nil, nil
	}
}

// dispatchDelete handles OnDelete notifications. Only Deployment deletions
// produce events in phase 3a (possible DEPLOY_CANCELLED if a rollout was in
// progress).
func (m *Mapper) dispatchDelete(ctx context.Context, obj client.Object) ([]wireformat.Event, error) {
	if d, ok := obj.(*appsv1.Deployment); ok {
		ev, ok, err := m.FromDeploymentDelete(ctx, d)
		if err != nil || !ok {
			return nil, err
		}
		return []wireformat.Event{ev}, nil
	}
	return nil, nil
}

// dispatchK8sEvent delegates to FromK8sEvent and adapts the (event, ok, err)
// signature to the ([]event, err) return shape expected by Dispatch.
func (m *Mapper) dispatchK8sEvent(ctx context.Context, ev *eventsv1.Event) ([]wireformat.Event, error) {
	out, ok, err := m.FromK8sEvent(ctx, ev)
	if err != nil || !ok {
		return nil, err
	}
	return []wireformat.Event{out}, nil
}

// dispatchCoreEvent delegates to FromCoreEvent.
func (m *Mapper) dispatchCoreEvent(ctx context.Context, ev *corev1.Event) ([]wireformat.Event, error) {
	out, ok, err := m.FromCoreEvent(ctx, ev)
	if err != nil || !ok {
		return nil, err
	}
	return []wireformat.Event{out}, nil
}
