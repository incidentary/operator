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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// IncidentaryConfigSpec defines the desired state of IncidentaryConfig.
type IncidentaryConfigSpec struct {
	// apiKeySecretRef references the Kubernetes Secret containing the
	// Incidentary workspace API key.
	// +kubebuilder:validation:Required
	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`

	// workspaceID is the Incidentary workspace identifier embedded in every
	// ingest batch as agent.workspace_id. Without it, the server rejects
	// every batch with WORKSPACE_MISMATCH (422). Not a secret — safe to
	// commit to git or set via Helm values.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	WorkspaceID string `json:"workspaceId"`

	// watchNamespaces restricts the operator to these namespaces.
	// When empty (default), the operator watches all namespaces except those
	// listed in excludeNamespaces.
	// +optional
	WatchNamespaces []string `json:"watchNamespaces,omitempty"`

	// excludeNamespaces is a list of namespaces the operator will not watch.
	// Defaults to: kube-system, kube-public, kube-node-lease.
	// +optional
	ExcludeNamespaces []string `json:"excludeNamespaces,omitempty"`

	// reconciliationIntervalSeconds is how often the operator re-discovers
	// workloads and sends topology reports.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=3600
	// +optional
	ReconciliationIntervalSeconds int32 `json:"reconciliationIntervalSeconds,omitempty"`

	// eventFilters controls which K8s events are sent to Incidentary.
	// +optional
	EventFilters EventFilters `json:"eventFilters,omitempty"`

	// ingestEndpoint overrides the default Incidentary ingest URL.
	// Default: https://api.incidentary.com/api/v2/ingest
	// +optional
	IngestEndpoint string `json:"ingestEndpoint,omitempty"`

	// topologyEndpoint overrides the default Incidentary topology URL.
	// Default: https://api.incidentary.com/api/v2/workspace/topology
	// +optional
	TopologyEndpoint string `json:"topologyEndpoint,omitempty"`
}

// SecretKeyRef references a specific key within a Kubernetes Secret.
type SecretKeyRef struct {
	// name is the Secret name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// key is the entry within the Secret data that holds the value.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// EventFilters configures the operator's event filtering rules.
type EventFilters struct {
	// minSeverity is the lowest severity level that will be sent to Incidentary.
	// Tier 1 events (crashes, OOMs, failures, deploys) always pass regardless of this threshold.
	// +kubebuilder:validation:Enum=trace;debug;info;warning;error;fatal
	// +kubebuilder:default=warning
	// +optional
	MinSeverity string `json:"minSeverity,omitempty"`
}

// IncidentaryConfigStatus defines the observed state of IncidentaryConfig.
type IncidentaryConfigStatus struct {
	// phase is a high-level summary: Running, Degraded, Error, or empty (not yet reconciled).
	// +optional
	Phase string `json:"phase,omitempty"`

	// lastReconciliation is the timestamp of the most recent successful reconciliation cycle.
	// +optional
	LastReconciliation *metav1.Time `json:"lastReconciliation,omitempty"`

	// watchedWorkloads is the count of Deployments/StatefulSets/DaemonSets currently tracked.
	// +optional
	WatchedWorkloads int32 `json:"watchedWorkloads,omitempty"`

	// matchedServices is the count of watched workloads that match an SDK-registered service.
	// +optional
	MatchedServices int32 `json:"matchedServices,omitempty"`

	// unmatchedWorkloads is the count of watched workloads that do not match any registered service.
	// +optional
	UnmatchedWorkloads int32 `json:"unmatchedWorkloads,omitempty"`

	// conditions represent the current state of the IncidentaryConfig resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Ready": the operator has successfully reconciled the config and is watching resources
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=inc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Watched",type=integer,JSONPath=`.status.watchedWorkloads`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.matchedServices`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IncidentaryConfig is the Schema for the incidentaryconfigs API
type IncidentaryConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IncidentaryConfig
	// +required
	Spec IncidentaryConfigSpec `json:"spec"`

	// status defines the observed state of IncidentaryConfig
	// +optional
	Status IncidentaryConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IncidentaryConfigList contains a list of IncidentaryConfig
type IncidentaryConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IncidentaryConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IncidentaryConfig{}, &IncidentaryConfigList{})
}
