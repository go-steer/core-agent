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

import (
	"bytes"
	"context"
	"flag"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

func TestNormalizeResumeArgs(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("core-agent", flag.ContinueOnError)
	_ = fs.String("p", "", "")
	_ = fs.String("resume", "", "")
	_ = fs.Bool("yolo", false, "")

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "bare --resume at end",
			in:   []string{"--resume"},
			want: []string{"--resume=" + resumePickSentinel},
		},
		{
			name: "bare -resume followed by another flag",
			in:   []string{"-resume", "-p", "continue from here"},
			want: []string{"--resume=" + resumePickSentinel, "-p", "continue from here"},
		},
		{
			name: "explicit --resume <id>",
			in:   []string{"--resume", "proj-112233445566-aabbccdd", "-p", "hi"},
			want: []string{"--resume", "proj-112233445566-aabbccdd", "-p", "hi"},
		},
		{
			name: "explicit --resume=<id>",
			in:   []string{"--resume=proj-112233445566-aabbccdd"},
			want: []string{"--resume=proj-112233445566-aabbccdd"},
		},
		{
			name: "empty --resume= becomes pick sentinel",
			in:   []string{"--resume="},
			want: []string{"--resume=" + resumePickSentinel},
		},
		{
			name: "stops at double dash",
			in:   []string{"-p", "hi", "--", "--resume"},
			want: []string{"-p", "hi", "--", "--resume"},
		},
		{
			name: "value of preceding -p flag equal to --resume is not rewritten",
			in:   []string{"-p", "--resume", "--yolo"},
			want: []string{"-p", "--resume", "--yolo"},
		},
		{
			name: "boolean flag before bare --resume still rewrites --resume",
			in:   []string{"--yolo", "--resume"},
			want: []string{"--yolo", "--resume=" + resumePickSentinel},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeResumeArgs(tc.in, fs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeResumeArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveSessionDBWithResume(t *testing.T) {
	t.Parallel()

	enabled, implied, by, conflict := resolveSessionDBWithResume(false, false, "", "", "", false, true)
	if !enabled || !implied || by != "--continue/--resume" || conflict {
		t.Errorf("resuming with no flags = (%v, %v, %q, %v), want (true, true, \"--continue/--resume\", false)", enabled, implied, by, conflict)
	}

	_, _, _, conflict = resolveSessionDBWithResume(false, true, "", "", "", false, true)
	if !conflict {
		t.Error("expected conflict when --session-db=false is combined with --continue/--resume")
	}

	enabled, implied, _, conflict = resolveSessionDBWithResume(false, true, "/tmp/custom.db", "", "", false, true)
	if conflict || enabled || implied {
		t.Errorf("explicit path with --session-db=false = (%v, %v, %v), want (false, false, false)", enabled, implied, conflict)
	}
}

func TestTwoProjectsSharingOneDBGetDistinctSessionsAndContinueResumesPerProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "shared-sessions.db")
	projA := t.TempDir()
	projB := t.TempDir()

	if projectSessionPrefix(projA) == projectSessionPrefix(projB) {
		t.Fatalf("distinct project roots produced identical prefix %q", projectSessionPrefix(projA))
	}

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderEcho

	runTurn := func(projectRoot string, sessCfg sessionOpts, prompt string) (string, bool, *usage.Tracker, int) {
		t.Helper()
		tracker := usage.NewTracker()
		var notices []string
		handle, sid, resumed, opts, err := openCLISessionAndEventlog(
			ctx, cfg, projectRoot, sessCfg, attachOpts{}, prompt, "",
			tracker, "echo", usage.Pricing{InputPerMTok: 1, OutputPerMTok: 2},
			nil, nil, func(s string) { notices = append(notices, s) },
		)
		if err != nil {
			t.Fatalf("openCLISessionAndEventlog(%s): %v", projectRoot, err)
		}
		defer func() {
			if handle != nil {
				_ = handle.Close()
			}
		}()
		ag, err := agent.New(mustEchoModel(t), opts...)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		for _, err := range ag.Run(ctx, prompt) {
			if err != nil {
				t.Fatalf("ag.Run: %v", err)
			}
		}
		tracker.Append("echo", 10, 5, usage.Pricing{InputPerMTok: 1, OutputPerMTok: 2})
		resp, err := handle.Service.Get(ctx, &session.GetRequest{
			AppName:   agent.DefaultAppName,
			UserID:    cliSessionUserID,
			SessionID: sid,
		})
		if err != nil {
			t.Fatalf("Service.Get(%s): %v", sid, err)
		}
		evCount := 0
		for range resp.Session.Events().All() {
			evCount++
		}
		return sid, resumed, tracker, evCount
	}

	// 1. Fresh run in Project A and Project B sharing one SQLite DB.
	sidA, resumedA, _, evA1 := runTurn(projA, sessionOpts{sessionDB: true, sessionDBPath: dbPath}, "hello from project A")
	sidB, resumedB, _, evB1 := runTurn(projB, sessionOpts{sessionDB: true, sessionDBPath: dbPath}, "hello from project B")
	if resumedA || resumedB {
		t.Fatalf("fresh runs reported resumed: A=%v B=%v", resumedA, resumedB)
	}
	if sidA == sidB {
		t.Fatalf("two projects sharing one DB got the same session ID %q", sidA)
	}
	if !strings.HasPrefix(sidA, projectSessionPrefix(projA)+"-") {
		t.Errorf("sidA = %q, want prefix %s-", sidA, projectSessionPrefix(projA))
	}
	if !strings.HasPrefix(sidB, projectSessionPrefix(projB)+"-") {
		t.Errorf("sidB = %q, want prefix %s-", sidB, projectSessionPrefix(projB))
	}

	// 2. --continue in Project A resumes sidA (not sidB, even though sidB ran later!)
	// and implies --session-db when sessionDB is false.
	sidACont, resumedACont, trackerACont, evA2 := runTurn(projA, sessionOpts{
		sessionDBPath:   dbPath,
		continueSession: true,
	}, "second turn in project A")
	if !resumedACont || sidACont != sidA {
		t.Fatalf("Project A --continue got (sid=%q, resumed=%v), want (%q, true)", sidACont, resumedACont, sidA)
	}
	if evA2 <= evA1 {
		t.Errorf("Project A event count after continue = %d, want > %d (conversation history preserved)", evA2, evA1)
	}
	if trackerACont.Totals().Turns < 2 {
		t.Errorf("Project A tracker turns after continue = %d, want >= 2 (prior turn restored + new turn)", trackerACont.Totals().Turns)
	}

	// 3. --continue in Project B resumes sidB and leaves Project B's event count independent of Project A.
	sidBCont, resumedBCont, _, evB2 := runTurn(projB, sessionOpts{
		sessionDBPath:   dbPath,
		continueSession: true,
	}, "second turn in project B")
	if !resumedBCont || sidBCont != sidB {
		t.Fatalf("Project B --continue got (sid=%q, resumed=%v), want (%q, true)", sidBCont, resumedBCont, sidB)
	}
	if evB2 <= evB1 {
		t.Errorf("Project B event count after continue = %d, want > %d", evB2, evB1)
	}
}

func TestResumeSessionByIDAndInteractivePicker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "picker.db")
	proj := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderEcho

	createSession := func(prompt string) string {
		t.Helper()
		tracker := usage.NewTracker()
		handle, sid, _, opts, err := openCLISessionAndEventlog(
			ctx, cfg, proj, sessionOpts{sessionDB: true, sessionDBPath: dbPath},
			attachOpts{}, prompt, "", tracker, "echo", usage.Pricing{}, nil, nil, func(string) {},
		)
		if err != nil {
			t.Fatalf("openCLISessionAndEventlog: %v", err)
		}
		defer func() { _ = handle.Close() }()
		ag, err := agent.New(mustEchoModel(t), opts...)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		for _, err := range ag.Run(ctx, prompt) {
			if err != nil {
				t.Fatalf("ag.Run: %v", err)
			}
		}
		return sid
	}

	sid1 := createSession("first session: investigate auth bug")
	time.Sleep(15 * time.Millisecond)
	sid2 := createSession("second session: refactor handler")

	handle, err := openEventlogIfEnabled(ctx, cfg, sessionOpts{sessionDB: true, sessionDBPath: dbPath}, attachOpts{}, "x", "", nil, func(string) {})
	if err != nil {
		t.Fatalf("openEventlogIfEnabled: %v", err)
	}
	defer func() { _ = handle.Close() }()

	// Direct --resume <id> selects sid1 even though sid2 is newer.
	gotID, resumed, err := resolveCLISessionID(ctx, handle.Service, proj, false, sid1, nil, nil, false)
	if err != nil || !resumed || gotID != sid1 {
		t.Fatalf("direct --resume sid1 = (%q, %v, %v), want (%q, true, nil)", gotID, resumed, err, sid1)
	}

	// Unknown session ID errors clearly.
	if _, _, err := resolveCLISessionID(ctx, handle.Service, proj, false, "proj-missing-id", nil, nil, false); err == nil {
		t.Error("expected error for unknown --resume session ID, got nil")
	}

	// Interactive picker with "2\n" selects the older session (sid1) and renders previews.
	var pickerOut bytes.Buffer
	gotID, resumed, err = resolveCLISessionID(ctx, handle.Service, proj, false, resumePickSentinel, strings.NewReader("2\n"), &pickerOut, true)
	if err != nil || !resumed || gotID != sid1 {
		t.Fatalf("interactive picker '2' = (%q, %v, %v), want (%q, true, nil)", gotID, resumed, err, sid1)
	}
	if !strings.Contains(pickerOut.String(), "first session: investigate auth bug") || !strings.Contains(pickerOut.String(), "second session: refactor handler") {
		t.Errorf("picker output missing session previews:\n%s", pickerOut.String())
	}

	// Interactive picker with "\n" defaults to 1 (newest session sid2).
	pickerOut.Reset()
	gotID, resumed, err = resolveCLISessionID(ctx, handle.Service, proj, false, resumePickSentinel, strings.NewReader("\n"), &pickerOut, true)
	if err != nil || !resumed || gotID != sid2 {
		t.Fatalf("interactive picker Enter = (%q, %v, %v), want (%q, true, nil)", gotID, resumed, err, sid2)
	}

	// Non-TTY bare --resume errors clearly.
	if _, _, err := resolveCLISessionID(ctx, handle.Service, proj, false, resumePickSentinel, strings.NewReader("1\n"), &pickerOut, false); err == nil {
		t.Error("expected error for bare --resume on non-TTY")
	}

	// Combining --continue and --resume errors clearly.
	if _, _, err := resolveCLISessionID(ctx, handle.Service, proj, true, sid1, nil, nil, false); err == nil {
		t.Error("expected error when combining --continue and --resume")
	}

	// Resuming a derived subagent session ID errors clearly.
	if _, _, err := resolveCLISessionID(ctx, handle.Service, proj, false, sid1+":sub:worker:1", nil, nil, false); err == nil {
		t.Error("expected error when passing derived subagent session ID to --resume")
	}

	// In-memory mode (sessionDB off) and attach mode without --continue/--resume preserve "default".
	memHandle, memSID, memResumed, _, err := openCLISessionAndEventlog(
		ctx, cfg, proj, sessionOpts{}, attachOpts{}, "x", "", usage.NewTracker(), "echo", usage.Pricing{}, nil, nil, func(string) {},
	)
	if err != nil || memHandle != nil || memSID != defaultCLISessionID || memResumed {
		t.Errorf("in-memory openCLISessionAndEventlog = (%v, %q, %v, %v), want (nil, %q, false, nil)", memHandle, memSID, memResumed, err, defaultCLISessionID)
	}
	attHandle, attSID, attResumed, _, err := openCLISessionAndEventlog(
		ctx, cfg, proj, sessionOpts{sessionDB: true, sessionDBPath: dbPath}, attachOpts{Listen: "127.0.0.1:0"}, "x", "", usage.NewTracker(), "echo", usage.Pricing{}, nil, nil, func(string) {},
	)
	if err != nil {
		t.Fatalf("attach openCLISessionAndEventlog: %v", err)
	}
	_ = attHandle.Close()
	if attSID != defaultCLISessionID || attResumed {
		t.Errorf("attach openCLISessionAndEventlog = (%q, %v), want (%q, false)", attSID, attResumed, defaultCLISessionID)
	}
}
