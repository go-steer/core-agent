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

// Originally derived from go-steer/mast.

package compose

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/internal/notify"
	"github.com/go-steer/core-agent/v3/pkg/config"
)

type fakeSwitchboard struct {
	channel  string
	posts    []fakeSwitchboardSend
	appends  []fakeSwitchboardSend
	replaces []fakeSwitchboardSend

	postErr    error
	appendErr  error
	replaceErr error
	rollTo     *notify.Ref
	seq        int
}

type fakeSwitchboardSend struct {
	Channel   string
	ID        string
	Text      string
	SessionID string
}

func (f *fakeSwitchboard) Post(_ context.Context, text, sessionID string) (notify.Ref, error) {
	ch := f.channel
	if ch == "" {
		ch = "#sre-oncall"
	}
	f.posts = append(f.posts, fakeSwitchboardSend{Channel: ch, Text: text, SessionID: sessionID})
	if err := f.postErr; err != nil {
		f.postErr = nil
		return notify.Ref{}, err
	}
	f.seq++
	return notify.Ref{Channel: ch, ID: fmt.Sprintf("m%d", f.seq)}, nil
}

func (f *fakeSwitchboard) Replace(_ context.Context, ref notify.Ref, text string) (notify.Ref, error) {
	f.replaces = append(f.replaces, fakeSwitchboardSend{Channel: ref.Channel, ID: ref.ID, Text: text})
	if err := f.replaceErr; err != nil {
		f.replaceErr = nil
		return notify.Ref{}, err
	}
	return ref, nil
}

func (f *fakeSwitchboard) Append(_ context.Context, ref notify.Ref, line string) (notify.Ref, error) {
	f.appends = append(f.appends, fakeSwitchboardSend{Channel: ref.Channel, ID: ref.ID, Text: line})
	if err := f.appendErr; err != nil {
		f.appendErr = nil
		return notify.Ref{}, err
	}
	if f.rollTo != nil {
		rolled := *f.rollTo
		f.rollTo = nil
		return rolled, nil
	}
	return ref, nil
}

func testSetWithEdge(edge, subject string) *monitor.Set {
	return &monitor.Set{
		Transitions: []monitor.Transition{
			{Kind: "pod", Edge: edge, SubjectKey: subject},
		},
	}
}

func testQuietSet() *monitor.Set {
	return &monitor.Set{Transitions: []monitor.Transition{}}
}

func TestNotifier_TimelineGrowsThenClosesOnQuiet(t *testing.T) {
	t.Parallel()
	fake := &fakeSwitchboard{channel: "#sre-oncall"}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	n, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-oncall",
			Update:  config.MonitorNotifyUpdateTimeline,
		},
		Client: fake,
		Now:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	ctx := context.Background()
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s1"), "first finding", false); err != nil {
		t.Fatalf("first AfterCycle: %v", err)
	}
	if len(fake.posts) != 1 || fake.posts[0].Channel != "#sre-oncall" {
		t.Fatalf("posts = %+v, want 1 post to #sre-oncall", fake.posts)
	}

	if err := n.AfterCycle(ctx, testSetWithEdge("re_opened", "s1"), "re-opened finding", false); err != nil {
		t.Fatalf("second AfterCycle: %v", err)
	}
	if len(fake.posts) != 1 || len(fake.appends) != 1 || fake.appends[0].ID != "m1" {
		t.Fatalf("posts=%d appends=%+v, want append to m1", len(fake.posts), fake.appends)
	}

	// Quiet cycle closes the timeline.
	if err := n.AfterCycle(ctx, testQuietSet(), "", false); err != nil {
		t.Fatalf("quiet AfterCycle: %v", err)
	}

	// Next incident opens a new message.
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s2"), "new incident", false); err != nil {
		t.Fatalf("fourth AfterCycle: %v", err)
	}
	if len(fake.posts) != 2 {
		t.Errorf("posts = %d, want 2 after timeline closed on quiet cycle", len(fake.posts))
	}
}

func TestNotifier_TimelineFallbacksAndRollover(t *testing.T) {
	t.Parallel()
	fake := &fakeSwitchboard{channel: "#sre-oncall"}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	n, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-oncall",
			Update:  config.MonitorNotifyUpdateTimeline,
		},
		Client: fake,
		Now:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	ctx := context.Background()
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s1"), "first", false); err != nil {
		t.Fatalf("first: %v", err)
	}

	// 1. 409 ErrSendFullText falls back to Replace with full text.
	fake.appendErr = &notify.Error{Status: 409, Err: notify.ErrSendFullText}
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s2"), "second", false); err != nil {
		t.Fatalf("second (409 fallback): %v", err)
	}
	if len(fake.replaces) != 1 || fake.replaces[0].Text != "first\n\nsecond" {
		t.Fatalf("replaces = %+v, want full text 'first\\n\\nsecond'", fake.replaces)
	}

	// 2. Rollover retargets subsequent appends to the continuation ref.
	fake.rollTo = &notify.Ref{Channel: "#sre-oncall", ID: "m1-cont"}
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s3"), "third", false); err != nil {
		t.Fatalf("third (rollover): %v", err)
	}
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s4"), "fourth", false); err != nil {
		t.Fatalf("fourth (after rollover): %v", err)
	}
	if len(fake.appends) != 3 || fake.appends[2].ID != "m1-cont" {
		t.Fatalf("appends = %+v, want append targeting m1-cont", fake.appends)
	}

	// 3. 409 on append + 501 on replace posts a fresh timeline carrying the continuation text.
	fake.appendErr = &notify.Error{Status: 409, Err: notify.ErrSendFullText}
	fake.replaceErr = &notify.Error{Status: 501, Err: notify.ErrEditUnsupported}
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s5"), "fifth", false); err != nil {
		t.Fatalf("fifth (edit unsupported fallback): %v", err)
	}
	if len(fake.posts) != 2 || fake.posts[1].Text != "third\n\nfourth\n\nfifth" {
		t.Errorf("posts = %+v, want fresh post with 'third\\n\\nfourth\\n\\nfifth'", fake.posts)
	}
}

func TestNotifier_ReplaceMode(t *testing.T) {
	t.Parallel()
	fake := &fakeSwitchboard{channel: "#sre-oncall"}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	n, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-oncall",
			Update:  config.MonitorNotifyUpdateReplace,
		},
		Client: fake,
		Now:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	ctx := context.Background()
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s1"), "v1 summary", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := n.AfterCycle(ctx, testSetWithEdge("re_opened", "s1"), "v2 summary", false); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(fake.posts) != 1 || len(fake.replaces) != 1 || fake.replaces[0].Text != "v2 summary" {
		t.Fatalf("posts=%d replaces=%+v, want 1 post and 1 replace with v2 summary", len(fake.posts), fake.replaces)
	}
}

func TestNotifier_RemindAfterAndDigestAfter(t *testing.T) {
	t.Parallel()
	fake := &fakeSwitchboard{channel: "#sre-oncall"}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	n, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		Notify: &config.MonitorNotifyConfig{
			Channel:     "#sre-oncall",
			RemindAfter: "1h",
			DigestAfter: "6h",
		},
		Client: fake,
		Now:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	ctx := context.Background()
	// 1. Open subject s1 at T=0.
	if err := n.AfterCycle(ctx, testSetWithEdge("opened", "s1"), "opened s1", false); err != nil {
		t.Fatalf("open s1: %v", err)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(fake.posts))
	}

	// 2. At T+30m, s1 remained -> before remind_after (1h), stays quiet.
	clock = clock.Add(30 * time.Minute)
	if err := n.AfterCycle(ctx, testSetWithEdge("remained", "s1"), "still open", false); err != nil {
		t.Fatalf("remained at 30m: %v", err)
	}
	if len(fake.posts) != 1 || len(fake.appends) != 0 {
		t.Fatalf("sends at 30m: posts=%d appends=%d, want no send before remind_after", len(fake.posts), len(fake.appends))
	}

	// 3. At T+60m, s1 remained -> remind_after elapsed, speaks!
	clock = clock.Add(30 * time.Minute)
	if err := n.AfterCycle(ctx, testSetWithEdge("remained", "s1"), "reminder for s1", false); err != nil {
		t.Fatalf("remained at 60m: %v", err)
	}
	if len(fake.posts) != 2 {
		t.Fatalf("posts at 60m = %d, want 2 (reminder sent)", len(fake.posts))
	}

	// 4. Advance 6h with quiet cycles -> digest_after triggers!
	clock = clock.Add(6 * time.Hour)
	if !n.isDigestDue(ctx, clock) {
		t.Fatal("isDigestDue = false after 6h, want true")
	}
	if err := n.AfterCycle(ctx, testQuietSet(), "6h all-quiet digest", false); err != nil {
		t.Fatalf("digest AfterCycle: %v", err)
	}
	if len(fake.appends) != 1 || fake.appends[0].Text != "6h all-quiet digest" {
		t.Fatalf("appends = %+v, want digest sent", fake.appends)
	}

	// 5. Subject s2 first observed in "remained" state seeds LastRemindedAt on
	// first check and triggers after remind_after (1h).
	if n.isRemindDue(ctx, "s2", clock) {
		t.Fatal("isRemindDue(s2) on first observation = true, want false (seeded)")
	}
	if !n.isRemindDue(ctx, "s2", clock.Add(time.Hour)) {
		t.Fatal("isRemindDue(s2) after 1h = false, want true")
	}
}

func TestNotifier_DurableStateSurvivesRestartAndNoReplayOnFailure(t *testing.T) {
	t.Parallel()
	hLog := openMonitorTestEventLog(t)
	fake := &fakeSwitchboard{channel: "#sre-oncall"}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)

	n1, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		AppName:      "core-agent",
		EventLog:     hLog,
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-oncall",
			Update:  config.MonitorNotifyUpdateTimeline,
		},
		Client: fake,
		Now:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewNotifier n1: %v", err)
	}

	ctx := context.Background()
	if err := n1.AfterCycle(ctx, testSetWithEdge("opened", "s1"), "cycle 1 text", false); err != nil {
		t.Fatalf("n1 AfterCycle: %v", err)
	}

	// Simulate process restart by constructing n2 against the same eventlog.Handle.
	n2, err := NewNotifier(NotifierConfig{
		WorkloadName: "cluster-watch",
		AppName:      "core-agent",
		EventLog:     hLog,
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-oncall",
			Update:  config.MonitorNotifyUpdateTimeline,
		},
		Client: fake,
		Now:    func() time.Time { return clock.Add(15 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewNotifier n2: %v", err)
	}
	if err := n2.AfterCycle(ctx, testSetWithEdge("re_opened", "s1"), "cycle 2 text", false); err != nil {
		t.Fatalf("n2 AfterCycle: %v", err)
	}
	if len(fake.appends) != 1 || fake.appends[0].ID != "m1" {
		t.Fatalf("appends after restart = %+v, want append to persisted ref m1", fake.appends)
	}

	// Now make the next send fail (403 Denied) and verify the failed text is NOT replayed on the following cycle.
	fake.appendErr = &notify.Error{Status: 403, Err: notify.ErrDenied}
	if err := n2.AfterCycle(ctx, testSetWithEdge("opened", "s2"), "lost cycle text", false); err == nil {
		t.Fatal("AfterCycle succeeded when switchboard returned 403")
	}

	if err := n2.AfterCycle(ctx, testSetWithEdge("opened", "s3"), "fresh cycle text", false); err != nil {
		t.Fatalf("AfterCycle after failure: %v", err)
	}
	if len(fake.posts) != 2 || strings.Contains(fake.posts[1].Text, "lost cycle text") || fake.posts[1].Text != "fresh cycle text" {
		t.Errorf("posts = %+v, want fresh post containing only 'fresh cycle text' (no replay of failed notify)", fake.posts)
	}
}
