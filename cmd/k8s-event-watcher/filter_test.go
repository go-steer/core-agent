// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import "testing"

func makeEvent(reason, namespace string, count int) TriageEvent {
	return TriageEvent{
		Key:       EventKey{UID: "u1", Reason: reason},
		Namespace: namespace,
		Count:     count,
	}
}

func TestFilter_Accept_DefaultReasons(t *testing.T) {
	t.Parallel()
	f := newFilter(newFilterConfig(nil, nil, nil, 0))
	// Every default reason should accept a plain event (count=1
	// suffices unless it's Unhealthy, which needs count>=3).
	for _, reason := range defaultReasons {
		count := 1
		if reason == "Unhealthy" {
			count = 3 // meet the threshold for Unhealthy
		}
		if !f.Accept(makeEvent(reason, "default", count)) {
			t.Errorf("default reason %s rejected", reason)
		}
	}
	if f.Accept(makeEvent("SomeRandomReason", "default", 1)) {
		t.Error("non-default reason should be rejected")
	}
}

func TestFilter_Accept_CustomAllowList(t *testing.T) {
	t.Parallel()
	f := newFilter(newFilterConfig([]string{"CustomReason"}, nil, nil, 0))
	if !f.Accept(makeEvent("CustomReason", "default", 1)) {
		t.Error("custom-listed reason should accept")
	}
	// The shipped defaults are NOT included when a custom list
	// is supplied — the operator's list is the complete set.
	if f.Accept(makeEvent("CrashLoopBackOff", "default", 1)) {
		t.Error("non-custom reason should reject when custom list is set")
	}
}

func TestFilter_Accept_ExcludedNamespaceWins(t *testing.T) {
	t.Parallel()
	// Exclude takes precedence over include (operator can express
	// "everything except kube-system" without listing every
	// included namespace).
	f := newFilter(newFilterConfig(nil, []string{"default", "kube-system"}, []string{"kube-system"}, 0))
	if f.Accept(makeEvent("CrashLoopBackOff", "kube-system", 1)) {
		t.Error("excluded namespace should reject even when listed as allowed")
	}
	if !f.Accept(makeEvent("CrashLoopBackOff", "default", 1)) {
		t.Error("allowed namespace (not excluded) should accept")
	}
}

func TestFilter_Accept_AllowNamespacesLimitsScope(t *testing.T) {
	t.Parallel()
	f := newFilter(newFilterConfig(nil, []string{"prod"}, nil, 0))
	if !f.Accept(makeEvent("CrashLoopBackOff", "prod", 1)) {
		t.Error("prod namespace should accept when allow-listed")
	}
	if f.Accept(makeEvent("CrashLoopBackOff", "dev", 1)) {
		t.Error("dev namespace should reject when only prod is allowed")
	}
}

func TestFilter_Accept_UnhealthyRequiresMinCount(t *testing.T) {
	t.Parallel()
	// Default unhealthy-min-count is 3.
	f := newFilter(newFilterConfig(nil, nil, nil, 0))
	if f.Accept(makeEvent("Unhealthy", "default", 1)) {
		t.Error("Unhealthy count=1 should reject (below threshold 3)")
	}
	if f.Accept(makeEvent("Unhealthy", "default", 2)) {
		t.Error("Unhealthy count=2 should reject")
	}
	if !f.Accept(makeEvent("Unhealthy", "default", 3)) {
		t.Error("Unhealthy count=3 should accept (meets threshold)")
	}
	if !f.Accept(makeEvent("Unhealthy", "default", 100)) {
		t.Error("Unhealthy count=100 should accept")
	}
}

func TestFilter_Accept_UnhealthyThresholdOverridable(t *testing.T) {
	t.Parallel()
	// Custom threshold of 10 — probe-flap tolerance turned up.
	f := newFilter(newFilterConfig(nil, nil, nil, 10))
	if f.Accept(makeEvent("Unhealthy", "default", 5)) {
		t.Error("Unhealthy count=5 should reject with threshold 10")
	}
	if !f.Accept(makeEvent("Unhealthy", "default", 10)) {
		t.Error("Unhealthy count=10 should accept with threshold 10")
	}
}

func TestFilter_Accept_UnhealthyThresholdDoesntAffectOtherReasons(t *testing.T) {
	t.Parallel()
	// The count-threshold rule is Unhealthy-specific; other
	// reasons fire on count=1.
	f := newFilter(newFilterConfig(nil, nil, nil, 100))
	if !f.Accept(makeEvent("CrashLoopBackOff", "default", 1)) {
		t.Error("CrashLoopBackOff count=1 should always accept")
	}
}
