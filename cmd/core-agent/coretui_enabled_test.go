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
	"errors"
	"testing"

	"google.golang.org/genai"
)

func TestSplitFunctionResponse_NoError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"content": "package main"},
	}
	body, errStr := splitFunctionResponse(resp)
	if errStr != "" {
		t.Errorf("expected empty errStr for success response, got %q", errStr)
	}
	if body["content"] != "package main" {
		t.Errorf("expected response body preserved, got %v", body)
	}
}

func TestSplitFunctionResponse_StringError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": "permission denied"},
	}
	body, errStr := splitFunctionResponse(resp)
	if errStr != "permission denied" {
		t.Errorf("expected 'permission denied' errStr, got %q", errStr)
	}
	// Body is still passed through (caller may want to inspect both).
	if body["error"] != "permission denied" {
		t.Errorf("expected error key preserved on body, got %v", body)
	}
}

func TestSplitFunctionResponse_ErrorTypeError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": errors.New("disk full")},
	}
	_, errStr := splitFunctionResponse(resp)
	if errStr != "disk full" {
		t.Errorf("expected 'disk full' from error-typed value, got %q", errStr)
	}
}

func TestSplitFunctionResponse_NilResp(t *testing.T) {
	body, errStr := splitFunctionResponse(nil)
	if body != nil || errStr != "" {
		t.Errorf("expected (nil, \"\") for nil input, got (%v, %q)", body, errStr)
	}
}

func TestSplitFunctionResponse_NilResponseMap(t *testing.T) {
	body, errStr := splitFunctionResponse(&genai.FunctionResponse{Name: "x"})
	if body != nil || errStr != "" {
		t.Errorf("expected (nil, \"\") for nil response map, got (%v, %q)", body, errStr)
	}
}

func TestSplitFunctionResponse_NonStringError(t *testing.T) {
	// An "error" key of an unexpected type (e.g. int) shouldn't crash
	// and shouldn't set errStr — we only recognize string / error.
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": 42},
	}
	_, errStr := splitFunctionResponse(resp)
	if errStr != "" {
		t.Errorf("expected empty errStr for non-string error value, got %q", errStr)
	}
}
