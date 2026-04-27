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

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"

	"github.com/incidentary/operator/internal/ids"
	"github.com/incidentary/operator/internal/wireformat"
)

// ----------------------------------------------------------------------------
// K8s Event → v2 kind mapping table.
//
// Non-negotiable: every mapping is qualified by BOTH reason AND the kind of
// the regarding object. The same reason on a different resource kind has
// different meaning.
//   "BackOff" on Pod          → K8S_POD_CRASH
//   "BackOff" on Deployment   → NOT a crash, ignored.
//   "Failed" on ReplicaSet    → not mapped; RS-scoped failures surface via Pod events.
//
// Event reason values are best-effort machine-readable strings documented in
// kubelet and controller source; see
// https://github.com/kubernetes/kubernetes/tree/master/pkg/kubelet/events.
// ----------------------------------------------------------------------------

// reasonMapping describes a single (reason, regarding kind) → v2 kind rule.
type reasonMapping struct {
	regardingKinds []string
	kind           wireformat.Kind
}

// containerReasonOOMKilled is the termination reason set by the kubelet when
// the Linux OOM killer terminates a container.
const containerReasonOOMKilled = "OOMKilled"

// k8sEventReasonMappings is the closed set of K8s Event reasons the operator
// translates into v2 events. Reasons not in this table are ignored.
var k8sEventReasonMappings = map[string]reasonMapping{
	// Pod crash / backoff. `BackOff` applies to both kubelet pod-level
	// restart backoffs and image pull backoffs — image pull is handled
	// separately by ImagePullBackOff / ErrImagePull below.
	"BackOff":    {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},
	"CrashLoop":  {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},
	"Crashed":    {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},
	"Unhealthy":  {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},
	"Failed":     {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},
	"FailedSync": {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sPodCrash},

	// OOM kill.
	"OOMKilling":             {regardingKinds: []string{"Pod", "Node"}, kind: wireformat.KindK8sOOMKill},
	containerReasonOOMKilled: {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sOOMKill},

	// Eviction (pods kicked off a node due to resource pressure).
	"Evicted": {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sEviction},

	// Schedule failure.
	"FailedScheduling": {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sScheduleFail},

	// Image pull failures.
	"ErrImagePull":     {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sImagePullFail},
	"ImagePullBackOff": {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sImagePullFail},
	"InvalidImageName": {regardingKinds: []string{"Pod"}, kind: wireformat.KindK8sImagePullFail},
	"ErrImageNeverPull": {
		regardingKinds: []string{"Pod"},
		kind:           wireformat.KindK8sImagePullFail,
	},
}

// severityForKind returns the default severity for a kind produced by the
// operator. These are deterministic — each kind has exactly one severity.
func severityForKind(k wireformat.Kind) wireformat.Severity {
	switch k {
	case wireformat.KindK8sPodCrash,
		wireformat.KindK8sOOMKill,
		wireformat.KindK8sEviction,
		wireformat.KindK8sScheduleFail,
		wireformat.KindK8sImagePullFail:
		return wireformat.SeverityError
	case wireformat.KindK8sNodePressure:
		return wireformat.SeverityWarning
	case wireformat.KindK8sHPAScale,
		wireformat.KindK8sPodStarted,
		wireformat.KindK8sPodTerminated,
		wireformat.KindK8sDeployRollout:
		return wireformat.SeverityInfo
	case wireformat.KindDeployStarted,
		wireformat.KindDeploySucceeded:
		return wireformat.SeverityInfo
	case wireformat.KindDeployFailed:
		return wireformat.SeverityError
	case wireformat.KindDeployCancelled,
		wireformat.KindDeployRolledBack:
		return wireformat.SeverityWarning
	default:
		return wireformat.SeverityWarning
	}
}

// ----------------------------------------------------------------------------
// K8s Event mappers.
// ----------------------------------------------------------------------------

// FromK8sEvent translates a k8s.io/api/events/v1.Event into zero or one wire
// format events.
//
// Returns (zero-value Event, false, nil) when the event does not match any
// known mapping — not all K8s events are forwarded to Incidentary.
func (m *Mapper) FromK8sEvent(ctx context.Context, ev *eventsv1.Event) (wireformat.Event, bool, error) {
	if ev == nil || ev.Reason == "" {
		return wireformat.Event{}, false, nil
	}

	kind, ok := matchReason(ev.Reason, ev.Regarding.Kind)
	if !ok {
		return wireformat.Event{}, false, nil
	}

	// Resolve the owning service via the Regarding reference.
	res, err := m.Resolver.ResolveRef(ctx, ev.Regarding.Kind, ev.Regarding.Namespace, ev.Regarding.Name)
	if err != nil {
		return wireformat.Event{}, false, fmt.Errorf("mapper.FromK8sEvent: resolve %s/%s: %w", ev.Regarding.Namespace, ev.Regarding.Name, err)
	}
	// If the resolver could not find anything, fall back to the Regarding
	// reference's name so the event still carries *some* service identity.
	serviceID := res.ServiceID
	if serviceID == "" {
		serviceID = ev.Regarding.Name
	}

	attrs := map[string]any{
		"k8s.reason":               ev.Reason,
		"k8s.resource.kind":        ev.Regarding.Kind,
		"k8s.reporting_controller": ev.ReportingController,
	}
	if ev.Regarding.Namespace != "" {
		attrs["k8s.namespace.name"] = ev.Regarding.Namespace
	}
	setPodAttrs(attrs, ev.Regarding)
	if ev.Note != "" {
		attrs["k8s.message"] = truncate(ev.Note, 1024)
	}
	if ev.Action != "" {
		attrs["k8s.action"] = ev.Action
	}

	// Prefer EventTime (modern) over DeprecatedFirstTimestamp (legacy).
	occurredAt := ev.EventTime.UnixNano()
	if ev.EventTime.IsZero() {
		occurredAt = wireformat.TimeToUnixNano(ev.DeprecatedFirstTimestamp.Time)
	}

	out := wireformat.Event{
		ID:         ids.NewID(),
		Kind:       kind,
		Severity:   severityForKind(kind),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}

	// Populate series deduplication metadata for repeated events.
	if ev.Series != nil && ev.Series.Count >= 2 {
		firstAt := occurredAt
		lastAt := max(ev.Series.LastObservedTime.UnixNano(), firstAt)
		out.Series = &wireformat.Series{
			Count:   int64(ev.Series.Count),
			FirstAt: firstAt,
			LastAt:  lastAt,
		}
		// Per spec blunder 3 fix: OccurredAt == Series.FirstAt. Already the
		// case here because we initialised firstAt from occurredAt.
	}

	return out, true, nil
}

// FromCoreEvent handles the legacy core v1.Event type. The mapping table is
// the same as FromK8sEvent; only the field lookups differ.
func (m *Mapper) FromCoreEvent(ctx context.Context, ev *corev1.Event) (wireformat.Event, bool, error) {
	if ev == nil || ev.Reason == "" {
		return wireformat.Event{}, false, nil
	}

	kind, ok := matchReason(ev.Reason, ev.InvolvedObject.Kind)
	if !ok {
		return wireformat.Event{}, false, nil
	}

	res, err := m.Resolver.ResolveRef(ctx, ev.InvolvedObject.Kind, ev.InvolvedObject.Namespace, ev.InvolvedObject.Name)
	if err != nil {
		return wireformat.Event{}, false, fmt.Errorf("mapper.FromCoreEvent: resolve %s/%s: %w", ev.InvolvedObject.Namespace, ev.InvolvedObject.Name, err)
	}
	serviceID := res.ServiceID
	if serviceID == "" {
		serviceID = ev.InvolvedObject.Name
	}

	attrs := map[string]any{
		"k8s.reason":               ev.Reason,
		"k8s.resource.kind":        ev.InvolvedObject.Kind,
		"k8s.reporting_controller": ev.ReportingController,
	}
	if ev.InvolvedObject.Namespace != "" {
		attrs["k8s.namespace.name"] = ev.InvolvedObject.Namespace
	}
	setPodAttrs(attrs, ev.InvolvedObject)
	if ev.Message != "" {
		attrs["k8s.message"] = truncate(ev.Message, 1024)
	}
	if ev.Source.Component != "" {
		attrs["k8s.source.component"] = ev.Source.Component
	}
	if ev.Source.Host != "" {
		attrs["k8s.node.name"] = ev.Source.Host
	}

	// Prefer FirstTimestamp (human-reported) over EventTime for legacy events.
	occurredAt := wireformat.TimeToUnixNano(ev.FirstTimestamp.Time)
	if occurredAt == 0 {
		occurredAt = ev.EventTime.UnixNano()
	}

	out := wireformat.Event{
		ID:         ids.NewID(),
		Kind:       kind,
		Severity:   severityForKind(kind),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}

	// For legacy events, repeated occurrences are expressed via top-level
	// Count + LastTimestamp (rather than a nested Series block).
	if ev.Count >= 2 {
		firstAt := occurredAt
		lastAt := max(wireformat.TimeToUnixNano(ev.LastTimestamp.Time), firstAt)
		out.Series = &wireformat.Series{
			Count:   int64(ev.Count),
			FirstAt: firstAt,
			LastAt:  lastAt,
		}
	} else if ev.Series != nil && ev.Series.Count >= 2 {
		firstAt := occurredAt
		lastAt := max(ev.Series.LastObservedTime.UnixNano(), firstAt)
		out.Series = &wireformat.Series{
			Count:   int64(ev.Series.Count),
			FirstAt: firstAt,
			LastAt:  lastAt,
		}
	}

	return out, true, nil
}

// matchReason returns the v2 kind for a (reason, regardingKind) pair, or
// false if no mapping exists.
func matchReason(reason, regardingKind string) (wireformat.Kind, bool) {
	mapping, ok := k8sEventReasonMappings[reason]
	if !ok {
		return "", false
	}
	for _, k := range mapping.regardingKinds {
		if strings.EqualFold(k, regardingKind) {
			return mapping.kind, true
		}
	}
	return "", false
}

// setPodAttrs populates k8s.pod.name when the regarding object is a Pod.
func setPodAttrs(attrs map[string]any, ref corev1.ObjectReference) {
	if strings.EqualFold(ref.Kind, "Pod") && ref.Name != "" {
		attrs["k8s.pod.name"] = ref.Name
	}
}

// truncate returns s limited to max bytes with an ellipsis marker when
// clipped.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "\u2026"
}

// ----------------------------------------------------------------------------
// Node condition transitions → K8S_NODE_PRESSURE.
// ----------------------------------------------------------------------------

// pressureConditions is the list of node conditions the operator treats as
// "pressure" signals. Ready is inverted (False == pressure).
var pressureConditions = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure,
	corev1.NodeDiskPressure,
	corev1.NodePIDPressure,
}

// FromNodeConditionChange emits K8S_NODE_PRESSURE events for every pressure
// condition that transitioned to the unhealthy state. On OnAdd (old == nil),
// the mapper reports any currently-unhealthy conditions so the backend has a
// baseline.
func (m *Mapper) FromNodeConditionChange(_ context.Context, oldNode, newNode *corev1.Node) ([]wireformat.Event, error) {
	if newNode == nil {
		return nil, nil
	}

	var out []wireformat.Event
	for _, cond := range pressureConditions {
		newStatus := findNodeCondition(newNode.Status.Conditions, cond)
		if newStatus == nil || newStatus.Status != corev1.ConditionTrue {
			continue
		}
		if oldNode != nil {
			oldStatus := findNodeCondition(oldNode.Status.Conditions, cond)
			if oldStatus != nil && oldStatus.Status == corev1.ConditionTrue {
				// Already pressured — not a transition.
				continue
			}
		}
		out = append(out, m.nodePressureEvent(newNode, string(cond), newStatus))
	}

	// Ready is inverted: False (or Unknown) means the node is pressured /
	// offline. We only emit this when it flipped False since last observation.
	newReady := findNodeCondition(newNode.Status.Conditions, corev1.NodeReady)
	if newReady != nil && newReady.Status != corev1.ConditionTrue {
		ready := false
		if oldNode != nil {
			oldReady := findNodeCondition(oldNode.Status.Conditions, corev1.NodeReady)
			if oldReady != nil && oldReady.Status != corev1.ConditionTrue {
				ready = true // already not-ready, skip
			}
		}
		if !ready {
			out = append(out, m.nodePressureEvent(newNode, "NotReady", newReady))
		}
	}

	return out, nil
}

func (m *Mapper) nodePressureEvent(node *corev1.Node, reason string, cond *corev1.NodeCondition) wireformat.Event {
	attrs := map[string]any{
		"k8s.resource.kind": "Node",
		"k8s.node.name":     node.Name,
		"k8s.reason":        reason,
	}
	if cond.Reason != "" {
		attrs["k8s.condition.reason"] = cond.Reason
	}
	if cond.Message != "" {
		attrs["k8s.message"] = truncate(cond.Message, 1024)
	}

	occurredAt := wireformat.TimeToUnixNano(cond.LastTransitionTime.Time)
	if occurredAt == 0 {
		occurredAt = wireformat.TimeToUnixNano(cond.LastHeartbeatTime.Time)
	}

	return wireformat.Event{
		ID:         ids.NewID(),
		Kind:       wireformat.KindK8sNodePressure,
		Severity:   severityForKind(wireformat.KindK8sNodePressure),
		OccurredAt: occurredAt,
		ServiceID:  node.Name,
		Attributes: attrs,
	}
}

func findNodeCondition(conds []corev1.NodeCondition, t corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// HPA scale → K8S_HPA_SCALE.
// ----------------------------------------------------------------------------

// FromHPAScale emits K8S_HPA_SCALE when the HPA's current replica count
// changes. On add (old == nil), no event is emitted — the baseline is the
// current state.
func (m *Mapper) FromHPAScale(_ context.Context, old, n *autoscalingv2.HorizontalPodAutoscaler) (wireformat.Event, bool, error) {
	if n == nil {
		return wireformat.Event{}, false, nil
	}
	if old == nil {
		return wireformat.Event{}, false, nil
	}
	if old.Status.CurrentReplicas == n.Status.CurrentReplicas {
		return wireformat.Event{}, false, nil
	}

	reason := "ScaleUp"
	if n.Status.CurrentReplicas < old.Status.CurrentReplicas {
		reason = "ScaleDown"
	}

	attrs := map[string]any{
		"k8s.hpa.name":             n.Name,
		"k8s.namespace.name":       n.Namespace,
		"k8s.hpa.current_replicas": int(n.Status.CurrentReplicas),
		"k8s.hpa.desired_replicas": int(n.Status.DesiredReplicas),
		"k8s.reason":               reason,
	}
	if n.Spec.MinReplicas != nil {
		attrs["k8s.hpa.min_replicas"] = int(*n.Spec.MinReplicas)
	}
	attrs["k8s.hpa.max_replicas"] = int(n.Spec.MaxReplicas)

	// The HPA target workload provides the service identity. We cannot
	// resolve it via Resolver without a cross-namespace lookup, so we rely
	// on the HPA's own namespace + scale target name.
	serviceID := n.Spec.ScaleTargetRef.Name
	if serviceID == "" {
		serviceID = n.Name
	}

	// LastScaleTime is *metav1.Time and may be nil when the HPA has never fired.
	var occurredAt int64
	if n.Status.LastScaleTime != nil {
		occurredAt = wireformat.TimeToUnixNano(n.Status.LastScaleTime.Time)
	}

	return wireformat.Event{
		ID:         ids.NewID(),
		Kind:       wireformat.KindK8sHPAScale,
		Severity:   severityForKind(wireformat.KindK8sHPAScale),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}, true, nil
}

// ----------------------------------------------------------------------------
// Pod lifecycle → K8S_POD_STARTED / K8S_POD_TERMINATED.
// ----------------------------------------------------------------------------

// FromPodStatusChange emits K8S_POD_STARTED when a Pod transitions into
// Running and K8S_POD_TERMINATED when it transitions into Succeeded / Failed.
// When a Failed pod has a container that terminated with reason containerReasonOOMKilled,
// K8S_OOM_KILL is emitted instead — it is a Tier 1 kind that bypasses the
// severity filter, ensuring OOM events always reach the backend regardless of
// the configured minSeverity threshold.
func (m *Mapper) FromPodStatusChange(ctx context.Context, old, n *corev1.Pod) ([]wireformat.Event, error) {
	if n == nil {
		return nil, nil
	}
	// Treat "no previous state" as "phase was empty" so that ADDs of an
	// already-running pod emit a single K8S_POD_STARTED baseline (useful for
	// operator restarts).
	var oldPhase corev1.PodPhase
	if old != nil {
		oldPhase = old.Status.Phase
	}
	newPhase := n.Status.Phase
	if oldPhase == newPhase {
		return nil, nil
	}

	switch newPhase {
	case corev1.PodRunning:
		ev, err := m.buildPodLifecycleEvent(ctx, n, wireformat.KindK8sPodStarted, "Started")
		if err != nil {
			return nil, err
		}
		return []wireformat.Event{ev}, nil
	case corev1.PodSucceeded, corev1.PodFailed:
		kind := wireformat.KindK8sPodTerminated
		reason := "Completed"
		if newPhase == corev1.PodFailed {
			if podContainerOOMKilled(n) {
				// Upgrade to K8S_OOM_KILL so the event is Tier 1 and bypasses
				// the severity filter — OOM kills must always be delivered.
				kind = wireformat.KindK8sOOMKill
				reason = containerReasonOOMKilled
			} else {
				reason = "Failed"
			}
		}
		ev, err := m.buildPodLifecycleEvent(ctx, n, kind, reason)
		if err != nil {
			return nil, err
		}
		return []wireformat.Event{ev}, nil
	default:
		return nil, nil
	}
}

// podContainerOOMKilled reports whether any container in the pod was terminated
// with reason containerReasonOOMKilled (exit code 137 from the Linux OOM killer).
func podContainerOOMKilled(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == containerReasonOOMKilled {
			return true
		}
	}
	return false
}

func (m *Mapper) buildPodLifecycleEvent(ctx context.Context, pod *corev1.Pod, kind wireformat.Kind, reason string) (wireformat.Event, error) {
	res, err := m.Resolver.Resolve(ctx, pod)
	if err != nil {
		return wireformat.Event{}, fmt.Errorf("mapper.buildPodLifecycleEvent: %w", err)
	}
	serviceID := res.ServiceID
	if serviceID == "" {
		serviceID = pod.Name
	}

	attrs := map[string]any{
		"k8s.pod.name":       pod.Name,
		"k8s.namespace.name": pod.Namespace,
		"k8s.resource.kind":  "Pod",
		"k8s.reason":         reason,
	}
	if pod.Spec.NodeName != "" {
		attrs["k8s.node.name"] = pod.Spec.NodeName
	}
	if res.OwnerKind != "" {
		attrs["k8s.owner.kind"] = res.OwnerKind
	}
	if res.OwnerName != "" {
		attrs["k8s.owner.name"] = res.OwnerName
	}

	// Choose an occurred-at timestamp that best reflects the transition.
	var occurredAt int64
	switch kind {
	case wireformat.KindK8sPodStarted:
		if pod.Status.StartTime != nil {
			occurredAt = wireformat.TimeToUnixNano(pod.Status.StartTime.Time)
		}
	case wireformat.KindK8sPodTerminated, wireformat.KindK8sOOMKill:
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				occurredAt = wireformat.TimeToUnixNano(cs.State.Terminated.FinishedAt.Time)
				if cs.State.Terminated.Reason != "" {
					attrs["k8s.reason"] = cs.State.Terminated.Reason
				}
				if cs.Name != "" {
					attrs["k8s.container.name"] = cs.Name
				}
				attrs["k8s.exit_code"] = int(cs.State.Terminated.ExitCode)
				break
			}
		}
	}
	if occurredAt == 0 {
		occurredAt = wireformat.TimeToUnixNano(pod.CreationTimestamp.Time)
	}
	if len(identityRestartCounts(pod)) > 0 {
		attrs["k8s.restart_count"] = maxRestartCount(pod)
	}

	return wireformat.Event{
		ID:         ids.NewID(),
		Kind:       kind,
		Severity:   severityForKind(kind),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}, nil
}

func identityRestartCounts(pod *corev1.Pod) []int32 {
	if pod == nil {
		return nil
	}
	out := make([]int32, 0, len(pod.Status.ContainerStatuses))
	for _, cs := range pod.Status.ContainerStatuses {
		out = append(out, cs.RestartCount)
	}
	return out
}

func maxRestartCount(pod *corev1.Pod) int {
	var max int32
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	return int(max)
}

// ----------------------------------------------------------------------------
// Deployment rollout progress → K8S_DEPLOY_ROLLOUT (infrastructure domain).
//
// Per the non-negotiable in the phase 3a task, K8S_DEPLOY_ROLLOUT lives in
// this file (infrastructure domain). Deployment-domain DEPLOY_* kinds live
// in deployment.go.
// ----------------------------------------------------------------------------

// FromDeploymentRollout emits K8S_DEPLOY_ROLLOUT events when the Deployment
// rollout status transitions between "progressing", "complete", and "failed".
// The returned slice is non-nil only when the status actually changed.
func (m *Mapper) FromDeploymentRollout(ctx context.Context, old, n *appsv1.Deployment) ([]wireformat.Event, error) {
	if n == nil {
		return nil, nil
	}
	status := deploymentRolloutStatus(n)
	var prev string
	if old != nil {
		prev = deploymentRolloutStatus(old)
	}
	if status == prev {
		return nil, nil
	}

	res, err := m.Resolver.Resolve(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("mapper.FromDeploymentRollout: resolve: %w", err)
	}
	serviceID := res.ServiceID
	if serviceID == "" {
		serviceID = n.Name
	}

	attrs := map[string]any{
		"k8s.deployment.name": n.Name,
		"k8s.namespace.name":  n.Namespace,
		"k8s.resource.kind":   "Deployment",
		"k8s.rollout.status":  status,
	}
	if rev, ok := n.Annotations["deployment.kubernetes.io/revision"]; ok {
		if revNum, err2 := parseRevision(rev); err2 == nil {
			attrs["k8s.rollout.revision"] = revNum
		} else {
			attrs["k8s.rollout.revision"] = rev
		}
	}

	var occurredAt int64
	if cond := findDeploymentCondition(n.Status.Conditions, appsv1.DeploymentProgressing); cond != nil {
		occurredAt = wireformat.TimeToUnixNano(cond.LastUpdateTime.Time)
	}
	if occurredAt == 0 {
		occurredAt = wireformat.TimeToUnixNano(n.CreationTimestamp.Time)
	}

	ev := wireformat.Event{
		ID:         ids.NewID(),
		Kind:       wireformat.KindK8sDeployRollout,
		Severity:   severityForKind(wireformat.KindK8sDeployRollout),
		OccurredAt: occurredAt,
		ServiceID:  serviceID,
		Attributes: attrs,
	}
	return []wireformat.Event{ev}, nil
}

// deploymentRolloutStatus maps a Deployment's condition set onto a simple
// three-value rollout state. "" means "no progressing condition yet".
func deploymentRolloutStatus(d *appsv1.Deployment) string {
	progressing := findDeploymentCondition(d.Status.Conditions, appsv1.DeploymentProgressing)
	if progressing == nil {
		return ""
	}
	if progressing.Status == corev1.ConditionFalse && progressing.Reason == "ProgressDeadlineExceeded" {
		return "failed"
	}
	if progressing.Status == corev1.ConditionTrue && progressing.Reason == "NewReplicaSetAvailable" {
		return "complete"
	}
	if progressing.Status == corev1.ConditionTrue {
		return "progressing"
	}
	return "unknown"
}

// findDeploymentCondition finds a condition by type in the given slice.
func findDeploymentCondition(conds []appsv1.DeploymentCondition, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
