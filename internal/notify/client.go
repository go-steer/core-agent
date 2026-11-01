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

// Package notify implements the switchboard /v1/messages HTTP client used by
// monitor.notify to post, replace, and thread-append monitoring digests.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultMaxChunkRunes = 3500
	defaultMaxRetries    = 3
	defaultBaseBackoff   = 200 * time.Millisecond
	defaultHTTPTimeout   = 15 * time.Second
	maxErrorBodyBytes    = 4096
)

// Sentinel errors returned by Client methods so callers (such as monitor.notify
// replace mode) can branch on upstream capabilities without inspecting raw
// status codes.
var (
	// ErrSendFullText is returned by Replace when text exceeds MaxChunkRunes;
	// switchboard PUT replaces a single message and cannot split across chunks,
	// so the caller must fall back to posting a fresh message.
	ErrSendFullText = errors.New("notify: message exceeds single-message edit limit")

	// ErrEditUnsupported is returned by Replace when the gateway responds 501
	// Not Implemented (platform does not support message editing).
	ErrEditUnsupported = errors.New("notify: message edit unsupported by gateway (501)")

	// ErrNoSuchMessage is returned by Replace when the gateway responds 404 Not
	// Found (the referenced message was deleted).
	ErrNoSuchMessage = errors.New("notify: referenced message not found (404)")

	// ErrDenied is returned when the gateway rejects the request with 401 or 403.
	ErrDenied = errors.New("notify: request denied by gateway")
)

// Error wraps an HTTP status failure from the switchboard gateway.
type Error struct {
	Status int
	Body   string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%v (status %d: %s)", e.Err, e.Status, e.Body)
	}
	return fmt.Sprintf("notify: gateway returned status %d: %s", e.Status, e.Body)
}

func (e *Error) Unwrap() error { return e.Err }

// Config configures a switchboard /v1/messages Client.
type Config struct {
	// Endpoint is the base URL of the switchboard gateway (e.g.
	// "http://switchboard.Namespace.svc:8080").
	Endpoint string
	// Channel is the default conversation key passed to Post when no thread
	// parent is specified.
	Channel string
	// Token is the bearer token sent in Authorization: Bearer <token>.
	Token string
	// HTTPClient is the underlying HTTP client; defaults to a 15s timeout client.
	HTTPClient *http.Client
	// MaxChunkRunes caps the rune length of each POSTed message chunk. Defaults
	// to 3500.
	MaxChunkRunes int
	// MaxRetries is the number of retry attempts on HTTP 429 or 5xx responses
	// (excluding 501). Defaults to 3.
	MaxRetries int
	// BaseBackoff is the initial backoff before the first retry (doubled each
	// attempt). Defaults to 200ms.
	BaseBackoff time.Duration
	// Sleep is an optional hook for tests to intercept backoff sleeps.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Ref identifies a posted message on the switchboard gateway so a later
// cycle can Replace it in place or Append to its thread.
type Ref struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

// Client is a switchboard /v1/messages client.
type Client struct {
	endpoint      string
	channel       string
	token         string
	httpClient    *http.Client
	maxChunkRunes int
	maxRetries    int
	baseBackoff   time.Duration
	sleep         func(ctx context.Context, d time.Duration) error
}

// New validates cfg and returns a ready Client.
func New(cfg Config) (*Client, error) {
	ep := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if ep == "" {
		return nil, errors.New("notify: endpoint is required")
	}
	if strings.TrimSpace(cfg.Channel) == "" {
		return nil, errors.New("notify: channel is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	maxRunes := cfg.MaxChunkRunes
	if maxRunes <= 0 {
		maxRunes = defaultMaxChunkRunes
	}
	maxRetries := cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	} else if maxRetries == 0 {
		maxRetries = defaultMaxRetries
	}
	backoff := cfg.BaseBackoff
	if backoff <= 0 {
		backoff = defaultBaseBackoff
	}
	sleepFn := cfg.Sleep
	if sleepFn == nil {
		sleepFn = defaultSleep
	}
	return &Client{
		endpoint:      ep,
		channel:       cfg.Channel,
		token:         cfg.Token,
		httpClient:    hc,
		maxChunkRunes: maxRunes,
		maxRetries:    maxRetries,
		baseBackoff:   backoff,
		sleep:         sleepFn,
	}, nil
}

// Post sends text to the configured channel via POST /v1/messages. If text
// exceeds MaxChunkRunes, the first chunk is posted to c.channel and remaining
// chunks are appended to the returned message's thread (or c.channel if the
// gateway returned no message ID).
//
// When sessionID is non-empty, Post sets the X-Agent-Session header on the
// initial POST so switchboard can bind replies in the thread back to that
// session.
func (c *Client) Post(ctx context.Context, text, sessionID string) (Ref, error) {
	return c.postToConversation(ctx, c.channel, text, sessionID)
}

// Append posts text into an existing message's thread ("<channel>:<id>") via
// POST /v1/messages. Multi-chunk messages are posted sequentially to the same
// thread conversation key.
func (c *Client) Append(ctx context.Context, parent Ref, text string) (Ref, error) {
	ch := parent.Channel
	if ch == "" {
		ch = c.channel
	}
	conv := ch
	if parent.ID != "" {
		conv = ch + ":" + parent.ID
	}
	return c.postToConversation(ctx, conv, text, "")
}

// Replace edits an existing message in place via
// PUT /v1/messages/{channel}/{id}.
//
// Returns ErrSendFullText without making an HTTP call when text exceeds
// MaxChunkRunes, ErrEditUnsupported on HTTP 501, and ErrNoSuchMessage on
// HTTP 404.
func (c *Client) Replace(ctx context.Context, ref Ref, text string) (Ref, error) {
	if ref.ID == "" {
		return Ref{}, fmt.Errorf("%w: empty message id", ErrNoSuchMessage)
	}
	if utf8.RuneCountInString(text) > c.maxChunkRunes {
		return Ref{}, ErrSendFullText
	}
	ch := ref.Channel
	if ch == "" {
		ch = c.channel
	}
	u := fmt.Sprintf("%s/v1/messages/%s/%s", c.endpoint, url.PathEscape(ch), url.PathEscape(ref.ID))
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return Ref{}, fmt.Errorf("notify: marshal replace body: %w", err)
	}
	respBody, err := c.doJSON(ctx, http.MethodPut, u, body, "")
	if err != nil {
		return Ref{}, err
	}
	out := Ref{Channel: ch, ID: ref.ID}
	if len(respBody) > 0 {
		var parsed struct {
			Channel string `json:"channel"`
			ID      string `json:"id"`
		}
		if json.Unmarshal(respBody, &parsed) == nil {
			if parsed.Channel != "" {
				out.Channel = parsed.Channel
			}
			if parsed.ID != "" {
				out.ID = parsed.ID
			}
		}
	}
	return out, nil
}

func (c *Client) postToConversation(ctx context.Context, conversation, text, sessionID string) (Ref, error) {
	chunks := chunkMessage(text, c.maxChunkRunes)
	u := c.endpoint + "/v1/messages"

	var rootRef Ref
	for i, chunk := range chunks {
		conv := conversation
		hdrSession := ""
		if i == 0 {
			hdrSession = sessionID
		} else if rootRef.ID != "" && !strings.Contains(conversation, ":") {
			conv = rootRef.Channel + ":" + rootRef.ID
		}
		payload, err := json.Marshal(map[string]string{
			"conversation": conv,
			"text":         chunk,
		})
		if err != nil {
			return Ref{}, fmt.Errorf("notify: marshal post body: %w", err)
		}
		respBody, err := c.doJSON(ctx, http.MethodPost, u, payload, hdrSession)
		if err != nil {
			return rootRef, err
		}
		if i == 0 {
			rootRef = parseRef(respBody, c.channel)
		}
	}
	return rootRef, nil
}

func parseRef(respBody []byte, fallbackChannel string) Ref {
	ref := Ref{Channel: fallbackChannel}
	if len(respBody) == 0 {
		return ref
	}
	var parsed struct {
		Channel string `json:"channel"`
		ID      string `json:"id"`
	}
	if json.Unmarshal(respBody, &parsed) == nil {
		if parsed.Channel != "" {
			ref.Channel = parsed.Channel
		}
		if parsed.ID != "" {
			ref.ID = parsed.ID
		}
	}
	return ref
}

func (c *Client) doJSON(ctx context.Context, method, rawURL string, body []byte, sessionID string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.baseBackoff * time.Duration(1<<(attempt-1))
			if err := c.sleep(ctx, backoff); err != nil {
				return nil, err
			}
		}
		respBody, retry, err := c.doAttempt(ctx, method, rawURL, body, sessionID)
		if !retry {
			return respBody, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) doAttempt(ctx context.Context, method, rawURL string, body []byte, sessionID string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if sessionID != "" {
		req.Header.Set("X-Agent-Session", sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("notify: http %s: %w", method, err)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("notify: read response: %w", readErr)
	}

	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		return respBody, false, nil
	case status == http.StatusNotImplemented:
		return nil, false, &Error{Status: status, Body: strings.TrimSpace(string(respBody)), Err: ErrEditUnsupported}
	case status == http.StatusNotFound && method == http.MethodPut:
		return nil, false, &Error{Status: status, Body: strings.TrimSpace(string(respBody)), Err: ErrNoSuchMessage}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, false, &Error{Status: status, Body: strings.TrimSpace(string(respBody)), Err: ErrDenied}
	case status == http.StatusTooManyRequests || status >= 500:
		return nil, true, &Error{Status: status, Body: strings.TrimSpace(string(respBody))}
	default:
		return nil, false, &Error{Status: status, Body: strings.TrimSpace(string(respBody))}
	}
}

func chunkMessage(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = defaultMaxChunkRunes
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	remaining := text
	for utf8.RuneCountInString(remaining) > maxRunes {
		cutByte := byteIndexAtRune(remaining, maxRunes)
		window := remaining[:cutByte]
		if nl := strings.LastIndexByte(window, '\n'); nl > 0 {
			chunks = append(chunks, strings.TrimRight(remaining[:nl], "\r"))
			remaining = remaining[nl+1:]
			continue
		}
		chunks = append(chunks, window)
		remaining = remaining[cutByte:]
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

func byteIndexAtRune(s string, runeCount int) int {
	n := 0
	for idx := range s {
		if n == runeCount {
			return idx
		}
		n++
	}
	return len(s)
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
