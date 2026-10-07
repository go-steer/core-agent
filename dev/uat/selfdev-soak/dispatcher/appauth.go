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

// The writer GitHub App's credentials (decisions 7 and 10).
//
// The App authenticates in two hops: a short-lived RS256 JWT signed with
// the App's private key proves "I am App N", and GitHub exchanges that
// JWT for an installation token scoped to the one repository the App is
// installed on. Only the installation token is ever sent to the REST API
// or to git; the JWT never leaves this file and the key never leaves the
// process.
//
// The JWT is ten lines of the standard library, so it is built here
// rather than through a JWT dependency the module does not otherwise
// need for this.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// maxKeyFileBytes bounds the private-key read. A 4096-bit RSA key in PEM
// is about 3.3 KiB.
const maxKeyFileBytes = 16 << 10

// insecureKeyBits are the mode bits that make a key file readable or
// writable by someone other than its owner and group. Same rule as
// childenv.ReadSecretFile applies to tokens: 0600/0640/0440 pass, and a
// Kubernetes secret volume needs defaultMode 0400 or 0440.
const insecureKeyBits = 0o027

// loadAppKey reads the App's PEM private key (PKCS#1 "RSA PRIVATE KEY",
// as GitHub issues it, or PKCS#8). The file is mode-checked like a token
// file; the content is never echoed in an error.
func loadAppKey(path string) (*rsa.PrivateKey, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names this file on purpose.
	if err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("app key %s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm&insecureKeyBits != 0 {
		return nil, fmt.Errorf("app key %s is accessible to other users (mode %04o); chmod 0600 it (for a Kubernetes secret volume set defaultMode: 0400)", path, perm)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, fmt.Errorf("app key %s is larger than %d bytes, so it is not a key", path, maxKeyFileBytes)
	}
	return parseAppKey(path, raw)
}

func parseAppKey(path string, raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("app key %s holds no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app key %s is neither a PKCS#1 nor a PKCS#8 private key", path)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app key %s is not an RSA key (GitHub Apps sign with RS256)", path)
	}
	return rk, nil
}

// appJWT builds the App-level JWT GitHub expects: RS256, issuer the App
// ID, issued 60s in the past to absorb clock skew, expiring well inside
// GitHub's 10-minute maximum.
func appJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// appTokenSource mints and caches installation tokens.
type appTokenSource struct {
	api            string // API root
	appID          int64
	key            *rsa.PrivateKey
	installationID int64 // 0: discover from owner/repo
	owner, repo    string
	http           *http.Client
	now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// tokenRefreshMargin is how long before expiry a cached token is
// replaced. Installation tokens last an hour; a push that starts with
// two minutes left must not fail halfway.
const tokenRefreshMargin = 5 * time.Minute

// Token returns a valid installation token, minting a new one when the
// cached one is missing or close to expiry.
func (s *appTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.token != "" && now.Add(tokenRefreshMargin).Before(s.expires) {
		return s.token, nil
	}
	jwt, err := appJWT(s.appID, s.key, now)
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	if s.installationID == 0 {
		id, err := s.discoverInstallation(ctx, jwt)
		if err != nil {
			return "", err
		}
		s.installationID = id
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", s.installationID)
	if err := s.call(ctx, http.MethodPost, path, jwt, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("installation token exchange returned no token")
	}
	s.token, s.expires = out.Token, out.ExpiresAt
	return s.token, nil
}

// discoverInstallation finds the App's installation on the mirror, so the
// operator configures the App ID and key and nothing else.
func (s *appTokenSource) discoverInstallation(ctx context.Context, jwt string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(s.repo) + "/installation"
	if err := s.call(ctx, http.MethodGet, path, jwt, &out); err != nil {
		return 0, fmt.Errorf("find the App's installation on %s/%s (is the App installed there?): %w", s.owner, s.repo, err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("no installation of App %d on %s/%s", s.appID, s.owner, s.repo)
	}
	return out.ID, nil
}

func (s *appTokenSource) call(ctx context.Context, method, path, jwt string, out any) error {
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ghError{Method: method, Path: path, Status: resp.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
