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

// Package kvwindow is the shape in which a model server's KV-cache
// activity is reported, and the fold that sums windows for the judged
// eval board (internal/evals/harness).
//
// Every window is fleet-level and time-correlated: the server's whole
// traffic between two readings (core-models kvmetrics), never a
// session's share of it, and never folded into cost.
package kvwindow

import (
	"time"

	"github.com/go-steer/core-models/kvmetrics"
)

// Window is one window of KV-cache activity, or a fold of several.
// Counts are tokens as the server reports them; a field the server did
// not expose is nil and omitted, never zero.
type Window struct {
	// Fleet is always true: the server's whole traffic in the window.
	Fleet bool      `json:"fleet"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Queries and Hits are prefix-cache tokens queried and hit.
	Queries *float64 `json:"queries_tokens,omitempty"`
	Hits    *float64 `json:"hits_tokens,omitempty"`
	// HitRate is Hits/Queries from the counters; never from a gauge.
	HitRate *float64 `json:"hit_rate,omitempty"`
	// HitRateGauge is the server's own instantaneous hit-rate gauge at
	// the window's last reading, for servers without counters. A fold
	// does not carry it: an instantaneous value is not a sum.
	HitRateGauge *float64 `json:"hit_rate_gauge,omitempty"`
	// PeakKVUsage is the highest KV utilization (0–1) seen.
	PeakKVUsage *float64 `json:"peak_kv_usage,omitempty"`
	Readings    int      `json:"readings"`
	// Reset: an engine's counters went backwards inside the window.
	Reset bool `json:"reset,omitempty"`
	// EndFailed: the closing reading could not be taken, so the window
	// ends at the last good one.
	EndFailed bool `json:"end_failed,omitempty"`
	// Approximate: a bracketing reading was not taken at the window's
	// edge, so the window reaches up to one poll interval past it (see
	// the producer's docs).
	Approximate bool `json:"approximate,omitempty"`
	// Windows is how many windows a fold summed; zero on a single one.
	Windows int `json:"windows,omitempty"`
}

// From converts a kvmetrics window.
func From(w kvmetrics.Window) *Window {
	return &Window{
		Fleet: true, Start: w.Start, End: w.End,
		Queries: w.Queries, Hits: w.Hits, HitRate: w.HitRate, HitRateGauge: w.HitRateGauge,
		PeakKVUsage: w.PeakKVUsage, Readings: w.Readings, Reset: w.Reset, EndFailed: w.EndFailed,
	}
}

// Fold sums windows: counts added (absent until one reports them), the
// hit rate recomputed from the sums, the peak the highest, the time span
// the union, and Reset, EndFailed and Approximate if any window's were.
// The zero value is an empty fold. Not safe for concurrent use.
type Fold struct {
	w Window
}

// Add folds one window in; a nil window is skipped.
func (f *Fold) Add(w *Window) {
	if w == nil {
		return
	}
	s := &f.w
	if s.Windows == 0 || w.Start.Before(s.Start) {
		s.Start = w.Start
	}
	if w.End.After(s.End) {
		s.End = w.End
	}
	s.Windows++
	s.Fleet = true
	s.Queries = add(s.Queries, w.Queries)
	s.Hits = add(s.Hits, w.Hits)
	s.HitRate = nil
	if s.Queries != nil && s.Hits != nil && *s.Queries > 0 {
		r := *s.Hits / *s.Queries
		s.HitRate = &r
	}
	if w.PeakKVUsage != nil && (s.PeakKVUsage == nil || *w.PeakKVUsage > *s.PeakKVUsage) {
		v := *w.PeakKVUsage
		s.PeakKVUsage = &v
	}
	s.Readings += w.Readings
	s.Reset = s.Reset || w.Reset
	s.EndFailed = s.EndFailed || w.EndFailed
	s.Approximate = s.Approximate || w.Approximate
}

// Window returns the fold so far, or nil when nothing was added.
func (f *Fold) Window() *Window {
	if f.w.Windows == 0 {
		return nil
	}
	w := f.w
	return &w
}

func add(a, b *float64) *float64 {
	switch {
	case b == nil:
		return a
	case a == nil:
		v := *b
		return &v
	default:
		v := *a + *b
		return &v
	}
}
