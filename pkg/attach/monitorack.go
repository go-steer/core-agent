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

package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/auth"
)

const monitorAckMaxBytes = 8 * 1024

// MonitorAckRequest is an operator acknowledgement of an active monitoring
// subject via POST /monitor-ack (#1406).
//
// There is deliberately no ack_by field on the wire. Caller identity is
// derived strictly from the authenticated request credential (and
// X-Asserted-Caller when presented by an allowlisted proxy). Any request body
// carrying "ack_by" is rejected with 400 rather than ignored.
type MonitorAckRequest struct {
	Subject  string `json:"subject"`
	Reason   string `json:"reason,omitempty"`
	Workload string `json:"workload,omitempty"`
	By       string `json:"-"`
	ProxyBy  string `json:"-"`
}

// MonitorAckResult reports the durable acknowledgement record and whether
// upstream forwarding succeeded (#1406).
type MonitorAckResult struct {
	Workload          string    `json:"workload"`
	Subject           string    `json:"subject"`
	By                string    `json:"by"`
	ProxyBy           string    `json:"proxy_by,omitempty"`
	At                time.Time `json:"at"`
	Forwarded         bool      `json:"forwarded"`
	PreviouslyAckedBy string    `json:"previously_acked_by,omitempty"`
	PreviouslyAckedAt string    `json:"previously_acked_at,omitempty"`
}

// ErrMonitorAckAnonymous is returned when an operator acknowledgement has no
// authenticated caller identity (#1406).
var ErrMonitorAckAnonymous = errors.New("monitor-ack: anonymous caller is not permitted")

// ErrMonitorAckBadRequest marks a client-side validation error in
// MonitorAckHandler (#1406).
var ErrMonitorAckBadRequest = errors.New("monitor-ack: bad request")

// MonitorAckHandler records and optionally forwards an operator
// acknowledgement. When nil on Options, POST /monitor-ack responds 404.
type MonitorAckHandler func(ctx context.Context, req MonitorAckRequest) (MonitorAckResult, error)

func (h *handlers) registerMonitorAck(mux *http.ServeMux) {
	mux.HandleFunc("POST /monitor-ack", h.handleMonitorAck)
}

func (h *handlers) handleMonitorAck(w http.ResponseWriter, r *http.Request) {
	if h.monitorAck == nil {
		http.NotFound(w, r)
		return
	}
	if h.rejectDraining(w) {
		return
	}
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(r.Body, monitorAckMaxBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(body) > monitorAckMaxBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body exceeds maximum size"})
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if _, hasAckBy := raw["ack_by"]; hasAckBy {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "ack_by is not accepted in the request body; caller identity is derived from the authenticated credential (use X-Asserted-Caller from an allowlisted proxy)",
		})
		return
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var payload struct {
		Subject  string `json:"subject"`
		Reason   string `json:"reason"`
		Workload string `json:"workload"`
	}
	if err := dec.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected trailing data in JSON body"})
		return
	}

	subject := strings.TrimSpace(payload.Subject)
	if subject == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subject is required"})
		return
	}

	caller, _ := auth.CallerFromContext(r.Context())
	by := strings.TrimSpace(caller.Identity)
	if by == "" {
		by = "anonymous"
	}
	proxyBy, _ := auth.ProxyByFromContext(r.Context())

	res, err := h.monitorAck(r.Context(), MonitorAckRequest{
		Subject:  subject,
		Reason:   strings.TrimSpace(payload.Reason),
		Workload: strings.TrimSpace(payload.Workload),
		By:       by,
		ProxyBy:  proxyBy,
	})
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, ErrMonitorAckAnonymous):
			status = http.StatusForbidden
		case errors.Is(err, ErrMonitorAckBadRequest):
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
