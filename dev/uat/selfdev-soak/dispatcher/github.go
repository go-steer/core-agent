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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenSource yields the bearer the GitHub client sends. The App's
// installation token in production; a constant in tests.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ghClient is the dispatcher's whole GitHub surface: the REST calls the
// A7 steps need against one repository, nothing else. Hand-rolled rather
// than a client library because it is a dozen endpoints and the module
// has no GitHub dependency to reuse.
type ghClient struct {
	base   string // API root, e.g. https://api.github.com
	owner  string
	repo   string
	http   *http.Client
	tokens tokenSource
}

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        ghUser    `json:"user"`
	Labels      []ghLabel `json:"labels"`
	Assignees   []ghUser  `json:"assignees"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
	ViaApp      *ghApp    `json:"performed_via_github_app,omitempty"`
}

type ghApp struct {
	Slug string `json:"slug"`
}

func (i ghIssue) hasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, name) { // GitHub label names are case-insensitive
			return true
		}
	}
	return false
}

// ghEvent is one row of GET /repos/{o}/{r}/issues/{n}/events: who did
// what to the issue. Label and Assignee are set only for the event kinds
// that carry them.
type ghEvent struct {
	Event    string   `json:"event"`
	Actor    *ghUser  `json:"actor"`
	Label    *ghLabel `json:"label,omitempty"`
	Assignee *ghUser  `json:"assignee,omitempty"`
	Assigner *ghUser  `json:"assigner,omitempty"`
	// ViaApp is set when a GitHub App performed the event on a person's
	// behalf. Such an event is not words the maintainer wrote (decision 6).
	ViaApp *ghApp `json:"performed_via_github_app,omitempty"`
}

func (e ghEvent) actor() string {
	if e.Actor == nil {
		return ""
	}
	return e.Actor.Login
}

// assigner is who performed an assignment. GitHub's issue events carry
// `assigner` alongside `actor`, and older payloads documented `actor` on
// an assigned event as the assignee, not the person assigning; reading
// `assigner` when present is right under either reading. The provenance
// check also requires `actor` to be the maintainer, so the two can never
// disagree in an issue's favour.
func (e ghEvent) assigner() string {
	if e.Assigner != nil {
		return e.Assigner.Login
	}
	return e.actor()
}

type ghPull struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	HTMLURL  string     `json:"html_url"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// ghError is a non-2xx answer. The body is GitHub's JSON error, which
// never echoes the Authorization header, so it is safe to log.
type ghError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *ghError) Error() string {
	return fmt.Sprintf("github %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

func (c *ghClient) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repo) + suffix
}

// do sends one request and decodes a JSON answer into out (nil to
// discard). path is relative to the API root and may carry a query.
func (c *ghClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader) // #nosec G704 -- base is the operator's --api-url; path is built here.
	if err != nil {
		return err
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) // #nosec G704 -- see above.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ghError{Method: method, Path: strings.SplitN(path, "?", 2)[0], Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pageSize is GitHub's maximum per_page. A short page ends the walk.
const pageSize = 100

// maxPages bounds every paginated walk. 30 pages of 100 is far beyond
// anything the mirror holds; the bound exists so a misbehaving server
// cannot spin the dispatcher forever.
const maxPages = 30

// paginate walks pages of path (which must already carry a query) and
// returns every row, or an error rather than a truncated list.
func paginate[T any](ctx context.Context, c *ghClient, path string) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		var rows []T
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s&per_page=%d&page=%d", path, pageSize, page), nil, &rows); err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < pageSize {
			return all, nil
		}
	}
	// Truncating would be a silent wrong answer: a provenance check that
	// never saw page 31 could pass an issue someone else labeled there.
	return nil, fmt.Errorf("%s: more than %d pages", strings.SplitN(path, "?", 2)[0], maxPages)
}

// openIssues lists open issues carrying label, oldest first. Pull
// requests, which the issues endpoint also returns, are dropped.
func (c *ghClient) openIssues(ctx context.Context, label string) ([]ghIssue, error) {
	rows, err := paginate[ghIssue](ctx, c, c.repoPath("/issues?state=open&sort=created&direction=asc&labels="+url.QueryEscape(label)))
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if r.PullRequest == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (c *ghClient) issueEvents(ctx context.Context, number int) ([]ghEvent, error) {
	return paginate[ghEvent](ctx, c, c.repoPath(fmt.Sprintf("/issues/%d/events?", number)))
}

func (c *ghClient) openPulls(ctx context.Context) ([]ghPull, error) {
	return paginate[ghPull](ctx, c, c.repoPath("/pulls?state=open"))
}

// maxContentEdits is how many body edits one GraphQL page returns. An
// issue with more is refused rather than paged: no seed is edited that
// often, and a partial list is not an answer.
const maxContentEdits = 100

// contentEditors returns who edited the issue's body, from GraphQL's
// userContentEdits — the REST events API has no record of body edits.
// An edit whose editor GitHub can't name (a deleted account) is "". total
// is GitHub's count, which may exceed len(editors).
func (c *ghClient) contentEditors(ctx context.Context, number int) (editors []string, total int, err error) {
	const query = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){userContentEdits(first:100){totalCount nodes{editor{login}}}}}}`
	var out struct {
		Data struct {
			Repository *struct {
				Issue *struct {
					UserContentEdits struct {
						TotalCount int `json:"totalCount"`
						Nodes      []struct {
							Editor *ghUser `json:"editor"`
						} `json:"nodes"`
					} `json:"userContentEdits"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": map[string]any{"owner": c.owner, "name": c.repo, "number": number}}
	if err := c.do(ctx, http.MethodPost, "/graphql", body, &out); err != nil {
		return nil, 0, err
	}
	if len(out.Errors) > 0 {
		return nil, 0, fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}
	if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
		return nil, 0, fmt.Errorf("graphql: issue #%d not found", number)
	}
	edits := out.Data.Repository.Issue.UserContentEdits
	for _, n := range edits.Nodes {
		login := ""
		if n.Editor != nil {
			login = n.Editor.Login
		}
		editors = append(editors, login)
	}
	return editors, edits.TotalCount, nil
}

// openPullForHead returns the open PR from the mirror's own branch, or
// nil. A dispatcher that crashed between opening a PR and recording it
// finds the PR here instead of failing to create a duplicate.
func (c *ghClient) openPullForHead(ctx context.Context, branch string) (*ghPull, error) {
	rows, err := paginate[ghPull](ctx, c, c.repoPath("/pulls?state=open&head="+url.QueryEscape(c.owner+":"+branch)))
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Head.Ref == branch {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (c *ghClient) pull(ctx context.Context, number int) (ghPull, error) {
	var p ghPull
	err := c.do(ctx, http.MethodGet, c.repoPath(fmt.Sprintf("/pulls/%d", number)), nil, &p)
	return p, err
}

func (c *ghClient) addLabels(ctx context.Context, number int, labels ...string) error {
	return c.do(ctx, http.MethodPost, c.repoPath(fmt.Sprintf("/issues/%d/labels", number)), map[string][]string{"labels": labels}, nil)
}

// removeLabel drops label from the issue. A 404 means the label was not
// on the issue, which is the state the caller asked for.
func (c *ghClient) removeLabel(ctx context.Context, number int, label string) error {
	err := c.do(ctx, http.MethodDelete, c.repoPath(fmt.Sprintf("/issues/%d/labels/%s", number, url.PathEscape(label))), nil, nil)
	if ge, ok := err.(*ghError); ok && ge.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *ghClient) comment(ctx context.Context, number int, body string) error {
	return c.do(ctx, http.MethodPost, c.repoPath(fmt.Sprintf("/issues/%d/comments", number)), map[string]string{"body": body}, nil)
}

func (c *ghClient) createPull(ctx context.Context, title, head, base, body string) (ghPull, error) {
	var p ghPull
	err := c.do(ctx, http.MethodPost, c.repoPath("/pulls"), map[string]any{
		"title": title, "head": head, "base": base, "body": body, "maintainer_can_modify": false,
	}, &p)
	return p, err
}
