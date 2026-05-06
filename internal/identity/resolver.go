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

// Package identity resolves Kubernetes workloads to Incidentary service IDs.
//
// The resolver walks owner references (Pod → ReplicaSet → Deployment, or
// Pod → StatefulSet, or Pod → DaemonSet) using a controller-runtime reader
// backed by the shared informer cache. For non-Pod objects the resolver
// returns the object itself when it is already a recognized workload type.
//
// The authoritative source for a workload's service_id is the annotation
// `incidentary.com/service-id`. When absent, the resolver falls back to the
// workload's metadata.name.
package identity

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServiceIDAnnotation is the per-workload annotation that overrides the
// default service_id (workload name). It is the authoritative source for
// service identity resolution.
const ServiceIDAnnotation = "incidentary.com/service-id"

// Source describes how a service_id was derived. Reported per workload in the
// topology report, not on individual v2 events.
type Source string

const (
	// SourceAnnotation means the service ID came from the ServiceIDAnnotation
	// annotation on the resolved workload.
	SourceAnnotation Source = "annotation"
	// SourceWorkloadName means the service ID is the workload's metadata.name
	// (no override annotation was set).
	SourceWorkloadName Source = "workload_name"
)

// Result holds the output of a Resolve call.
type Result struct {
	// ServiceID is the resolved Incidentary service identifier.
	ServiceID string
	// Source indicates how ServiceID was derived.
	Source Source
	// OwnerKind is the Kubernetes kind of the resolved owning workload
	// ("Deployment", "StatefulSet", "DaemonSet"), or "Pod" if the resolver
	// fell back to a naked Pod, or "ReplicaSet" if the resolver found an
	// orphaned ReplicaSet whose Deployment was deleted.
	OwnerKind string
	// OwnerName is the metadata.name of the resolved workload.
	OwnerName string
	// Namespace is the namespace of the resolved workload.
	Namespace string
}

// Empty reports whether r holds no resolved identity at all.
func (r Result) Empty() bool {
	return r.ServiceID == "" && r.OwnerKind == "" && r.OwnerName == ""
}

// Resolver maps K8s objects to Incidentary service identities.
//
// In production, Client is the controller-runtime manager's cached reader.
// In tests it can be any client.Reader, typically a fake client built with
// sigs.k8s.io/controller-runtime/pkg/client/fake.
type Resolver struct {
	Client client.Reader
}

// NewResolver constructs a Resolver backed by the provided reader. A nil
// client panics to surface wiring bugs at startup rather than at first event.
func NewResolver(c client.Reader) *Resolver {
	if c == nil {
		panic("identity.NewResolver: client must not be nil")
	}
	return &Resolver{Client: c}
}

// Resolve walks owner references from the given object up to the top-level
// workload (Deployment, StatefulSet, DaemonSet) and derives the service ID.
//
// Rules:
//  1. If the object is already a workload (Deployment / StatefulSet /
//     DaemonSet), use it directly.
//  2. If the object is a Pod, look up its owning ReplicaSet (for Deployments)
//     or use the Pod's direct owner for StatefulSet/DaemonSet.
//  3. Check the owning workload for the ServiceIDAnnotation. If present,
//     ServiceID = annotation value, Source = SourceAnnotation.
//  4. Otherwise ServiceID = workload.Name, Source = SourceWorkloadName.
//  5. If the object has no recognizable owner (a naked Pod), fall back to
//     ServiceID = pod.Name, Source = SourceWorkloadName, OwnerKind = "Pod".
//
// Errors wrap the underlying client.Reader error with enough context to
// debug, but missing intermediate resources (e.g., a ReplicaSet whose
// Deployment was deleted) are treated as a partial success — the resolver
// returns the highest-level workload it could reach instead of erroring.
func (r *Resolver) Resolve(ctx context.Context, obj client.Object) (Result, error) {
	if obj == nil {
		return Result{}, nil
	}

	switch o := obj.(type) {
	case *appsv1.Deployment:
		return r.resolveFromWorkload("Deployment", o.Name, o.Namespace, o.GetAnnotations()), nil
	case *appsv1.StatefulSet:
		return r.resolveFromWorkload("StatefulSet", o.Name, o.Namespace, o.GetAnnotations()), nil
	case *appsv1.DaemonSet:
		return r.resolveFromWorkload("DaemonSet", o.Name, o.Namespace, o.GetAnnotations()), nil
	case *appsv1.ReplicaSet:
		return r.resolveFromReplicaSet(ctx, o)
	case *corev1.Pod:
		return r.resolveFromPod(ctx, o)
	default:
		// Non-workload objects (Events, Nodes, HPAs, ...) do not carry
		// service identity themselves — the caller is expected to resolve
		// identity via the Regarding / involvedObject reference instead.
		return Result{}, nil
	}
}

// ResolveRef resolves a loose (kind, namespace, name) reference. This is the
// path the K8s Event mapper uses: the Event carries a corev1.ObjectReference
// pointing at the affected object.
func (r *Resolver) ResolveRef(ctx context.Context, kind, namespace, name string) (Result, error) {
	if name == "" {
		return Result{}, nil
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}

	switch kind {
	case "Deployment":
		var d appsv1.Deployment
		if err := r.Client.Get(ctx, key, &d); err != nil {
			return handleFetchError(err, kind, namespace, name)
		}
		return r.resolveFromWorkload("Deployment", d.Name, d.Namespace, d.GetAnnotations()), nil
	case "StatefulSet":
		var s appsv1.StatefulSet
		if err := r.Client.Get(ctx, key, &s); err != nil {
			return handleFetchError(err, kind, namespace, name)
		}
		return r.resolveFromWorkload("StatefulSet", s.Name, s.Namespace, s.GetAnnotations()), nil
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := r.Client.Get(ctx, key, &ds); err != nil {
			return handleFetchError(err, kind, namespace, name)
		}
		return r.resolveFromWorkload("DaemonSet", ds.Name, ds.Namespace, ds.GetAnnotations()), nil
	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := r.Client.Get(ctx, key, &rs); err != nil {
			return handleFetchError(err, kind, namespace, name)
		}
		return r.resolveFromReplicaSet(ctx, &rs)
	case "Pod":
		var p corev1.Pod
		if err := r.Client.Get(ctx, key, &p); err != nil {
			return handleFetchError(err, kind, namespace, name)
		}
		return r.resolveFromPod(ctx, &p)
	case "Node":
		// Nodes do not carry service identity; callers handle node events
		// with a synthetic service_id tied to the node name.
		return Result{
			ServiceID: name,
			Source:    SourceWorkloadName,
			OwnerKind: "Node",
			OwnerName: name,
			Namespace: "",
		}, nil
	default:
		return Result{}, nil
	}
}

// resolveFromWorkload derives ServiceID + Source from a known workload's
// name and annotations.
func (r *Resolver) resolveFromWorkload(kind, name, namespace string, annotations map[string]string) Result {
	if id, ok := annotations[ServiceIDAnnotation]; ok && id != "" {
		return Result{
			ServiceID: id,
			Source:    SourceAnnotation,
			OwnerKind: kind,
			OwnerName: name,
			Namespace: namespace,
		}
	}
	return Result{
		ServiceID: name,
		Source:    SourceWorkloadName,
		OwnerKind: kind,
		OwnerName: name,
		Namespace: namespace,
	}
}

// resolveFromPod walks Pod → RS → Deployment, or directly to StatefulSet /
// DaemonSet if those are the immediate owners.
func (r *Resolver) resolveFromPod(ctx context.Context, p *corev1.Pod) (Result, error) {
	owner := controllerOwner(p.GetOwnerReferences())
	if owner == nil {
		// Naked Pod — use its own name.
		return r.resolveFromWorkload("Pod", p.Name, p.Namespace, p.GetAnnotations()), nil
	}

	switch owner.Kind {
	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &rs); err != nil {
			if apierrors.IsNotFound(err) {
				// Orphaned Pod: the RS was deleted but the Pod is still
				// around. Use the pod annotations / name as best-effort.
				return r.resolveFromWorkload("Pod", p.Name, p.Namespace, p.GetAnnotations()), nil
			}
			return Result{}, fmt.Errorf("identity.Resolver: fetch ReplicaSet %s/%s: %w", p.Namespace, owner.Name, err)
		}
		return r.resolveFromReplicaSet(ctx, &rs)
	case "StatefulSet":
		var ss appsv1.StatefulSet
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &ss); err != nil {
			if apierrors.IsNotFound(err) {
				return r.resolveFromWorkload("StatefulSet", owner.Name, p.Namespace, nil), nil
			}
			return Result{}, fmt.Errorf("identity.Resolver: fetch StatefulSet %s/%s: %w", p.Namespace, owner.Name, err)
		}
		return r.resolveFromWorkload("StatefulSet", ss.Name, ss.Namespace, ss.GetAnnotations()), nil
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				return r.resolveFromWorkload("DaemonSet", owner.Name, p.Namespace, nil), nil
			}
			return Result{}, fmt.Errorf("identity.Resolver: fetch DaemonSet %s/%s: %w", p.Namespace, owner.Name, err)
		}
		return r.resolveFromWorkload("DaemonSet", ds.Name, ds.Namespace, ds.GetAnnotations()), nil
	default:
		// Unknown controller kind (e.g., a Job, CronJob, or a custom
		// controller). Fall back to the Pod itself so downstream mappers
		// still have something to correlate on.
		return r.resolveFromWorkload("Pod", p.Name, p.Namespace, p.GetAnnotations()), nil
	}
}

// resolveFromReplicaSet walks RS → Deployment. If the owning Deployment is
// missing, it returns the ReplicaSet itself as the resolved owner.
func (r *Resolver) resolveFromReplicaSet(ctx context.Context, rs *appsv1.ReplicaSet) (Result, error) {
	owner := controllerOwner(rs.GetOwnerReferences())
	if owner == nil || owner.Kind != "Deployment" {
		// Orphaned ReplicaSet (no Deployment parent) — use the RS annotations
		// and name.
		return r.resolveFromWorkload("ReplicaSet", rs.Name, rs.Namespace, rs.GetAnnotations()), nil
	}

	var d appsv1.Deployment
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: owner.Name}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			// The Deployment was deleted (rare but possible during rollbacks
			// or cleanup) — fall back to the ReplicaSet.
			return r.resolveFromWorkload("ReplicaSet", rs.Name, rs.Namespace, rs.GetAnnotations()), nil
		}
		return Result{}, fmt.Errorf("identity.Resolver: fetch Deployment %s/%s: %w", rs.Namespace, owner.Name, err)
	}
	return r.resolveFromWorkload("Deployment", d.Name, d.Namespace, d.GetAnnotations()), nil
}

// controllerOwner returns the single controller-owner reference from refs, or
// nil if no entry has Controller == true. Kubernetes guarantees at most one
// controller owner per object.
func controllerOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	// Fallback: if no ref is marked Controller, return the first entry (K8s
	// does not require Controller to be set in all legacy clients).
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

// handleFetchError translates NotFound into an empty Result and wraps other
// errors with context so tests and logs can attribute them.
func handleFetchError(err error, kind, namespace, name string) (Result, error) {
	if apierrors.IsNotFound(err) {
		return Result{}, nil
	}
	return Result{}, fmt.Errorf("identity.Resolver: fetch %s %s/%s: %w", kind, namespace, name, err)
}
