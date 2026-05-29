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

package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// PeerClient is a thin wrapper around http.Client for talking to a
// hub agent's peer-registration endpoints. Used by peer binaries
// (typically wired via cmd/core-agent's --attach-register-to flag).
//
// Single-use lifecycle: construct, RegisterAndHeartbeat, defer
// Deregister. The goroutine spawned by RegisterAndHeartbeat handles
// the heartbeat cadence; the caller doesn't have to track it.
type PeerClient struct {
	hubURL     string
	bearer     string
	httpClient *http.Client
	// heartbeatFraction is what fraction of TTL we heartbeat at.
	// Default 1/3 (heartbeat at 20s for a 60s TTL) — three retries
	// before the lease expires under steady network conditions.
	heartbeatFraction int
}

// PeerClientOption configures NewPeerClient.
type PeerClientOption func(*PeerClient)

// WithPeerBearerToken sets the Authorization: Bearer header used on
// every request to the hub. Required when the hub has
// Auth.BearerToken set.
func WithPeerBearerToken(token string) PeerClientOption {
	return func(c *PeerClient) { c.bearer = token }
}

// WithPeerHTTPClient overrides the default http.Client. Useful for
// mTLS (caller supplies a Transport with a configured TLSConfig) or
// for tests with a custom round-tripper.
func WithPeerHTTPClient(client *http.Client) PeerClientOption {
	return func(c *PeerClient) { c.httpClient = client }
}

// NewPeerClient builds a client targeting hubURL (e.g.
// "https://hub.default.svc:7777").
func NewPeerClient(hubURL string, opts ...PeerClientOption) *PeerClient {
	c := &PeerClient{
		hubURL:            hubURL,
		httpClient:        http.DefaultClient,
		heartbeatFraction: 3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Register POSTs the registration request to the hub. Returns the
// assigned RegistrationID + lease expiry. Doesn't start a heartbeat
// goroutine — use RegisterAndHeartbeat for the standard lifecycle.
func (c *PeerClient) Register(ctx context.Context, req RegisterRequest) (*Peer, error) {
	var p Peer
	if err := c.do(ctx, http.MethodPost, "/peers", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Heartbeat extends the lease on the named registration.
func (c *PeerClient) Heartbeat(ctx context.Context, registrationID string) (*Peer, error) {
	var p Peer
	if err := c.do(ctx, http.MethodPost, "/peers/"+registrationID+"/heartbeat", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Deregister removes the registration from the hub. Idempotent on
// the server side; this client also treats network errors as
// best-effort (graceful shutdown shouldn't fail because the hub is
// momentarily unreachable).
func (c *PeerClient) Deregister(ctx context.Context, registrationID string) error {
	return c.do(ctx, http.MethodDelete, "/peers/"+registrationID, nil, nil)
}

// RegisterAndHeartbeat registers the peer and starts a background
// heartbeat goroutine that runs until ctx is cancelled. Returns a
// cancel function that triggers Deregister + goroutine shutdown.
//
// Heartbeat cadence is the registered TTL divided by c.heartbeatFraction
// (default 1/3 of TTL). If the registered TTL is 0 (peer wanted
// default), the hub-supplied lease expiry is used to derive cadence.
//
// Heartbeat failures are logged but don't fatal — a peer that loses
// its registration via expired lease just re-registers on the next
// attempt (the hub's name-based upsert makes this clean).
func (c *PeerClient) RegisterAndHeartbeat(ctx context.Context, req RegisterRequest) (cancel func(), err error) {
	p, err := c.Register(ctx, req)
	if err != nil {
		return nil, err
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go c.heartbeatLoop(hbCtx, p, req, done)
	return func() {
		hbCancel()
		// Best-effort deregister on a short fresh ctx so cancellation
		// in the parent ctx doesn't block shutdown.
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer dcancel()
		_ = c.Deregister(dctx, p.RegistrationID)
		<-done
	}, nil
}

func (c *PeerClient) heartbeatLoop(ctx context.Context, initial *Peer, req RegisterRequest, done chan<- struct{}) {
	defer close(done)
	current := initial
	for {
		// Cadence is (lease_remaining / heartbeatFraction). Clamp at
		// least 1s to avoid pathological tight loops.
		remaining := time.Until(current.LeaseExpiresAt)
		cadence := remaining / time.Duration(c.heartbeatFraction)
		if cadence < time.Second {
			cadence = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cadence):
		}
		next, err := c.Heartbeat(ctx, current.RegistrationID)
		if err != nil {
			// On any error, try a re-register so the peer recovers
			// from an expired lease / hub restart / network blip.
			log.Printf("attach: peer heartbeat for %s failed (%v); re-registering",
				current.RegistrationID, err)
			fresh, rerr := c.Register(ctx, req)
			if rerr != nil {
				log.Printf("attach: peer re-register also failed: %v; will retry on next tick", rerr)
				current.LeaseExpiresAt = time.Now().Add(DefaultHeartbeatTTL)
				continue
			}
			current = fresh
			continue
		}
		current = next
	}
}

// do is the shared request helper. POST/DELETE/etc. with optional
// JSON body + JSON response. Returns the underlying error or an HTTP
// status error.
func (c *PeerClient) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("attach: peer client: marshal body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.hubURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("attach: peer client: build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("attach: peer client: do %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("attach: peer client: %s %s: status %d: %s",
			method, path, resp.StatusCode, respBody)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("attach: peer client: decode response: %w", err)
		}
	}
	return nil
}
