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

package filter_test

import (
	"testing"

	"github.com/incidentary/operator/internal/filter"
	"github.com/incidentary/operator/internal/wireformat"
)

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   wireformat.Severity
		want filter.SeverityLevel
	}{
		{wireformat.SeverityTrace, filter.LevelTrace},
		{wireformat.SeverityDebug, filter.LevelDebug},
		{wireformat.SeverityInfo, filter.LevelInfo},
		{wireformat.SeverityWarning, filter.LevelWarning},
		{wireformat.SeverityError, filter.LevelError},
		{wireformat.SeverityFatal, filter.LevelFatal},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			got := filter.ParseSeverity(tc.in)
			if got != tc.want {
				t.Fatalf("ParseSeverity(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSeverity_UnknownDefaultsToWarning(t *testing.T) {
	t.Parallel()

	got := filter.ParseSeverity(wireformat.Severity("bogus"))
	if got != filter.LevelWarning {
		t.Fatalf("ParseSeverity(bogus) = %v, want LevelWarning", got)
	}
}

func TestAccept_Tier1AlwaysPasses(t *testing.T) {
	t.Parallel()

	// Filter is at its most strict (fatal). Tier 1 events must still pass.
	f := filter.Filter{MinSeverity: filter.LevelFatal}

	tier1 := []wireformat.Kind{
		wireformat.KindK8sPodCrash,
		wireformat.KindK8sOOMKill,
		wireformat.KindK8sEviction,
		wireformat.KindK8sScheduleFail,
		wireformat.KindK8sImagePullFail,
		wireformat.KindK8sNodePressure,
		wireformat.KindK8sHPAScale,
	}
	for _, k := range tier1 {
		k := k
		t.Run(string(k), func(t *testing.T) {
			t.Parallel()
			ev := wireformat.Event{Kind: k, Severity: wireformat.SeverityTrace}
			if !f.Accept(ev) {
				t.Fatalf("Tier 1 kind %q must pass the filter even at the strictest threshold", k)
			}
		})
	}
}

func TestAccept_DeployAlwaysPasses(t *testing.T) {
	t.Parallel()

	f := filter.Filter{MinSeverity: filter.LevelFatal}

	deployKinds := []wireformat.Kind{
		wireformat.KindDeployStarted,
		wireformat.KindDeploySucceeded,
		wireformat.KindDeployFailed,
		wireformat.KindDeployCancelled,
		wireformat.KindDeployRolledBack,
	}
	for _, k := range deployKinds {
		k := k
		t.Run(string(k), func(t *testing.T) {
			t.Parallel()
			ev := wireformat.Event{Kind: k, Severity: wireformat.SeverityInfo}
			if !f.Accept(ev) {
				t.Fatalf("DEPLOY_* kind %q must always pass the filter", k)
			}
		})
	}
}

func TestAccept_Tier2ChecksSeverity(t *testing.T) {
	t.Parallel()

	// K8S_POD_STARTED is Tier 2 — it must respect MinSeverity.
	ev := wireformat.Event{Kind: wireformat.KindK8sPodStarted, Severity: wireformat.SeverityInfo}

	warn := filter.Filter{MinSeverity: filter.LevelWarning}
	if warn.Accept(ev) {
		t.Fatalf("K8S_POD_STARTED @ info must NOT pass when MinSeverity=warning")
	}

	info := filter.Filter{MinSeverity: filter.LevelInfo}
	if !info.Accept(ev) {
		t.Fatalf("K8S_POD_STARTED @ info must pass when MinSeverity=info")
	}
}

func TestAccept_UnknownKindChecksSeverity(t *testing.T) {
	t.Parallel()

	// A hypothetical future K8S_* kind that is not in the Tier 1 allowlist
	// still respects the filter.
	ev := wireformat.Event{Kind: wireformat.Kind("K8S_FOO"), Severity: wireformat.SeverityDebug}

	warn := filter.Filter{MinSeverity: filter.LevelWarning}
	if warn.Accept(ev) {
		t.Fatalf("unknown K8S_* kind @ debug must NOT pass when MinSeverity=warning")
	}

	debug := filter.Filter{MinSeverity: filter.LevelDebug}
	if !debug.Accept(ev) {
		t.Fatalf("unknown K8S_* kind @ debug must pass when MinSeverity=debug")
	}
}

func TestNewFromString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want filter.SeverityLevel
	}{
		{"trace", filter.LevelTrace},
		{"debug", filter.LevelDebug},
		{"info", filter.LevelInfo},
		{"warning", filter.LevelWarning},
		{"error", filter.LevelError},
		{"fatal", filter.LevelFatal},
		{"", filter.LevelWarning},        // empty defaults to warning
		{"garbage", filter.LevelWarning}, // unknown defaults to warning
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			f := filter.NewFromString(tc.in)
			if f.MinSeverity != tc.want {
				t.Fatalf("NewFromString(%q).MinSeverity = %v, want %v", tc.in, f.MinSeverity, tc.want)
			}
		})
	}
}

func TestIsTier1(t *testing.T) {
	t.Parallel()

	if !filter.IsTier1(wireformat.KindK8sOOMKill) {
		t.Fatal("K8S_OOM_KILL should be Tier 1")
	}
	if filter.IsTier1(wireformat.KindK8sPodStarted) {
		t.Fatal("K8S_POD_STARTED should NOT be Tier 1")
	}
	if filter.IsTier1(wireformat.KindDeployStarted) {
		t.Fatal("DEPLOY_STARTED is handled by domain rule, not Tier 1")
	}
}
