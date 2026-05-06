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

// Incident-domain mapper — placeholder.
//
// The Incidentary V2 spec defines incident-domain event kinds:
//
//   INCIDENT_TRIGGERED
//   INCIDENT_ACKNOWLEDGED
//   INCIDENT_RESOLVED
//   INCIDENT_ESCALATED
//   INCIDENT_ANNOTATED
//
// These events originate from external incident-management systems
// (PagerDuty, OpsGenie, Slack workflows) via their respective webhook
// integrations — NOT from Kubernetes resources.
//
// The operator does not generate incident-domain events today because
// Kubernetes has no native incident CRD. If a future integration adds
// custom CRDs or annotations that represent incident lifecycle events
// (e.g. an IncidentAnnotation CR created by a ChatOps bot), this file
// is where the mapper would be implemented.
//
// Expected signature when needed:
//
//   func (m *Mapper) FromIncidentAnnotation(
//       ctx context.Context,
//       obj client.Object,
//   ) (*wireformat.Event, error)
