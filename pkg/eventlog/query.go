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

package eventlog

import (
	"strings"

	"google.golang.org/adk/v2/session"
)

// subSessionSep is the separator between a root session ID and the
// derived IDs of its subagent sessions ("<root>:sub:<branch>:<inv>").
// The Stream contract defines a root session tree in terms of it.
const subSessionSep = ":sub:"

// RootOf returns the root of a session ID's tree: the part before its
// first ":sub:". An ID with no ":sub:" is its own root, so derived IDs
// such as "<sid>:digest" are their own trees. This is the rule the
// Stream doc states for Seq; Transcript backends key their per-tree
// sequences and subscriptions on it.
func RootOf(sessionID string) string {
	if i := strings.Index(sessionID, subSessionSep); i >= 0 {
		return sessionID[:i]
	}
	return sessionID
}

// inTree reports whether sessionID is parent itself or one of its
// ":sub:" descendants — the same membership WithSessionTree's SQL
// clause (session_id = ? OR session_id LIKE '<parent>:sub:%') tests.
func inTree(parent, sessionID string) bool {
	return sessionID == parent || strings.HasPrefix(sessionID, parent+subSessionSep)
}

// Query is the resolved form of a QueryOption list: what a Since or
// Watch call asked for, readable by a Transcript backend outside this
// package. The SQL backend translates the same options into WHERE
// clauses (applyQueryFilters); Matches is the in-memory equivalent and
// the two must agree on every filter the conformance suite exercises.
// One edge is left to the dialect: Matches compares bytes, while SQL
// LIKE is case-insensitive for ASCII on SQLite and collation-dependent
// on MySQL, so a branch or author that differs only in case matches
// there and not here. IDs, authors and branches are machine-minted,
// so nothing in the tree relies on either reading.
type Query struct {
	// AppName, UserID and SessionID are ForSession's triple; an empty
	// field is a wildcard.
	AppName, UserID, SessionID string
	// TreeAppName, TreeUserID and TreeParentID are WithSessionTree's
	// arguments. When TreeParentID is set it wins over the ForSession
	// triple, as it does in SQL.
	TreeAppName, TreeUserID, TreeParentID string
	// BranchPrefixes is WithBranchPrefix / WithAnyBranchPrefix: a
	// branch matches when it equals a prefix or begins with it followed
	// by "." or "/". Empty means every branch.
	BranchPrefixes []string
	// Author is WithAuthor (exact); AuthorSuffix is WithAuthorSuffix.
	Author, AuthorSuffix string
	// Limit is WithLimit; 0 means unbounded. Since applies it to the
	// rows it yields; Watch applies it per catch-up pass and runs
	// another pass at once when the cursor moved, as the SQL poller
	// does, so a limit bounds a pass and not the tail.
	Limit int
}

// ResolveQuery applies opts and returns the resulting Query.
func ResolveQuery(opts ...QueryOption) Query {
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	return Query{
		AppName: q.appName, UserID: q.userID, SessionID: q.sessionID,
		TreeAppName: q.treeAppName, TreeUserID: q.treeUserID, TreeParentID: q.treeParentID,
		BranchPrefixes: q.branchPrefixes,
		Author:         q.author, AuthorSuffix: q.authorSuffix,
		Limit: q.limit,
	}
}

// Scope returns the one root session tree the query is confined to,
// and ok=false when the query is unscoped under the Stream contract
// (no ForSession / WithSessionTree, or an empty session ID). A backend
// that cannot enumerate its whole store answers !ok with an error
// wrapping ErrUnscopedQuery.
func (q Query) Scope() (appName, userID, root string, ok bool) {
	switch {
	case q.TreeParentID != "":
		return q.TreeAppName, q.TreeUserID, RootOf(q.TreeParentID), true
	case q.SessionID != "":
		return q.AppName, q.UserID, RootOf(q.SessionID), true
	}
	return "", "", "", false
}

// Matches reports whether an event appended under (appName, userID,
// sessionID) satisfies every filter in q except Limit. Branch and
// author come from ev; a nil ev matches only when neither filter is
// set.
func (q Query) Matches(appName, userID, sessionID string, ev *session.Event) bool {
	if q.TreeParentID != "" {
		if appName != q.TreeAppName || userID != q.TreeUserID || !inTree(q.TreeParentID, sessionID) {
			return false
		}
	} else {
		if q.AppName != "" && appName != q.AppName {
			return false
		}
		if q.UserID != "" && userID != q.UserID {
			return false
		}
		if q.SessionID != "" && sessionID != q.SessionID {
			return false
		}
	}
	var branch, author string
	if ev != nil {
		branch, author = ev.Branch, ev.Author
	}
	if len(q.BranchPrefixes) > 0 && !branchMatchesAny(branch, q.BranchPrefixes) {
		return false
	}
	if q.Author != "" && author != q.Author {
		return false
	}
	if q.AuthorSuffix != "" && !strings.HasSuffix(author, q.AuthorSuffix) {
		return false
	}
	return true
}

// branchMatchesAny mirrors branchPrefixClause: exact match, or the
// prefix followed by ADK's "." separator or the "/" alternative.
func branchMatchesAny(branch string, prefixes []string) bool {
	for _, p := range prefixes {
		if branch == p || strings.HasPrefix(branch, p+".") || strings.HasPrefix(branch, p+"/") {
			return true
		}
	}
	return false
}
