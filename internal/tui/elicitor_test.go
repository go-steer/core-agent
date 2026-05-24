// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSender records the messages it gets so the test can inspect what
// was Send-ed and reply on the elicit message's reply channel.
type fakeSender struct {
	got chan tea.Msg
}

func (f *fakeSender) Send(m tea.Msg) { f.got <- m }

func TestTUIElicitor_NoProgramAttached(t *testing.T) {
	t.Parallel()
	e := newTUIElicitor()
	_, err := e.Elicit(context.Background(), "x", &mcpsdk.ElicitRequest{})
	if err == nil {
		t.Fatalf("expected error when no program attached")
	}
}

func TestTUIElicitor_RoundTripsResult(t *testing.T) {
	t.Parallel()
	e := newTUIElicitor()
	fs := &fakeSender{got: make(chan tea.Msg, 1)}
	e.attach(fs)

	wantContent := map[string]any{"name": "Ada"}
	resCh := make(chan *mcpsdk.ElicitResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := e.Elicit(context.Background(), "svc", &mcpsdk.ElicitRequest{
			Params: &mcpsdk.ElicitParams{Message: "hi"},
		})
		resCh <- r
		errCh <- err
	}()

	// The bridge should Send an elicitReqMsg.
	var msg tea.Msg
	select {
	case msg = <-fs.got:
	case <-time.After(2 * time.Second):
		t.Fatalf("no message sent within 2s")
	}
	em, ok := msg.(elicitReqMsg)
	if !ok {
		t.Fatalf("got %T, want elicitReqMsg", msg)
	}
	if em.ServerName != "svc" {
		t.Errorf("ServerName = %q", em.ServerName)
	}
	em.Out <- &mcpsdk.ElicitResult{Action: "accept", Content: wantContent}

	select {
	case r := <-resCh:
		if r.Action != "accept" {
			t.Errorf("Action = %q", r.Action)
		}
		if r.Content["name"] != "Ada" {
			t.Errorf("Content = %v", r.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no reply within 2s")
	}
	if err := <-errCh; err != nil {
		t.Errorf("unexpected err: %v", err)
	}
}

func TestTUIElicitor_NilResultBecomesDecline(t *testing.T) {
	t.Parallel()
	e := newTUIElicitor()
	fs := &fakeSender{got: make(chan tea.Msg, 1)}
	e.attach(fs)

	resCh := make(chan *mcpsdk.ElicitResult, 1)
	go func() {
		r, _ := e.Elicit(context.Background(), "x", &mcpsdk.ElicitRequest{})
		resCh <- r
	}()
	em := (<-fs.got).(elicitReqMsg)
	em.Out <- nil

	select {
	case r := <-resCh:
		if r.Action != "decline" {
			t.Errorf("nil result should decline; got %q", r.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no reply within 2s")
	}
}

func TestTUIElicitor_ContextCanceled(t *testing.T) {
	t.Parallel()
	e := newTUIElicitor()
	fs := &fakeSender{got: make(chan tea.Msg, 1)}
	e.attach(fs)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Elicit(ctx, "x", &mcpsdk.ElicitRequest{})
		errCh <- err
	}()
	// Drain the Send so the goroutine is parked on the chan read.
	<-fs.got
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Errorf("expected ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no return within 2s")
	}
}
