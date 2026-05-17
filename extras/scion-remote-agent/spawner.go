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

package scionremote

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"

	"github.com/go-steer/core-agent/agent"
)

// defaultTemplate is the Scion template name used when neither the
// SCION_DEFAULT_TEMPLATE env var nor a WithTemplate option supplies
// one. "default" matches Scion's bundled template name.
const defaultTemplate = "default"

// ErrNotInsideScion is returned by New when the required configuration
// (Hub endpoint, agent token, project ID) is missing — typical for a
// core-agent running outside any Scion environment. Caller should
// fall back to agent.RefuseRemoteAgentSpawner so the model gets a
// clean tool-result error rather than a panic.
var ErrNotInsideScion = errors.New("scion-remote-agent: not running inside a Scion environment (set SCION_HUB_ENDPOINT, SCION_AGENT_TOKEN, SCION_PROJECT_ID or pass options)")

// Spawner implements agent.RemoteAgentSpawner against a Scion Hub.
// One Spawner serves many Spawn calls; safe for concurrent use
// (the underlying hubclient.Client is goroutine-safe).
type Spawner struct {
	client     hubclient.Client
	svc        hubclient.AgentService
	projectID  string
	template   string
	classifier Classifier
}

// Option configures a Spawner at construction time.
type Option func(*spawnerCfg)

type spawnerCfg struct {
	token      string
	endpoint   string
	projectID  string
	template   string
	httpClient hubclient.Client       // pre-built; lets tests inject a mock
	agentSvc   hubclient.AgentService // direct test seam
	classifier Classifier
}

// WithAgentToken overrides the SCION_AGENT_TOKEN env var. Pass this
// when the spawner is constructed outside a Scion container (e.g.
// during local development) and credentials come from elsewhere.
func WithAgentToken(t string) Option { return func(c *spawnerCfg) { c.token = t } }

// WithHubEndpoint overrides the SCION_HUB_ENDPOINT env var.
func WithHubEndpoint(url string) Option { return func(c *spawnerCfg) { c.endpoint = url } }

// WithProjectID overrides the SCION_PROJECT_ID env var.
func WithProjectID(id string) Option { return func(c *spawnerCfg) { c.projectID = id } }

// WithTemplate overrides the SCION_DEFAULT_TEMPLATE env var.
// Determines the Scion template each spawned sibling is created
// from. The template's harness config selects which agent binary
// runs in the spawned container.
func WithTemplate(name string) Option { return func(c *spawnerCfg) { c.template = name } }

// WithClient lets tests (and advanced consumers) inject a pre-built
// hubclient.Client instead of having Spawner construct one from
// endpoint+token. When supplied, WithAgentToken and WithHubEndpoint
// are ignored.
func WithClient(client hubclient.Client) Option {
	return func(c *spawnerCfg) { c.httpClient = client }
}

// WithAgentService lets tests inject a fake AgentService directly
// without constructing a full hubclient.Client. The most common
// reason consumers reach for this is unit testing: implementing the
// (small) subset of hubclient.AgentService methods that Spawn uses
// is much easier than implementing the (large) Client interface.
//
// When supplied, the spawner uses svc directly and never asks
// hubclient.New for a Client — so WithAgentToken, WithHubEndpoint,
// and WithClient are all ignored. ProjectID still comes from
// WithProjectID or SCION_PROJECT_ID since we use it to set
// CreateAgentRequest.ProjectID.
func WithAgentService(svc hubclient.AgentService) Option {
	return func(c *spawnerCfg) { c.agentSvc = svc }
}

// WithClassifier overrides the default log-entry-to-event mapping.
// See classify.go for the bundled classifiers; default is
// PreferStructuredPayload which uses jsonPayload.kind when present
// and falls back to lifecycle-only.
func WithClassifier(cl Classifier) Option { return func(c *spawnerCfg) { c.classifier = cl } }

// New constructs a Spawner from options + env variables. Options
// take precedence over env; the env is the auto-detect fallback for
// the common case of running inside a Scion container.
//
// Returns ErrNotInsideScion when the resolved configuration is
// incomplete (no endpoint, no token, or no project). The caller is
// expected to handle this by falling back to
// agent.RefuseRemoteAgentSpawner.
func New(opts ...Option) (*Spawner, error) {
	cfg := spawnerCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	env := loadEnv()
	endpoint := firstNonEmpty(cfg.endpoint, env.Endpoint)
	token := firstNonEmpty(cfg.token, env.Token)
	project := firstNonEmpty(cfg.projectID, env.Project)
	template := firstNonEmpty(cfg.template, env.Template, defaultTemplate)
	classifier := cfg.classifier
	if classifier == nil {
		classifier = PreferStructuredPayload
	}

	// Three construction paths:
	//   1. WithAgentService — test seam; use the service directly,
	//      no Client involved.
	//   2. WithClient — pre-built Client (e.g. for advanced auth);
	//      we ask it for a ProjectAgents view.
	//   3. Env/options — construct the Client ourselves from the
	//      resolved endpoint + token, then ProjectAgents.
	if project == "" {
		return nil, ErrNotInsideScion
	}
	var svc hubclient.AgentService
	var client hubclient.Client
	switch {
	case cfg.agentSvc != nil:
		svc = cfg.agentSvc
	case cfg.httpClient != nil:
		client = cfg.httpClient
		svc = client.ProjectAgents(project)
	default:
		if endpoint == "" || token == "" {
			return nil, ErrNotInsideScion
		}
		c, err := hubclient.New(endpoint, hubclient.WithAgentToken(token))
		if err != nil {
			return nil, fmt.Errorf("scion-remote-agent: build hub client: %w", err)
		}
		client = c
		svc = client.ProjectAgents(project)
	}

	return &Spawner{
		client:     client,
		svc:        svc,
		projectID:  project,
		template:   template,
		classifier: classifier,
	}, nil
}

// Spawn launches a new sibling Scion agent via the Hub Create API
// and returns a handle the manager uses for status / stop / event
// fan-in. Implements agent.RemoteAgentSpawner.
func (s *Spawner) Spawn(ctx context.Context, spec agent.RemoteAgentSpec) (agent.RemoteAgentHandle, error) {
	if s == nil {
		return nil, errors.New("scion-remote-agent: nil Spawner")
	}
	req := &hubclient.CreateAgentRequest{
		Name:      spec.Name,
		ProjectID: s.projectID,
		Template:  s.template,
		Task:      spec.Goal,
		Notify:    true,
		Labels: map[string]string{
			"spawned-by":    "core-agent",
			"parent-source": spec.Name, // best-effort; consumer can override via Spec.Extras
		},
	}
	resp, err := s.svc.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("scion-remote-agent: hub create %q: %w", spec.Name, err)
	}
	if resp == nil || resp.Agent == nil {
		return nil, errors.New("scion-remote-agent: hub returned empty response")
	}

	h := newHandle(s.svc, resp.Agent.ID, s.classifier)
	// Background goroutine streams cloud logs and pushes events
	// onto the handle's Events() channel. Exits when the SSE
	// connection ends, ctx is cancelled, or Stop is called.
	go h.streamLogs(ctx)
	return h, nil
}

// firstNonEmpty returns the first non-empty string from its args
// or "" if all are empty. Helper for option-vs-env merging.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
