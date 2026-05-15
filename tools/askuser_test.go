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

package tools

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewAskUserTool_RequiresPrompter(t *testing.T) {
	t.Parallel()
	_, err := NewAskUserTool(AskUserOptions{})
	if err == nil || !strings.Contains(err.Error(), "Prompter is required") {
		t.Fatalf("expected Prompter-required error, got %v", err)
	}
}

func TestNewAskUserTool_DefaultsNameAndDescription(t *testing.T) {
	t.Parallel()
	tl, err := NewAskUserTool(AskUserOptions{Prompter: StaticPrompter("ok")})
	if err != nil {
		t.Fatalf("NewAskUserTool: %v", err)
	}
	if tl.Name() != "ask_user" {
		t.Errorf("default name = %q, want ask_user", tl.Name())
	}
	if tl.Description() == "" {
		t.Errorf("default description should be non-empty")
	}
}

func TestNewAskUserTool_NameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	tl, err := NewAskUserTool(AskUserOptions{
		Prompter:    StaticPrompter("ok"),
		Name:        "ask_human",
		Description: "ask the human",
	})
	if err != nil {
		t.Fatalf("NewAskUserTool: %v", err)
	}
	if tl.Name() != "ask_human" {
		t.Errorf("name override didn't take, got %q", tl.Name())
	}
	if tl.Description() != "ask the human" {
		t.Errorf("description override didn't take, got %q", tl.Description())
	}
}

func TestStdinPrompter_ReadsLine(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("yes please\n")
	var out bytes.Buffer
	p := StdinPrompter(in, &out)

	got, err := p.Prompt(context.Background(), "shall I proceed?")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got != "yes please" {
		t.Errorf("answer = %q, want %q", got, "yes please")
	}
	if !strings.Contains(out.String(), "shall I proceed?") {
		t.Errorf("question should be echoed to out, got %q", out.String())
	}
}

func TestStdinPrompter_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	p := StdinPrompter(strings.NewReader("  spaced answer  \n"), &bytes.Buffer{})
	got, err := p.Prompt(context.Background(), "q")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got != "spaced answer" {
		t.Errorf("got %q, want %q", got, "spaced answer")
	}
}

func TestStdinPrompter_RespectsCancelledContext(t *testing.T) {
	t.Parallel()
	p := StdinPrompter(strings.NewReader("x\n"), &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling

	_, err := p.Prompt(ctx, "q")
	if err == nil {
		t.Errorf("expected ctx-cancelled error before reading stdin")
	}
}

func TestStdinPrompter_EOFErrors(t *testing.T) {
	t.Parallel()
	p := StdinPrompter(strings.NewReader(""), &bytes.Buffer{})
	_, err := p.Prompt(context.Background(), "q")
	if err == nil {
		t.Errorf("expected EOF error for empty stdin")
	}
}

func TestRefusePrompter_AlwaysErrors(t *testing.T) {
	t.Parallel()
	p := RefusePrompter("running headless")
	_, err := p.Prompt(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "running headless") {
		t.Errorf("expected refuse error, got %v", err)
	}
}

func TestRefusePrompter_DefaultReason(t *testing.T) {
	t.Parallel()
	p := RefusePrompter("")
	_, err := p.Prompt(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "no interactive user") {
		t.Errorf("expected default reason, got %v", err)
	}
}

func TestStaticPrompter_ReturnsCannedAnswer(t *testing.T) {
	t.Parallel()
	p := StaticPrompter("42")
	got, err := p.Prompt(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestPrompterFunc_ImplementsPrompter(t *testing.T) {
	t.Parallel()
	var p Prompter = PrompterFunc(func(_ context.Context, q string) (string, error) {
		return "echo: " + q, nil
	})
	got, err := p.Prompt(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got != "echo: hello" {
		t.Errorf("got %q", got)
	}
}

func TestPrompterFunc_PropagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	p := PrompterFunc(func(_ context.Context, _ string) (string, error) {
		return "", want
	})
	_, err := p.Prompt(context.Background(), "q")
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}
