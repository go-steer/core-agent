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

package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestFacade_ErrorEventInStreamFailsTheCall: an endpoint that streams part
// of an answer and then an SSE event whose data is an error object fails
// the call with that error, rather than completing it with the partial
// text. genai before v1.76.0 read such an event as an empty chunk, so this
// pins the behaviour against a downgrade.
//
// The call stays on the v1beta path, which core-models pins for the
// Developer API since genai v1.78.0 made v1 the default.
func TestFacade_ErrorEventInStreamFailsTheCall(t *testing.T) {
	t.Parallel()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"+
			"data: {\"error\":{\"code\":500,\"message\":\"Internal error encountered.\",\"status\":\"INTERNAL\"}}\n\n")
	}))
	t.Cleanup(srv.Close)
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	p.baseURL = srv.URL
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	var last error
	for _, err := range m.GenerateContent(context.Background(), req, true) {
		if err != nil {
			last = err
			break
		}
	}
	if path != "/v1beta/models/gemini-3.5-flash:streamGenerateContent" {
		t.Errorf("the call went to %q, want the v1beta streamGenerateContent path", path)
	}
	var apiErr genai.APIError
	if !errors.As(last, &apiErr) || apiErr.Code != http.StatusInternalServerError {
		t.Fatalf("err %v, want the stream's genai.APIError with code 500", last)
	}
}
