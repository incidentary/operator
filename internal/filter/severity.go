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

// Package filter implements the Philosophy 1 severity floor for K8s event
// filtering.
//
// Philosophy 1 rules (applied in order):
//  1. DEPLOY_* kinds always pass (deployment-domain events are expensive to
//     capture and few in number, but crucial for incident correlation).
//  2. Tier 1 infrastructure kinds — crashes, OOMs, evictions, schedule/image
//     failures, node pressure, HPA scale — always pass regardless of the
//     configured minimum severity.
//  3. Everything else (Tier 2: pod lifecycle, rollout progression, etc.) is
//     subject to the MinSeverity threshold.
package filter

import "github.com/incidentary/operator/internal/wireformat"

// SeverityLevel is a numeric ordering over wireformat.Severity values. It
// exists so the filter can compare severities with `>=` without string
// matching at every event.
type SeverityLevel int

const (
	LevelTrace SeverityLevel = iota
	LevelDebug
	LevelInfo
	LevelWarning
	LevelError
	LevelFatal
)

// ParseSeverity returns the numeric level for a wireformat.Severity. Unknown
// values default to LevelWarning — the safest fallback for an operator that
// is the last line of defence against noise.
func ParseSeverity(s wireformat.Severity) SeverityLevel {
	switch s {
	case wireformat.SeverityTrace:
		return LevelTrace
	case wireformat.SeverityDebug:
		return LevelDebug
	case wireformat.SeverityInfo:
		return LevelInfo
	case wireformat.SeverityWarning:
		return LevelWarning
	case wireformat.SeverityError:
		return LevelError
	case wireformat.SeverityFatal:
		return LevelFatal
	default:
		return LevelWarning
	}
}

// tier1Kinds always pass the filter regardless of severity. These are the
// "something is on fire" kinds — every on-call engineer wants to see them.
var tier1Kinds = map[wireformat.Kind]bool{
	wireformat.KindK8sPodCrash:      true,
	wireformat.KindK8sOOMKill:       true,
	wireformat.KindK8sEviction:      true,
	wireformat.KindK8sScheduleFail:  true,
	wireformat.KindK8sImagePullFail: true,
	wireformat.KindK8sNodePressure:  true,
	wireformat.KindK8sHPAScale:      true,
}

// Filter accepts a wireformat.Event and returns true if the event should be
// forwarded to Incidentary.
type Filter struct {
	MinSeverity SeverityLevel
}

// NewFromString constructs a Filter from a string severity (as configured in
// IncidentaryConfig.Spec.EventFilters.MinSeverity). Unknown values fall back
// to LevelWarning via ParseSeverity.
func NewFromString(minSeverity string) Filter {
	return Filter{MinSeverity: ParseSeverity(wireformat.Severity(minSeverity))}
}

// Accept returns true if the event passes the Philosophy 1 filter.
func (f Filter) Accept(ev wireformat.Event) bool {
	if ev.Kind.Domain() == wireformat.DomainDeployment {
		return true
	}
	if tier1Kinds[ev.Kind] {
		return true
	}
	return ParseSeverity(ev.Severity) >= f.MinSeverity
}

// IsTier1 reports whether the given kind is a Tier 1 "always pass" kind.
// Exported so tests and pattern detection can reason about it without
// importing the internal map.
func IsTier1(k wireformat.Kind) bool {
	return tier1Kinds[k]
}
