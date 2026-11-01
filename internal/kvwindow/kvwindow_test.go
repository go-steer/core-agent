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

package kvwindow_test

import (
	"testing"
	"time"

	"github.com/go-steer/core-models/kvmetrics"

	"github.com/go-steer/core-agent/v3/internal/kvwindow"
)

func TestFoldAndFrom(t *testing.T) {
	var f kvwindow.Fold
	if f.Window() != nil {
		t.Fatal("empty fold returned non-nil window")
	}
	f.Add(nil)
	if f.Window() != nil {
		t.Fatal("nil add produced non-nil window")
	}

	q1, h1, p1 := 100.0, 25.0, 0.2
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	w1 := kvwindow.From(kvmetrics.Window{
		Start:       t0,
		End:         t1,
		Queries:     &q1,
		Hits:        &h1,
		PeakKVUsage: &p1,
		Readings:    2,
	})
	f.Add(w1)

	q2, h2, p2 := 300.0, 75.0, 0.5
	t2 := t1.Add(time.Minute)
	w2 := &kvwindow.Window{
		Start:       t1,
		End:         t2,
		Queries:     &q2,
		Hits:        &h2,
		PeakKVUsage: &p2,
		Readings:    3,
		EndFailed:   true,
	}
	f.Add(w2)

	got := f.Window()
	if got == nil {
		t.Fatal("expected folded window")
	}
	if got.Windows != 2 || got.Readings != 5 || !got.EndFailed {
		t.Errorf("got = %+v", got)
	}
	if *got.Queries != 400 || *got.Hits != 100 || *got.HitRate != 0.25 || *got.PeakKVUsage != 0.5 {
		t.Errorf("got counts = %+v", got)
	}
	if !got.Start.Equal(t0) || !got.End.Equal(t2) {
		t.Errorf("span = %v..%v, want %v..%v", got.Start, got.End, t0, t2)
	}
}
