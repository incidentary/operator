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

package mapper

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/incidentary/operator/internal/wireformat"
)

// Deployment condition reasons used by the heuristic detection helpers.
const (
	reasonNewReplicaSetCreated     = "NewReplicaSetCreated"
	reasonNewReplicaSetAvailable   = "NewReplicaSetAvailable"
	reasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
)

// ----------------------------------------------------------------------------
// Deployment domain mappers — DEPLOY_* kinds.
//
// Unlike K8S_DEPLOY_ROLLOUT (infrastructure.go) which tracks the mechanical
// rollout progression of the Deployment controller, the DEPLOY_* kinds here
// are domain-level deployment lifecycle events: started, succeeded, failed,
// rolled back, cancelled. They model the user-observable "deploy" rather
// than the K8s controller's internal state.
//
// Kubernetes does not expose a single clean "deploy started" or "deploy
// cancelled" signal, so several of these mappings are heuristic:
//   - DEPLOY_STARTED: new revision annotation AND Progressing with
//                     reason=NewReplicaSetCreated.
//   - DEPLOY_SUCCEEDED: Available=True AND UpdatedReplicas==*Spec.Replicas
//                       AND ReadyReplicas==*Spec.Replicas AND Progressing
//                       transitioned to reason=NewReplicaSetAvailable.
//   - DEPLOY_FAILED: Progressing=False AND reason=ProgressDeadlineExceeded.
//   - DEPLOY_ROLLED_BACK: revision annotation decreased OR change-cause
//                         annotation contains "rollback".
//   - DEPLOY_CANCELLED: spec.replicas set to 0 DURING an active rollout, or
//                       Deployment deleted while Progressing=True.
// ----------------------------------------------------------------------------

// FromDeploymentChange emits zero or more DEPLOY_* events based on
// transitions in a Deployment's conditions and annotations. Returns nil when
// nothing changed since `old`.
func (m *Mapper) FromDeploymentChange(ctx context.Context, old, n *appsv1.Deployment) ([]wireformat.Event, error) {
	if n == nil {
		return nil, nil
	}

	serviceID, err := m.resolveWorkloadServiceID(ctx, "Deployment", n.Namespace, n.Name)
	if err != nil {
		return nil, fmt.Errorf("mapper.FromDeploymentChange: %w", err)
	}

	newRevision := n.Annotations["deployment.kubernetes.io/revision"]
	newChangeCause := n.Annotations["kubernetes.io/change-cause"]
	var oldRevision, oldChangeCause string
	if old != nil {
		oldRevision = old.Annotations["deployment.kubernetes.io/revision"]
		oldChangeCause = old.Annotations["kubernetes.io/change-cause"]
	}

	// DEPLOY_ROLLED_BACK has the highest priority: if the revision decreased
	// or the change-cause marks a rollback, emit that single event instead
	// of any other DEPLOY_* transition.
	if isRollback(oldRevision, newRevision, oldChangeCause, newChangeCause) {
		return []wireformat.Event{m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeployRolledBack,
			"RolledBack",
			deploymentOccurredAt(n),
			map[string]string{
				"k8s.rollout.revision":      newRevision,
				"k8s.rollout.prev_revision": oldRevision,
				"k8s.rollout.change_cause":  newChangeCause,
			},
		)}, nil
	}

	var out []wireformat.Event

	if isDeployStarted(old, n, oldRevision, newRevision) {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeployStarted,
			"Started",
			deploymentOccurredAt(n),
			map[string]string{
				"k8s.rollout.revision": newRevision,
			},
		))
	}

	if isDeploySucceeded(old, n) {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeploySucceeded,
			"Succeeded",
			deploymentOccurredAt(n),
			map[string]string{
				"k8s.rollout.revision": newRevision,
			},
		))
	}

	if isDeployFailed(old, n) {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeployFailed,
			reasonProgressDeadlineExceeded,
			deploymentOccurredAt(n),
			map[string]string{
				"k8s.rollout.revision": newRevision,
			},
		))
	}

	if isDeployCancelled(old, n) {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeployCancelled,
			"ScaleToZero",
			deploymentOccurredAt(n),
			map[string]string{
				"k8s.rollout.revision": newRevision,
			},
		))
	}

	return out, nil
}

// FromDeploymentDelete emits DEPLOY_CANCELLED when a Deployment is deleted
// while a rollout was in progress. Returns (zero, false, nil) otherwise.
func (m *Mapper) FromDeploymentDelete(ctx context.Context, d *appsv1.Deployment) (wireformat.Event, bool, error) {
	if d == nil {
		return wireformat.Event{}, false, nil
	}
	progressing := findDeploymentCondition(d.Status.Conditions, appsv1.DeploymentProgressing)
	if progressing == nil || progressing.Status != corev1.ConditionTrue {
		return wireformat.Event{}, false, nil
	}

	serviceID, err := m.resolveWorkloadServiceID(ctx, "Deployment", d.Namespace, d.Name)
	if err != nil {
		return wireformat.Event{}, false, fmt.Errorf("mapper.FromDeploymentDelete: %w", err)
	}

	ev := m.buildDeployEvent(
		d, serviceID,
		wireformat.KindDeployCancelled,
		"DeletedMidRollout",
		deploymentOccurredAt(d),
		map[string]string{
			"k8s.rollout.revision": d.Annotations["deployment.kubernetes.io/revision"],
		},
	)
	return ev, true, nil
}

// FromStatefulSetChange emits DEPLOY_* events for StatefulSet rollouts using
// UpdateRevision and CurrentRevision.
func (m *Mapper) FromStatefulSetChange(ctx context.Context, old, n *appsv1.StatefulSet) ([]wireformat.Event, error) {
	if n == nil {
		return nil, nil
	}

	serviceID, err := m.resolveWorkloadServiceID(ctx, "StatefulSet", n.Namespace, n.Name)
	if err != nil {
		return nil, fmt.Errorf("mapper.FromStatefulSetChange: %w", err)
	}

	newUpdate := n.Status.UpdateRevision
	newCurrent := n.Status.CurrentRevision
	var oldUpdate, oldCurrent string
	if old != nil {
		oldUpdate = old.Status.UpdateRevision
		oldCurrent = old.Status.CurrentRevision
	}

	var out []wireformat.Event

	// DEPLOY_STARTED: update revision just diverged from current revision.
	if newUpdate != "" && newUpdate != newCurrent {
		var started bool
		switch {
		case old == nil:
			started = true
		case oldUpdate == oldCurrent:
			started = true
		case oldUpdate != newUpdate:
			started = true
		}
		if started {
			out = append(out, m.buildDeployEvent(
				n, serviceID,
				wireformat.KindDeployStarted,
				"Started",
				wireformat.TimeToUnixNano(n.CreationTimestamp.Time),
				map[string]string{
					"k8s.rollout.revision": newUpdate,
					"k8s.rollout.current":  newCurrent,
					"k8s.resource.kind":    "StatefulSet",
				},
			))
		}
	}

	// DEPLOY_SUCCEEDED: update revision converged to current revision after
	// previously diverging, AND all replicas are ready.
	if old != nil && oldUpdate != "" && oldUpdate != oldCurrent &&
		newUpdate == newCurrent && newUpdate != "" &&
		n.Status.ReadyReplicas == specReplicas(n.Spec.Replicas) {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeploySucceeded,
			"Succeeded",
			wireformat.TimeToUnixNano(n.CreationTimestamp.Time),
			map[string]string{
				"k8s.rollout.revision": newUpdate,
				"k8s.resource.kind":    "StatefulSet",
			},
		))
	}

	return out, nil
}

// FromDaemonSetChange emits DEPLOY_* events for DaemonSet rollouts using
// UpdatedNumberScheduled vs DesiredNumberScheduled.
func (m *Mapper) FromDaemonSetChange(ctx context.Context, old, n *appsv1.DaemonSet) ([]wireformat.Event, error) {
	if n == nil {
		return nil, nil
	}

	serviceID, err := m.resolveWorkloadServiceID(ctx, "DaemonSet", n.Namespace, n.Name)
	if err != nil {
		return nil, fmt.Errorf("mapper.FromDaemonSetChange: %w", err)
	}

	newGen := n.Status.ObservedGeneration
	newUpdated := n.Status.UpdatedNumberScheduled
	newDesired := n.Status.DesiredNumberScheduled
	var oldGen int64
	var oldUpdated, oldDesired int32
	if old != nil {
		oldGen = old.Status.ObservedGeneration
		oldUpdated = old.Status.UpdatedNumberScheduled
		oldDesired = old.Status.DesiredNumberScheduled
	}

	var out []wireformat.Event

	// DEPLOY_STARTED: observed generation incremented and not all pods
	// updated yet.
	if newGen > oldGen && newUpdated < newDesired {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeployStarted,
			"Started",
			wireformat.TimeToUnixNano(n.CreationTimestamp.Time),
			map[string]string{
				"k8s.rollout.generation": fmt.Sprintf("%d", newGen),
				"k8s.resource.kind":      "DaemonSet",
			},
		))
	}

	// DEPLOY_SUCCEEDED: updated == desired where previously updated < desired.
	if old != nil && oldUpdated < oldDesired &&
		newUpdated == newDesired && newDesired > 0 {
		out = append(out, m.buildDeployEvent(
			n, serviceID,
			wireformat.KindDeploySucceeded,
			"Succeeded",
			wireformat.TimeToUnixNano(n.CreationTimestamp.Time),
			map[string]string{
				"k8s.rollout.generation": fmt.Sprintf("%d", newGen),
				"k8s.resource.kind":      "DaemonSet",
			},
		))
	}

	return out, nil
}

// ----------------------------------------------------------------------------
// Heuristic detection helpers.
// ----------------------------------------------------------------------------

// isRollback returns true when the revision decreased or the change-cause
// annotation indicates a rollback.
func isRollback(oldRev, newRev, oldCC, newCC string) bool {
	if newRev != "" && oldRev != "" && revisionLess(newRev, oldRev) {
		return true
	}
	if newCC != "" && newCC != oldCC && strings.Contains(strings.ToLower(newCC), "rollback") {
		return true
	}
	return false
}

// revisionLess compares revision strings numerically, falling back to lexical
// compare when either is not integer-formatted.
func revisionLess(a, b string) bool {
	var ai, bi int
	_, errA := fmt.Sscanf(a, "%d", &ai)
	_, errB := fmt.Sscanf(b, "%d", &bi)
	if errA != nil || errB != nil {
		return a < b
	}
	return ai < bi
}

// isDeployStarted returns true when a new rollout just started.
func isDeployStarted(old, n *appsv1.Deployment, oldRev, newRev string) bool {
	if newRev == "" {
		return false
	}
	progressing := findDeploymentCondition(n.Status.Conditions, appsv1.DeploymentProgressing)
	if progressing == nil {
		return false
	}
	// On OnAdd (old == nil): only fire if the Progressing condition is True
	// with NewReplicaSetCreated — a freshly-created Deployment mid-rollout.
	if old == nil {
		return progressing.Status == corev1.ConditionTrue &&
			progressing.Reason == reasonNewReplicaSetCreated
	}
	// Strongest signal: revision bumped.
	if oldRev != newRev {
		return true
	}
	// Progressing became True with NewReplicaSetCreated in this transition.
	oldProgressing := findDeploymentCondition(old.Status.Conditions, appsv1.DeploymentProgressing)
	if oldProgressing == nil || oldProgressing.Reason != reasonNewReplicaSetCreated {
		return progressing.Status == corev1.ConditionTrue &&
			progressing.Reason == reasonNewReplicaSetCreated
	}
	return false
}

// isDeploySucceeded returns true when the rollout just completed successfully.
func isDeploySucceeded(old, n *appsv1.Deployment) bool {
	available := findDeploymentCondition(n.Status.Conditions, appsv1.DeploymentAvailable)
	progressing := findDeploymentCondition(n.Status.Conditions, appsv1.DeploymentProgressing)
	if available == nil || available.Status != corev1.ConditionTrue {
		return false
	}
	if progressing == nil || progressing.Reason != reasonNewReplicaSetAvailable {
		return false
	}
	want := specReplicas(n.Spec.Replicas)
	if n.Status.UpdatedReplicas != want || n.Status.ReadyReplicas != want {
		return false
	}
	// Only fire on transition: suppress baseline SUCCEEDED on OnAdd because
	// we cannot distinguish a fresh successful rollout from an already-
	// stable deployment that just appeared in the informer cache.
	if old == nil {
		return false
	}
	oldProgressing := findDeploymentCondition(old.Status.Conditions, appsv1.DeploymentProgressing)
	if oldProgressing != nil && oldProgressing.Reason == reasonNewReplicaSetAvailable {
		return false
	}
	return true
}

// isDeployFailed returns true when Progressing transitioned to False with
// reason ProgressDeadlineExceeded.
func isDeployFailed(old, n *appsv1.Deployment) bool {
	progressing := findDeploymentCondition(n.Status.Conditions, appsv1.DeploymentProgressing)
	if progressing == nil {
		return false
	}
	if progressing.Status != corev1.ConditionFalse || progressing.Reason != reasonProgressDeadlineExceeded {
		return false
	}
	if old == nil {
		return true
	}
	oldProgressing := findDeploymentCondition(old.Status.Conditions, appsv1.DeploymentProgressing)
	if oldProgressing != nil && oldProgressing.Reason == reasonProgressDeadlineExceeded {
		return false
	}
	return true
}

// isDeployCancelled returns true when the deployment was just scaled to zero
// mid-rollout.
func isDeployCancelled(old, n *appsv1.Deployment) bool {
	if old == nil {
		return false
	}
	oldReplicas := specReplicas(old.Spec.Replicas)
	newReplicas := specReplicas(n.Spec.Replicas)
	if oldReplicas == 0 || newReplicas != 0 {
		return false
	}
	progressing := findDeploymentCondition(old.Status.Conditions, appsv1.DeploymentProgressing)
	if progressing == nil || progressing.Status != corev1.ConditionTrue {
		return false
	}
	return true
}

// specReplicas unwraps a *int32 replica count, defaulting to 1 per K8s
// convention when nil.
func specReplicas(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// deploymentOccurredAt picks the latest LastTransitionTime across a
// Deployment's conditions, falling back to CreationTimestamp.
func deploymentOccurredAt(d *appsv1.Deployment) int64 {
	var latest int64
	for _, cond := range d.Status.Conditions {
		t := wireformat.TimeToUnixNano(cond.LastTransitionTime.Time)
		if t > latest {
			latest = t
		}
	}
	if latest == 0 {
		latest = wireformat.TimeToUnixNano(d.CreationTimestamp.Time)
	}
	return latest
}

// deployObject is the subset of client.Object methods buildDeployEvent reads.
// Accepting an interface keeps the helper workload-agnostic (works for
// Deployment, StatefulSet, and DaemonSet).
type deployObject interface {
	GetName() string
	GetNamespace() string
	GetAnnotations() map[string]string
	GetLabels() map[string]string
}

// buildDeployEvent assembles a DEPLOY_* wire format event with the standard
// attributes shared across all deployment-domain kinds.
func (m *Mapper) buildDeployEvent(
	obj deployObject,
	serviceID string,
	kind wireformat.Kind,
	reason string,
	occurredAt int64,
	extra map[string]string,
) wireformat.Event {
	labels := obj.GetLabels()
	annotations := obj.GetAnnotations()

	attrs := map[string]string{
		"k8s.namespace.name": obj.GetNamespace(),
		"k8s.workload.name":  obj.GetName(),
		"k8s.reason":         reason,
	}
	if env := labels["app.kubernetes.io/part-of"]; env != "" {
		attrs["deployment.environment"] = env
	}
	if env := labels["environment"]; env != "" {
		attrs["deployment.environment"] = env
	}
	if cc := annotations["kubernetes.io/change-cause"]; cc != "" {
		attrs["k8s.change_cause"] = truncate(cc, 512)
	}
	for k, v := range extra {
		if v != "" {
			attrs[k] = v
		}
	}
	return wireformat.Event{
		ID:         uuid.NewString(),
		Kind:       kind,
		Severity:   severityForKind(kind),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}
}

// resolveWorkloadServiceID resolves a workload's service_id via the identity
// resolver, falling back to the workload name when nothing could be resolved.
func (m *Mapper) resolveWorkloadServiceID(ctx context.Context, kind, namespace, name string) (string, error) {
	res, err := m.Resolver.ResolveRef(ctx, kind, namespace, name)
	if err != nil {
		return name, err
	}
	if res.ServiceID != "" {
		return res.ServiceID, nil
	}
	return name, nil
}
