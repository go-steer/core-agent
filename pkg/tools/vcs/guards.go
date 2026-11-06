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

package vcs

import (
	"github.com/go-steer/core-vcs/providers/git"
	"github.com/go-steer/core-vcs/providers/github"
	"github.com/go-steer/core-vcs/providers/gitlab"
	corevcs "github.com/go-steer/core-vcs/vcs"
)

// Compile-time guards verifying that go-steer/core-vcs exported provider and
// credential types satisfy corevcs.Provider and corevcs.Credential. Enforced by
// dev/tools/verify-core-vcs-pin (#1490).
var (
	_ corevcs.Provider   = (*github.Provider)(nil)
	_ corevcs.Provider   = (*gitlab.Provider)(nil)
	_ corevcs.Provider   = (*git.Provider)(nil)
	_ corevcs.Credential = corevcs.TokenFile("")
	_ corevcs.Credential = corevcs.CredentialFunc(nil)
)
