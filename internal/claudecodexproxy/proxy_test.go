package claudecodexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func toolOutputStringForTest(t *testing.T, output any) string {
	t.Helper()
	switch typed := output.(type) {
	case string:
		return typed
	case json.RawMessage:
		return string(typed)
	case []byte:
		return string(typed)
	default:
		blob, err := json.Marshal(typed)
		if err != nil {
			t.Fatalf("marshal tool output %#v: %v", output, err)
		}
		return string(blob)
	}
}

func newClientAuthProxyForTest(t *testing.T, clientAPIKey string) (*Proxy, *int, func()) {
	t.Helper()

	backendHits := new(int)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*backendHits++
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		ClientAPIKey:   clientAPIKey,
	})
	return proxy, backendHits, backend.Close
}

func newBasicMessagesRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestBuildBackendRequestConvertsToolHistory(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5-codex",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:  "claude-sonnet-4-5",
		System: "You are helpful",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "plan"},
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "bash",
					"input": map[string]any{"command": "pwd"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     "D:\\repo",
				},
				map[string]any{
					"type": "text",
					"text": "continue",
				},
			}},
		},
		Tools: []AnthropicTool{
			{
				Name:        "bash",
				Description: "run shell",
				InputSchema: map[string]any{
					"type": "object",
				},
			},
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Model != "gpt-5-codex" {
		t.Fatalf("backend model = %q, want gpt-5-codex", payload.Model)
	}
	if payload.Instructions != "You are helpful" {
		t.Fatalf("instructions = %q, want You are helpful", payload.Instructions)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization header = %q", got)
	}
	if len(payload.Input) != 3 {
		t.Fatalf("input item count = %d, want 3", len(payload.Input))
	}
	if payload.Input[1].Type != "function_call" || payload.Input[1].CallID != "toolu_1" {
		t.Fatalf("tool use mapping incorrect: %#v", payload.Input[1])
	}
	if payload.Input[2].Type != "function_call_output" || toolOutputStringForTest(t, payload.Input[2].Output) != "D:\\repo\n\ncontinue" {
		t.Fatalf("tool result mapping incorrect: %#v", payload.Input[2])
	}
	if len(payload.Tools) != 1 || payload.Tools[0].Name != "bash" {
		t.Fatalf("tool conversion incorrect: %#v", payload.Tools)
	}
	if payload.Metadata["x-claude-code-session-id"] != "" {
		t.Fatalf("raw session id should not be forwarded: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["claude_code_prompt_cache_key"]; ok {
		t.Fatalf("raw prompt cache key should not be mirrored into metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_root_session_id"] == "" {
		t.Fatalf("continuity root session missing: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestDerivesPromptCacheKeyFromJSONUserID(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"user_id": `{"device_id":"dev-1","account_uuid":"","session_id":"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752"}`,
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752" {
		t.Fatalf("prompt_cache_key = %q", payload.PromptCacheKey)
	}
	if payload.Metadata != nil {
		if _, ok := payload.Metadata["user_id"]; ok {
			t.Fatalf("raw user_id should not be forwarded: %#v", payload.Metadata)
		}
	}
}

func TestBuildBackendRequestDerivesPromptCacheKeyFromLegacyUserID(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"user_id": "user_deadbeef_account__session_7d0e2f61-4b5c-4a9d-8f11-2c3d4e5f6a7b",
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "7d0e2f61-4b5c-4a9d-8f11-2c3d4e5f6a7b" {
		t.Fatalf("prompt_cache_key = %q", payload.PromptCacheKey)
	}
}

func TestBuildBackendRequestDerivesContinuityMetadataFromSessionAndSubagent(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": `<system-reminder>
__SUBAGENT_MARKER__{"session_id":"root-session","agent_id":"agent-1","agent_type":"researcher"}
</system-reminder>`,
					},
					map[string]any{
						"type": "text",
						"text": "hello",
					},
				},
			},
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if _, ok := payload.Metadata["claude_code_prompt_cache_key"]; ok {
		t.Fatalf("prompt cache key should not be mirrored into metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_root_session_id"] == "" {
		t.Fatalf("root session id missing: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_request_id"] == "" {
		t.Fatalf("request id missing: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["claude_code_marker_session_id"]; ok {
		t.Fatalf("raw marker session should not be mirrored into metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_subagent_id"] != "agent-1" || payload.Metadata["claude_code_subagent_type"] != "researcher" {
		t.Fatalf("subagent continuity missing: %#v", payload.Metadata)
	}
	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	if got := payload.Input[0].Content[0].Text; strings.Contains(got, "__SUBAGENT_MARKER__") || strings.Contains(got, "<system-reminder>") {
		t.Fatalf("subagent marker leaked into model input: %q", got)
	}
}

func TestBuildBackendRequestUsesHeaderSessionForPromptCacheKey(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "session-1" {
		t.Fatalf("prompt_cache_key = %q, want session-1", payload.PromptCacheKey)
	}
}

func TestBuildBackendRequestDoesNotMirrorRawPromptCacheKeyIntoMetadata(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "session-1" {
		t.Fatalf("prompt_cache_key = %q, want session-1", payload.PromptCacheKey)
	}
	if got := payload.Metadata["claude_code_prompt_cache_key"]; got != "" {
		t.Fatalf("raw prompt_cache_key should not be mirrored into metadata, metadata = %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestAddsSecondWaveContinuityMetadata(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, http.Header{
		"X-Session-Affinity":  []string{"aff-1"},
		"X-Parent-Session-Id": []string{"parent-1"},
		"X-Request-Id":        []string{"req-1"},
		"Traceparent":         []string{"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
		"X-Interaction-Type":  []string{"conversation-subagent"},
		"X-Interaction-Id":    []string{"interaction-1"},
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["claude_code_session_affinity"] != stableUUID("aff-1") {
		t.Fatalf("missing session affinity metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_parent_session_id"] != stableUUID("parent-1") {
		t.Fatalf("missing parent session metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_inbound_request_id"] != "req-1" {
		t.Fatalf("missing inbound request metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_trace_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("missing trace metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_interaction_type"] != "conversation" {
		t.Fatalf("missing interaction type metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_interaction_id"] == "" {
		t.Fatalf("missing interaction id metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_request_id"] == "" {
		t.Fatalf("missing generated request id metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_request_id"] == payload.Metadata["claude_code_inbound_request_id"] {
		t.Fatalf("generated request id should remain distinct from inbound request id: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestDerivesCompactAutoContinueInteractionType(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{
				Role:    "user",
				Content: compactAutoContinueClaudePrompt,
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["claude_code_interaction_type"] != "compact_auto_continue" {
		t.Fatalf("interaction type = %q, want compact_auto_continue", payload.Metadata["claude_code_interaction_type"])
	}
}

func TestBuildBackendRequestSanitizesControlCharactersFromMetadataValues(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Metadata: map[string]any{"trace": "line1\r\nline2\x00end"},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["trace"] != "line1line2end" {
		t.Fatalf("trace metadata = %q, want sanitized line1line2end", payload.Metadata["trace"])
	}
}

func TestBuildBackendRequestCanDisableForwardingUserMetadata(t *testing.T) {
	cfg := Config{
		BackendBaseURL:                "https://example.com/codex",
		BackendPath:                   "/v1/responses",
		BackendAPIKey:                 "test-key",
		BackendModel:                  "gpt-5.4",
		EnableBackendMetadata:         true,
		DisableUserMetadataForwarding: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"trace":   "abc",
			"user_id": `{"device_id":"dev-1","account_uuid":"","session_id":"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752"}`,
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if _, ok := payload.Metadata["trace"]; ok {
		t.Fatalf("user metadata trace should be omitted when forwarding disabled: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["user_id"]; ok {
		t.Fatalf("raw user_id should never be forwarded: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_root_session_id"] == "" || payload.Metadata["claude_code_request_id"] == "" {
		t.Fatalf("bridge-derived continuity metadata should remain: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestCanDisableContinuityMetadata(t *testing.T) {
	cfg := Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableBackendMetadata:     true,
		DisableContinuityMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{"trace": "abc"},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["trace"] != "abc" {
		t.Fatalf("user metadata should remain when continuity metadata disabled: %#v", payload.Metadata)
	}
	for _, key := range []string{
		"claude_code_root_session_id",
		"claude_code_request_id",
		"claude_code_session_affinity",
		"claude_code_interaction_id",
	} {
		if _, ok := payload.Metadata[key]; ok {
			t.Fatalf("continuity metadata key %q should be omitted: %#v", key, payload.Metadata)
		}
	}
}

func TestBuildBackendRequestCanDisablePromptCacheKey(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		DisablePromptCacheKey: true,
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"user_id": `{"device_id":"dev-1","account_uuid":"","session_id":"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752"}`,
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key should be omitted when disabled: %#v", payload.PromptCacheKey)
	}
}

func TestHandleMessagesDoesNotSendPromptCacheKeyWhenDisabled(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		DisablePromptCacheKey: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"\",\"session_id\":\"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752\"}"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if _, ok := requests[0]["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be absent when disabled: %#v", requests[0])
	}
}

func TestHandlerRejectsMessagesWithoutClientAuth(t *testing.T) {
	proxy, backendHits, cleanup := newClientAuthProxyForTest(t, "client-key")
	defer cleanup()

	recorder := httptest.NewRecorder()
	request := newBasicMessagesRequest()

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if *backendHits != 0 {
		t.Fatalf("backend hits = %d, want 0", *backendHits)
	}
}

func TestHandlerRejectsMessagesWithWrongClientAuth(t *testing.T) {
	proxy, backendHits, cleanup := newClientAuthProxyForTest(t, "client-key")
	defer cleanup()

	recorder := httptest.NewRecorder()
	request := newBasicMessagesRequest()
	request.Header.Set("Authorization", "Bearer wrong-key")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if *backendHits != 0 {
		t.Fatalf("backend hits = %d, want 0", *backendHits)
	}
}

func TestHandlerAcceptsMessagesWithBearerClientAuth(t *testing.T) {
	proxy, backendHits, cleanup := newClientAuthProxyForTest(t, "client-key")
	defer cleanup()

	recorder := httptest.NewRecorder()
	request := newBasicMessagesRequest()
	request.Header.Set("Authorization", "Bearer client-key")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if *backendHits != 1 {
		t.Fatalf("backend hits = %d, want 1", *backendHits)
	}
}

func TestHandlerAcceptsMessagesWithXAPIKeyClientAuth(t *testing.T) {
	proxy, backendHits, cleanup := newClientAuthProxyForTest(t, "client-key")
	defer cleanup()

	recorder := httptest.NewRecorder()
	request := newBasicMessagesRequest()
	request.Header.Set("x-api-key", "client-key")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if *backendHits != 1 {
		t.Fatalf("backend hits = %d, want 1", *backendHits)
	}
}

func TestHandlerLeavesHealthzUnauthenticated(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		ClientAPIKey:   "client-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerProtectsModelsAndCountTokensWhenClientAuthEnabled(t *testing.T) {
	backendHits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "gpt-5.4"},
				},
			})
		default:
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		ClientAPIKey:   "client-key",
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	countRecorder := httptest.NewRecorder()
	countRequest := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	countRequest.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(countRecorder, countRequest)
	if countRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("count_tokens status = %d, body = %s", countRecorder.Code, countRecorder.Body.String())
	}

	if backendHits != 0 {
		t.Fatalf("backend hits = %d, want 0", backendHits)
	}
}

func TestBuildBackendRequestForwardUserMetadataAliasOverridesLegacyDisableFlag(t *testing.T) {
	cfg := Config{
		BackendBaseURL:                "https://example.com/codex",
		BackendPath:                   "/v1/responses",
		BackendAPIKey:                 "test-key",
		BackendModel:                  "gpt-5.4",
		EnableBackendMetadata:         true,
		DisableUserMetadataForwarding: true,
	}
	forward := true
	cfg.ForwardUserMetadata = &forward

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{"trace": "abc"},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["trace"] != "abc" {
		t.Fatalf("ForwardUserMetadata alias should override legacy disable flag: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestUserMetadataAllowlistFiltersUserMetadataOnly(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
		UserMetadataAllowlist: []string{"trace"},
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"trace":                  "abc",
			"tenant":                 "team-1",
			"claude_code_request_id": "evil",
			"user_id":                `{"device_id":"dev-1","account_uuid":"","session_id":"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752"}`,
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["trace"] != "abc" {
		t.Fatalf("allowlisted trace metadata missing: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["tenant"]; ok {
		t.Fatalf("non-allowlisted tenant metadata should be omitted: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["user_id"]; ok {
		t.Fatalf("raw user_id should never be forwarded: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_request_id"] == "evil" {
		t.Fatalf("user-supplied claude_code_* should not override bridge metadata: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_root_session_id"] == "" {
		t.Fatalf("bridge-derived metadata should not be filtered by allowlist: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestUserMetadataAllowlistIsCaseSensitiveExactMatch(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
		UserMetadataAllowlist: []string{"Trace", "tenant_id"},
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"trace":     "lowercase-omitted",
			"Trace":     "exact-kept",
			"tenant":    "prefix-omitted",
			"tenant_id": "exact-tenant-kept",
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata["Trace"] != "exact-kept" {
		t.Fatalf("exact-case allowlisted metadata missing: %#v", payload.Metadata)
	}
	if payload.Metadata["tenant_id"] != "exact-tenant-kept" {
		t.Fatalf("exact allowlisted tenant_id missing: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["trace"]; ok {
		t.Fatalf("allowlist should be case-sensitive exact match for trace: %#v", payload.Metadata)
	}
	if _, ok := payload.Metadata["tenant"]; ok {
		t.Fatalf("allowlist should not treat prefixes as matches: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestAllowlistIgnoredWhenForwardUserMetadataDisabled(t *testing.T) {
	cfg := Config{
		BackendBaseURL:                "https://example.com/codex",
		BackendPath:                   "/v1/responses",
		BackendAPIKey:                 "test-key",
		BackendModel:                  "gpt-5.4",
		EnableBackendMetadata:         true,
		DisableUserMetadataForwarding: true,
		UserMetadataAllowlist:         []string{"trace"},
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"trace": "abc",
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if _, ok := payload.Metadata["trace"]; ok {
		t.Fatalf("allowlist should not override disabled user metadata forwarding: %#v", payload.Metadata)
	}
	if payload.Metadata["claude_code_root_session_id"] == "" {
		t.Fatalf("bridge-derived metadata should remain: %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestDoesNotMirrorRawMarkerSessionID(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": `<system-reminder>
__SUBAGENT_MARKER__{"session_id":"root-session","agent_id":"agent-1","agent_type":"researcher"}
</system-reminder>`,
					},
					map[string]any{
						"type": "text",
						"text": "hello",
					},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if _, ok := payload.Metadata["claude_code_marker_session_id"]; ok {
		t.Fatalf("raw marker session should not be mirrored, metadata = %#v", payload.Metadata)
	}
}

func TestBuildBackendRequestCapsMaxOutputTokensFromModelProfile(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_output_tokens": 2048,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.MaxOutputTokens != 2048 {
		t.Fatalf("MaxOutputTokens = %d, want 2048", payload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestDoesNotCapMaxOutputTokensWhenCapabilityInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_output_tokens": 2048,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.MaxOutputTokens != 4096 {
		t.Fatalf("MaxOutputTokens = %d, want 4096 when init disabled", payload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestDoesNotCapWhenProfileLimitExceedsRequest(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_output_tokens": 8192,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.MaxOutputTokens != 4096 {
		t.Fatalf("MaxOutputTokens = %d, want 4096", payload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestDoesNotCapWhenProfileLimitMissing(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits":   map[string]any{},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.MaxOutputTokens != 4096 {
		t.Fatalf("MaxOutputTokens = %d, want 4096", payload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestFallsBackWhenProfilePromptLimitTooLow(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-large",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 4096,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: strings.Repeat("long prompt ", 20)}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4-large" {
		t.Fatalf("Model = %q, want fallback model gpt-5.4-large", payload.Model)
	}
}

func TestBuildBackendRequestIgnoresPromptLimitFallbackWhenCapabilityInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-large",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 4096,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: strings.Repeat("long prompt ", 20)}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want pinned model gpt-5.4 when init disabled", payload.Model)
	}
}

func TestBuildBackendRequestFallsBackWhenProfileDisablesToolCalls(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-tools",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": true,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": false,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4-tools" {
		t.Fatalf("Model = %q, want fallback tool-capable model", payload.Model)
	}
}

func TestBuildBackendRequestEnablesParallelToolCallsWhenProfileSupportsIt(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls":          true,
					"parallel_tool_calls": true,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.ParallelToolCalls == nil || !*payload.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls = %#v, want true", payload.ParallelToolCalls)
	}
}

func TestBuildBackendRequestOmitsParallelToolCallsWhenProfileDoesNotAdvertiseIt(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": true,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var raw map[string]any
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if _, ok := raw["parallel_tool_calls"]; ok {
		t.Fatalf("parallel_tool_calls should be omitted when not advertised: %#v", raw["parallel_tool_calls"])
	}
}

func TestBuildBackendRequestDowngradesHighReasoningWhenMaxThinkingBudgetIsSmall(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"max_thinking_budget": 4000,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:        "claude-sonnet-4-5",
		OutputConfig: &AnthropicOutputConfig{Effort: "max"},
		Messages:     []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Reasoning == nil || payload.Reasoning.Effort != "medium" {
		t.Fatalf("Reasoning = %#v, want medium after max thinking budget cap", payload.Reasoning)
	}
}

func TestBuildBackendRequestDoesNotAutoCreateReasoningFromThinkingBudgetProfile(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"min_thinking_budget": 4096,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Reasoning != nil {
		t.Fatalf("Reasoning = %#v, want nil without user reasoning request", payload.Reasoning)
	}
}

func TestBuildBackendRequestFallsBackWhenThinkingBudgetBelowProfileMinimum(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-flex",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"min_thinking_budget": 512,
					"max_thinking_budget": 8192,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"min_thinking_budget": 4096,
					"max_thinking_budget": 8192,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Thinking: &AnthropicThinking{Type: "enabled", BudgetTokens: 1024},
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4-flex" {
		t.Fatalf("Model = %q, want thinking-budget-compatible fallback", payload.Model)
	}
}

func TestBuildBackendRequestIgnoresThinkingBudgetProfileWhenCapabilityInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-flex",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"min_thinking_budget": 512,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"min_thinking_budget": 4096,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Thinking: &AnthropicThinking{Type: "enabled", BudgetTokens: 1024},
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want pinned model when init disabled", payload.Model)
	}
}

func TestBuildBackendRequestKeepsCurrentModelWhenToolCallSupportUnknown(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-tools",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": true,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want current model when tool_calls support is unknown", payload.Model)
	}
	if len(payload.Tools) != 1 || payload.Tools[0].Name != "Read" {
		t.Fatalf("tools should remain intact: %#v", payload.Tools)
	}
}

func TestBuildBackendRequestKeepsToolsWhenNoFallbackModelSupportsToolCalls(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-no-tools",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": false,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"tool_calls": false,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want current model when no fallback supports tools", payload.Model)
	}
	if len(payload.Tools) != 1 || payload.Tools[0].Name != "Read" {
		t.Fatalf("tools should remain intact when fallback fails: %#v", payload.Tools)
	}
	if len(payload.Input) != 1 || len(payload.Input[0].Content) != 1 {
		t.Fatalf("prompt should remain a single user text item: %#v", payload.Input)
	}
	if got := payload.Input[0].Content[0].Text; got != "hello" {
		t.Fatalf("prompt text = %q, want original text hello", got)
	}
	if marshaled, err := json.Marshal(payload.Input); err != nil {
		t.Fatalf("marshal input: %v", err)
	} else if strings.Contains(string(marshaled), "Respond with TEXT ONLY") {
		t.Fatalf("prompt should not be rewritten to text-only guard: %s", marshaled)
	}
}

func TestSeedCapabilitiesFromModelsResetsPreferredModel(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{"id": "old-model"}),
	})
	if proxy.caps.PreferredModel != "old-model" {
		t.Fatalf("PreferredModel = %q, want old-model", proxy.caps.PreferredModel)
	}

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{"id": "new-model"}),
	})
	if proxy.caps.PreferredModel != "new-model" {
		t.Fatalf("PreferredModel = %q, want refreshed new-model", proxy.caps.PreferredModel)
	}
}

func TestFetchBackendModelsRefreshesPreferredModelAndClearsOldProfiles(t *testing.T) {
	modelsCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		modelsCalls++
		if modelsCalls == 1 {
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "old-model",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{"max_prompt_tokens": 4096},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{"max_prompt_tokens": 10},
						},
					},
				},
			})
			return
		}
		writeJSONWithStatus(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"id":                  "new-model",
					"supported_endpoints": []string{"/responses"},
					"capabilities": map[string]any{
						"limits": map[string]any{"max_prompt_tokens": 4096},
					},
				},
				{
					"id":                  "gpt-5.4",
					"supported_endpoints": []string{"/responses"},
					"capabilities": map[string]any{
						"limits": map[string]any{"max_prompt_tokens": 10},
					},
				},
			},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	if _, ok := proxy.fetchBackendModels(); !ok {
		t.Fatalf("first fetchBackendModels failed")
	}
	if proxy.caps.PreferredModel != "old-model" {
		t.Fatalf("PreferredModel after first fetch = %q, want old-model", proxy.caps.PreferredModel)
	}
	if _, ok := proxy.caps.ModelProfiles["old-model"]; !ok {
		t.Fatalf("old-model profile missing after first fetch")
	}

	if _, ok := proxy.fetchBackendModels(); !ok {
		t.Fatalf("second fetchBackendModels failed")
	}
	if proxy.caps.PreferredModel != "new-model" {
		t.Fatalf("PreferredModel after second fetch = %q, want new-model", proxy.caps.PreferredModel)
	}
	if _, ok := proxy.caps.ModelProfiles["old-model"]; ok {
		t.Fatalf("old-model profile should be cleared after second fetch")
	}
	if _, ok := proxy.caps.SupportedModels["old-model"]; ok {
		t.Fatalf("old-model should not remain in supported models after second fetch")
	}

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: strings.Repeat("long prompt ", 20)}},
	}, nil)
	if err != nil {
		t.Fatalf("build request after refreshed fetch: %v", err)
	}
	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "new-model" {
		t.Fatalf("Model after refreshed fetch = %q, want new-model", payload.Model)
	}
}

func TestBuildBackendRequestDoesNotCapWhenProfileLimitZero(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_output_tokens": 0,
				},
			},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.MaxOutputTokens != 4096 {
		t.Fatalf("MaxOutputTokens = %d, want 4096", payload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestWarmupLimitDoesNotPolluteNormalModel(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendWarmupModel:        "gpt-5.4-mini",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4-mini",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"streaming": true,
				},
				"limits": map[string]any{
					"max_output_tokens": 256,
				},
			},
		}),
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"streaming": true,
				},
				"limits": map[string]any{
					"max_output_tokens": 4096,
				},
			},
		}),
	})

	warmupReq, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: "warmup please"}},
	}, http.Header{"Anthropic-Beta": []string{"warmup-beta"}})
	if err != nil {
		t.Fatalf("build warmup request: %v", err)
	}

	var warmupPayload OpenAIResponsesRequest
	if err := json.NewDecoder(warmupReq.Body).Decode(&warmupPayload); err != nil {
		t.Fatalf("decode warmup request: %v", err)
	}
	if warmupPayload.Model != "gpt-5.4-mini" {
		t.Fatalf("warmup model = %q, want gpt-5.4-mini", warmupPayload.Model)
	}
	if warmupPayload.MaxOutputTokens != 256 {
		t.Fatalf("warmup MaxOutputTokens = %d, want 256", warmupPayload.MaxOutputTokens)
	}

	normalReq, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: "normal request"}},
		Tools: []AnthropicTool{
			{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build normal request: %v", err)
	}

	var normalPayload OpenAIResponsesRequest
	if err := json.NewDecoder(normalReq.Body).Decode(&normalPayload); err != nil {
		t.Fatalf("decode normal request: %v", err)
	}
	if normalPayload.Model == "gpt-5.4-mini" {
		t.Fatalf("normal request should not stay on warmup model: %#v", normalPayload)
	}
	if normalPayload.MaxOutputTokens != 1024 {
		t.Fatalf("normal MaxOutputTokens = %d, want non-polluted 1024", normalPayload.MaxOutputTokens)
	}
}

func TestBuildBackendRequestIgnoresSeededProfileBehaviorsWhenCapabilityInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnablePhaseCommentary: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":  false,
					"streaming":          false,
					"structured_outputs": false,
					"phase":              false,
				},
				"limits": map[string]any{
					"max_output_tokens": 512,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:     "claude-sonnet-4-5",
		Stream:    true,
		MaxTokens: 4096,
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "I will inspect this."},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content": []any{
							map[string]any{"type": "text", "text": "stdout"},
							map[string]any{"type": "json", "json": map[string]any{"severity": "high"}},
						},
					},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.MaxOutputTokens != 4096 {
		t.Fatalf("MaxOutputTokens = %d, want uncapped 4096 when init disabled", payload.MaxOutputTokens)
	}
	if !payload.Stream {
		t.Fatalf("stream should remain enabled when init disabled: %#v", payload)
	}
	if payload.Reasoning == nil {
		t.Fatalf("reasoning should remain enabled when init disabled: %#v", payload)
	}
	if len(payload.Input) < 3 {
		t.Fatalf("input item count = %d, want at least 3", len(payload.Input))
	}
	if payload.Input[0].Phase != "commentary" {
		t.Fatalf("phase should remain enabled when init disabled: %#v", payload.Input[0])
	}
	output, ok := payload.Input[2].Output.([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("structured tool_result output should remain preserved when init disabled: %#v", payload.Input[2].Output)
	}
}

func TestBuildBackendRequestSanitizesIDETools(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
		Tools: []AnthropicTool{
			{Name: "mcp__ide__executeCode", Description: "execute", InputSchema: map[string]any{"type": "object"}},
			{Name: "mcp__ide__getDiagnostics", Description: "old", InputSchema: map[string]any{"type": "object"}},
			{Name: "keep_me", Description: "keep", InputSchema: map[string]any{"type": "object"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Tools) != 2 {
		t.Fatalf("tool count = %d, want 2", len(payload.Tools))
	}
	if payload.Tools[0].Name != "mcp__ide__getDiagnostics" || payload.Tools[0].Description != ideGetDiagnosticsDescription {
		t.Fatalf("diagnostics tool not sanitized: %#v", payload.Tools[0])
	}
	if payload.Tools[1].Name != "keep_me" {
		t.Fatalf("unexpected second tool: %#v", payload.Tools[1])
	}
}

func TestBuildBackendRequestSkipsLastMergeForCompactRequest(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content":     "Launching skill: compact",
					},
					map[string]any{
						"type": "text",
						"text": strings.Join([]string{
							compactTextOnlyGuard,
							compactSummaryPromptStart,
							"Pending Tasks:",
							"Current Work:",
						}, "\n\n"),
					},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if payload.Input[0].Type != "function_call_output" || payload.Input[1].Type != "message" {
		t.Fatalf("compact last message should not be merged: %#v", payload.Input)
	}
	if got := toolOutputStringForTest(t, payload.Input[0].Output); got != "Launching skill: compact" {
		t.Fatalf("tool output should stay unmerged, got %q", got)
	}
}

func TestBuildBackendRequestNormalizesObjectToolSchemaWithoutProperties(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
		Tools: []AnthropicTool{
			{
				Name:        "empty_object_tool",
				Description: "schema omits properties",
				InputSchema: map[string]any{
					"type": "object",
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var raw map[string]any
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	tools, ok := raw["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools malformed: %#v", raw["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool malformed: %#v", tools[0])
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters malformed: %#v", tool["parameters"])
	}
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("object schema missing normalized empty properties object: %#v", parameters)
	}
	if len(properties) != 0 {
		t.Fatalf("properties = %#v, want empty object", properties)
	}
}

func TestBuildBackendRequestRecursivelyNormalizesObjectToolSchemas(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
		Tools: []AnthropicTool{
			{
				Name:        "nested_schema_tool",
				Description: "nested object schemas omit properties",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"config": map[string]any{"type": "object"},
						"items": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "object"},
						},
						"choice": map[string]any{
							"anyOf": []any{
								map[string]any{"type": "object"},
								map[string]any{"type": "string"},
							},
						},
					},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var raw map[string]any
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	tools := raw["tools"].([]any)
	tool := tools[0].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)

	configSchema := properties["config"].(map[string]any)
	if _, ok := configSchema["properties"].(map[string]any); !ok {
		t.Fatalf("nested object schema missing properties: %#v", configSchema)
	}
	itemsSchema := properties["items"].(map[string]any)
	itemSchema := itemsSchema["items"].(map[string]any)
	if _, ok := itemSchema["properties"].(map[string]any); !ok {
		t.Fatalf("array item object schema missing properties: %#v", itemSchema)
	}
	choiceSchema := properties["choice"].(map[string]any)
	anyOf := choiceSchema["anyOf"].([]any)
	choiceObject := anyOf[0].(map[string]any)
	if _, ok := choiceObject["properties"].(map[string]any); !ok {
		t.Fatalf("anyOf object schema missing properties: %#v", choiceObject)
	}
}

func TestBuildBackendRequestPreservesStructuredToolResultContentAsArrayOutput(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "inspect",
					"input": map[string]any{"path": "report.json"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "text", "text": "stdout"},
						map[string]any{
							"type": "json",
							"json": map[string]any{
								"findings": []any{
									map[string]any{"id": "F-1", "severity": "high"},
								},
							},
						},
					},
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 2 || payload.Input[1].Type != "function_call_output" {
		t.Fatalf("tool result mapping incorrect: %#v", payload.Input)
	}

	content, ok := payload.Input[1].Output.([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("structured tool_result output not preserved as array: %#v", payload.Input[1].Output)
	}
	textBlock, ok := content[0].(map[string]any)
	if !ok || textBlock["type"] != "input_text" || textBlock["text"] != "stdout" {
		t.Fatalf("text block not preserved: %#v", content[0])
	}
	jsonBlock, ok := content[1].(map[string]any)
	if !ok || jsonBlock["type"] != "input_text" || jsonBlock["text"] != `{"findings":[{"id":"F-1","severity":"high"}]}` {
		t.Fatalf("json block not preserved: %#v", content[1])
	}
}

func TestBuildBackendRequestConvertsImageDocumentAndThinking(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       "iVBORw0KGgo=",
					},
				},
				map[string]any{
					"type":  "document",
					"title": "note.txt",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "text/plain",
						"data":       "SGVsbG8=",
					},
				},
				map[string]any{
					"type":     "thinking",
					"thinking": "hidden chain",
				},
				map[string]any{
					"type": "redacted_thinking",
					"data": "opaque",
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	content := payload.Input[0].Content
	if len(content) != 2 {
		t.Fatalf("content item count = %d, want 2", len(content))
	}
	if content[0].Type != "input_image" || !strings.HasPrefix(content[0].ImageURL, "data:image/png;base64,") {
		t.Fatalf("image mapping incorrect: %#v", content[0])
	}
	if content[1].Type != "input_file" || content[1].Filename != "note.txt" || !strings.HasPrefix(content[1].FileData, "data:text/plain;base64,") {
		t.Fatalf("document mapping incorrect: %#v", content[1])
	}
}

func TestBuildBackendRequestConvertsDocumentURLAndReasoningCarrier(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
		Summary: []OpenAIReasoningPart{
			{Type: "summary_text", Text: "brief reasoning"},
		},
	})

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":      "thinking",
					"thinking":  "brief reasoning",
					"signature": carrier,
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":  "document",
					"title": "paper.pdf",
					"source": map[string]any{
						"type": "url",
						"url":  "https://example.com/paper.pdf",
					},
				},
				map[string]any{
					"type": "redacted_thinking",
					"data": carrier,
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 3 {
		t.Fatalf("input item count = %d, want 3", len(payload.Input))
	}
	if payload.Input[0].Type != "reasoning" || payload.Input[0].EncryptedContent != "opaque-reasoning" {
		t.Fatalf("thinking carrier mapping incorrect: %#v", payload.Input[0])
	}
	if payload.Input[1].Role != "user" || payload.Input[1].Content[0].FileURL != "https://example.com/paper.pdf" {
		t.Fatalf("document url mapping incorrect: %#v", payload.Input[1])
	}
	if payload.Input[2].Type != "reasoning" || payload.Input[2].EncryptedContent != "opaque-reasoning" {
		t.Fatalf("redacted thinking mapping incorrect: %#v", payload.Input[2])
	}
	if len(payload.Include) == 0 || payload.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include missing reasoning.encrypted_content: %#v", payload.Include)
	}
	if !payload.Store {
		t.Fatalf("store should stay enabled so returned reasoning carriers remain reusable across turns")
	}
}

func TestBuildBackendRequestConvertsCompactionCarrier(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	carrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")
	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":      "thinking",
					"thinking":  defaultThinkingText,
					"signature": carrier,
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	if payload.Input[0].Type != "compaction" || payload.Input[0].ID != "cmp_1" || payload.Input[0].EncryptedContent != "opaque-compaction" {
		t.Fatalf("compaction carrier mapping incorrect: %#v", payload.Input[0])
	}
}

func TestBuildBackendRequestDropsReasoningItemIDWithoutEncryptedContent(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:      "rs_1",
		Type:    "reasoning",
		Summary: []OpenAIReasoningPart{{Type: "summary_text", Text: "brief reasoning"}},
	})

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":      "thinking",
					"thinking":  "brief reasoning",
					"signature": carrier,
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	if payload.Input[0].Type != "reasoning" {
		t.Fatalf("reasoning mapping incorrect: %#v", payload.Input[0])
	}
	if payload.Input[0].ID != "" {
		t.Fatalf("reasoning item id should be dropped when encrypted_content is unavailable: %#v", payload.Input[0])
	}
}

func TestBuildBackendRequestInjectsContextManagementCompactionWhenProfileAllows(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	carrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")
	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": carrier},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.ContextManagement) != 1 || payload.ContextManagement[0].Type != "compaction" {
		t.Fatalf("context_management compaction missing: %#v", payload.ContextManagement)
	}
}

func TestBuildBackendRequestKeepsOnlyLatestCompactionAndFollowingItems(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": oldCarrier},
			}},
			{Role: "user", Content: "before latest"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": newCarrier},
			}},
			{Role: "user", Content: "after latest"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2 (latest compaction + trailing message)", len(payload.Input))
	}
	if payload.Input[0].Type != "compaction" || payload.Input[0].ID != "cmp_new" {
		t.Fatalf("latest compaction not retained: %#v", payload.Input)
	}
}

func TestBuildBackendRequestDoesNotInjectContextManagementCompactionWhenInitDisabled(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	carrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")
	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": carrier},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var raw map[string]any
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if _, ok := raw["context_management"]; ok {
		t.Fatalf("context_management should be omitted when capability init disabled: %#v", raw["context_management"])
	}
}

func TestBuildBackendRequestStillShrinksToLatestCompactionWhenInitDisabled(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": oldCarrier},
			}},
			{Role: "user", Content: "before latest"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": newCarrier},
			}},
			{Role: "user", Content: "after latest"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if payload.Input[0].Type != "compaction" || payload.Input[0].ID != "cmp_new" {
		t.Fatalf("latest compaction should still be retained when init disabled: %#v", payload.Input)
	}
}

func TestBuildBackendRequestDowngradesCompactionInputWhenCapabilityDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})
	scopeKey := backendCapabilityScopeKey("gpt-5.4")
	proxy.scopedCaps[scopeKey] = scopedRuntimeCapabilities{CompactionInput: capabilityUnsupported}

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	backendReq, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "Thinking...", "signature": oldCarrier}}},
			{Role: "user", Content: "before latest"},
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "Thinking...", "signature": newCarrier}}},
			{Role: "user", Content: "after latest"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if backendReq == nil {
		t.Fatal("backend request is nil")
	}
	var backendPayload OpenAIResponsesRequest
	if err := json.NewDecoder(backendReq.Body).Decode(&backendPayload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(backendPayload.ContextManagement) != 0 {
		t.Fatalf("context_management should not be injected when compaction input is downgraded: %#v", backendPayload.ContextManagement)
	}
	if len(backendPayload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2 (latest compaction fallback + trailing message)", len(backendPayload.Input))
	}
	if backendPayload.Input[0].Type != "reasoning" || backendPayload.Input[0].ID != "cmp_new" || backendPayload.Input[0].EncryptedContent != "opaque-new" {
		t.Fatalf("latest compaction should be downgraded to reasoning carrier: %#v", backendPayload.Input)
	}
}

func TestBuildBackendRequestDropsCompactionInputWhenCompactionAndReasoningAreDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})
	scopeKey := backendCapabilityScopeKey("gpt-5.4")
	proxy.scopedCaps[scopeKey] = scopedRuntimeCapabilities{
		CompactionInput: capabilityUnsupported,
		Reasoning:       capabilityUnsupported,
	}

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	backendReq, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "Thinking...", "signature": oldCarrier}}},
			{Role: "user", Content: "before latest"},
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "Thinking...", "signature": newCarrier}}},
			{Role: "user", Content: "after latest"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	var backendPayload OpenAIResponsesRequest
	if err := json.NewDecoder(backendReq.Body).Decode(&backendPayload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(backendPayload.Input) != 1 {
		t.Fatalf("input item count = %d, want only trailing message after dropping unsupported compaction", len(backendPayload.Input))
	}
	if backendPayload.Input[0].Type != "message" || backendPayload.Input[0].Role != "user" {
		t.Fatalf("trailing user message should remain after dropping unsupported compaction: %#v", backendPayload.Input)
	}
}

func TestHandleMessagesRetriesWithoutContextManagementButKeepsLatestCompactionShrinking(t *testing.T) {
	type captured struct {
		ContextManagement any
		Input             []any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		var input []any
		if rawInput, ok := body["input"].([]any); ok {
			input = rawInput
		}
		requests = append(requests, captured{
			ContextManagement: body["context_management"],
			Input:             input,
		})
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: context_management","type":"invalid_request_error","param":"context_management"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_compaction_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})
	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+oldCarrier+`"}]},
			{"role":"user","content":"before latest"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+newCarrier+`"}]},
			{"role":"user","content":"after latest"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].ContextManagement == nil {
		t.Fatalf("first request should include context_management")
	}
	if requests[1].ContextManagement != nil {
		t.Fatalf("second request should drop context_management after unsupported retry: %#v", requests[1].ContextManagement)
	}
	for i, req := range requests {
		if len(req.Input) != 2 {
			t.Fatalf("request %d input count = %d, want 2", i, len(req.Input))
		}
		first := req.Input[0].(map[string]any)
		if first["type"] != "compaction" || first["id"] != "cmp_new" {
			t.Fatalf("request %d should still be shrunk to latest compaction: %#v", i, req.Input)
		}
	}
}

func TestHandleMessagesRetriesWithDowngradedCompactionInputAfterUnsupportedType(t *testing.T) {
	type captured struct {
		ContextManagement any
		Input             []any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		var input []any
		if rawInput, ok := body["input"].([]any); ok {
			input = rawInput
		}
		requests = append(requests, captured{
			ContextManagement: body["context_management"],
			Input:             input,
		})
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported input item type: compaction","type":"invalid_request_error","param":"input[0].type"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_compaction_fallback_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})
	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+oldCarrier+`"}]},
			{"role":"user","content":"before latest"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+newCarrier+`"}]},
			{"role":"user","content":"after latest"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].ContextManagement == nil {
		t.Fatalf("first request should include context_management while compaction input is enabled")
	}
	if requests[1].ContextManagement != nil {
		t.Fatalf("retry should omit context_management when compaction input is downgraded: %#v", requests[1].ContextManagement)
	}
	for i, req := range requests {
		if len(req.Input) != 2 {
			t.Fatalf("request %d input count = %d, want 2", i, len(req.Input))
		}
	}
	first := requests[0].Input[0].(map[string]any)
	if first["type"] != "compaction" || first["id"] != "cmp_new" {
		t.Fatalf("first request should send latest compaction item: %#v", requests[0].Input)
	}
	second := requests[1].Input[0].(map[string]any)
	if second["type"] != "reasoning" || second["id"] != "cmp_new" || second["encrypted_content"] != "opaque-new" {
		t.Fatalf("retry should downgrade latest compaction to reasoning carrier: %#v", requests[1].Input)
	}
}

func TestBuildBackendRequestSerializesEmptyReasoningSummaryArray(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-only",
	})

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type": "redacted_thinking",
					"data": carrier,
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if !strings.Contains(string(body), `"summary":[]`) {
		t.Fatalf("reasoning summary missing empty array: %s", string(body))
	}
}

func TestHandleMessagesNonStreamTranslatesResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer rawchat-key" {
			t.Fatalf("authorization header = %q", got)
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_1",
			Output: []OpenAIOutputItem{
				{
					Type: "message",
					Role: "assistant",
					Content: []OpenAIOutputContent{
						{Type: "output_text", Text: "hello from codex"},
					},
				},
				{
					Type:      "function_call",
					ID:        "fc_1",
					CallID:    "toolu_2",
					Name:      "bash",
					Arguments: `{"command":"pwd"}`,
				},
			},
			Usage: OpenAIUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5-codex",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}

	if response.Model != "gpt-5-codex" {
		t.Fatalf("response model = %q, want gpt-5-codex", response.Model)
	}
	if response.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", response.StopReason)
	}
	if len(response.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(response.Content))
	}
	if response.Content[0].Type != "text" || response.Content[0].Text != "hello from codex" {
		t.Fatalf("text block incorrect: %#v", response.Content[0])
	}
	if response.Content[1].Type != "tool_use" || response.Content[1].ID != "toolu_2" {
		t.Fatalf("tool block incorrect: %#v", response.Content[1])
	}
	if response.Usage.InputTokens != 10 || response.Usage.OutputTokens != 5 {
		t.Fatalf("usage incorrect: %#v", response.Usage)
	}
}

func TestHandleMessagesMapsToolResultIsErrorToFunctionCallOutputStatus(t *testing.T) {
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_1",
			Output: []OpenAIOutputItem{},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"false"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"exit status 1"}]}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	input, ok := captured["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("backend input malformed: %#v", captured["input"])
	}
	toolOutput, ok := input[1].(map[string]any)
	if !ok || toolOutput["type"] != "function_call_output" {
		t.Fatalf("tool output malformed: %#v", input[1])
	}
	if got := toolOutput["status"]; got != "incomplete" {
		t.Fatalf("is_error=true mapped status = %#v, want incomplete in backend function_call_output", got)
	}
}

func TestHandleMessagesNonStreamPrefersFinalAnswerPhaseOverCommentary(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp_phase",
			"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"commentary trace","phase":"commentary"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer","phase":"final_answer"}]}
			],
			"usage":{"input_tokens":2,"output_tokens":2}
		}`)
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 1 {
		t.Fatalf("content blocks = %d, want only final_answer phase: %#v", len(response.Content), response.Content)
	}
	if response.Content[0].Type != "text" || response.Content[0].Text != "final answer" {
		t.Fatalf("final_answer phase not preferred: %#v", response.Content[0])
	}
}

func TestHandleMessagesNonStreamTranslatesReasoningResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_reasoning",
			Output: []OpenAIOutputItem{
				{
					Type:             "reasoning",
					ID:               "rs_1",
					EncryptedContent: "opaque-state",
					Summary: []OpenAIReasoningPart{
						{Type: "summary_text", Text: "think briefly"},
					},
				},
				{
					Type: "message",
					Role: "assistant",
					Content: []OpenAIOutputContent{
						{Type: "output_text", Text: "done"},
					},
				},
			},
			Usage: OpenAIUsage{
				InputTokens:  8,
				OutputTokens: 4,
			},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(response.Content))
	}
	if response.Content[0].Type != "thinking" || response.Content[0].Thinking != "think briefly" || !strings.HasPrefix(response.Content[0].Signature, opaqueReasoningPrefix) {
		t.Fatalf("reasoning block incorrect: %#v", response.Content[0])
	}
}

func TestHandleMessagesNonStreamTranslatesCompactionResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_compaction",
			Output: []OpenAIOutputItem{
				{
					ID:               "cmp_1",
					Type:             "compaction",
					EncryptedContent: "opaque-compaction",
				},
			},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 1 {
		t.Fatalf("content block count = %d, want 1", len(response.Content))
	}
	if response.Content[0].Type != "thinking" || response.Content[0].Thinking != defaultThinkingText || !strings.HasPrefix(response.Content[0].Signature, compactionCarrierPrefix) {
		t.Fatalf("compaction block incorrect: %#v", response.Content[0])
	}
}

func TestHandleMessagesNonStreamAggregatesBackendSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"pong\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Text != "pong" {
		t.Fatalf("aggregated content incorrect: %#v", response.Content)
	}
	if response.Usage.InputTokens != 4 || response.Usage.OutputTokens != 2 {
		t.Fatalf("usage incorrect: %#v", response.Usage)
	}
}

func TestHandleMessagesNonStreamAggregatesContentPartAddedText(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.content_part.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"hello from added\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Text != "hello from added" {
		t.Fatalf("aggregated content_part.added text incorrect: %#v", response.Content)
	}
	if response.Usage.InputTokens != 4 || response.Usage.OutputTokens != 3 {
		t.Fatalf("usage incorrect: %#v", response.Usage)
	}
}

func TestHandleMessagesNonStreamAggregatesReasoningContentPartText(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.content_part.added\",\"item_id\":\"rs_1\",\"content_index\":0,\"part\":{\"type\":\"summary_text\",\"text\":\"reasoning from added\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"opaque-state\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response AnthropicMessageResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "thinking" || response.Content[0].Thinking != "reasoning from added" {
		t.Fatalf("aggregated reasoning content incorrect: %#v", response.Content)
	}
	if !strings.HasPrefix(response.Content[0].Signature, opaqueReasoningPrefix) {
		t.Fatalf("reasoning signature missing: %#v", response.Content[0])
	}
}

func TestHandleMessagesStreamTranslatesSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"bash\",\"call_id\":\"toolu_9\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"command\\\":\\\"pwd\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"toolu_9\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"pwd\\\"}\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	for _, snippet := range []string{
		"event: message_start",
		"\"type\":\"tool_use\"",
		"\"type\":\"input_json_delta\"",
		"event: message_stop",
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("stream body missing %q\n%s", snippet, body)
		}
	}
}

func TestHandleMessagesStreamTranslatesReasoningSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.reasoning_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"step one\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"opaque-state\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"opaque-state\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	for _, snippet := range []string{
		"\"type\":\"thinking\"",
		"\"type\":\"thinking_delta\"",
		"\"type\":\"signature_delta\"",
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("stream body missing %q\n%s", snippet, body)
		}
	}
}

func TestHandleMessagesStreamFallsBackToJSONBackendResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_json_stream",
			Output: []OpenAIOutputItem{
				{
					Type: "message",
					Role: "assistant",
					Content: []OpenAIOutputContent{
						{Type: "output_text", Text: "hello from json"},
					},
				},
			},
			Usage: OpenAIUsage{
				InputTokens:  3,
				OutputTokens: 2,
			},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:          backend.URL,
		BackendPath:             "/v1/responses",
		BackendAPIKey:           "rawchat-key",
		DisableStreamingBackend: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	for _, snippet := range []string{
		"event: message_start",
		"\"type\":\"text_delta\"",
		"hello from json",
		"event: message_stop",
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("stream body missing %q\n%s", snippet, body)
		}
	}
}

func TestHandleMessagesStreamDoesNotDuplicateToolArgumentsOnDone(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"max_results\\\":10,\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"\\\"query\\\":\\\"foo\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"max_results\\\":10,\\\"query\\\":\\\"foo\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}

	if got := strings.Count(body, "\"type\":\"input_json_delta\""); got != 2 {
		t.Fatalf("input_json_delta count = %d, want 2\n%s", got, body)
	}
	if strings.Count(body, "\\\"max_results\\\":10,\\\"query\\\":\\\"foo\\\"}") > 0 {
		t.Fatalf("unexpected full duplicate arguments delta in body\n%s", body)
	}
}

func TestHandleMessagesStreamEmitsInitialToolArgumentsFromOutputItemAdded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctftime\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "\"partial_json\":\"{\\\"query\\\":\\\"ctftime\\\"}\"") {
		t.Fatalf("stream body missing initial tool arguments delta\n%s", body)
	}
}

func TestHandleMessagesStreamDoesNotDuplicateInitialToolArgumentsWhenDoneRepeatsThem(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctftime\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctftime\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	if got := strings.Count(body, "\"type\":\"input_json_delta\""); got != 1 {
		t.Fatalf("input_json_delta count = %d, want 1 when output_item.done repeats initial arguments\n%s", got, body)
	}
	if got := strings.Count(body, "\\\"query\\\":\\\"ctftime\\\""); got != 1 {
		t.Fatalf("serialized query argument occurrences = %d, want 1\n%s", got, body)
	}
}

func TestHandleMessagesStreamAllowsLateOutputItemAddedArgumentsAfterToolClosed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"query\\\":\\\"ct\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("late output_item.added arguments after close should be tolerated\n%s", body)
	}
	if got := strings.Count(body, "\"type\":\"input_json_delta\""); got != 2 {
		t.Fatalf("input_json_delta count = %d, want 2 without duplicate late add\n%s", got, body)
	}
}

func TestHandleMessagesStreamAllowsLateOutputItemDoneArgumentsAfterToolClosed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"query\\\":\\\"ct\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("late output_item.done arguments after close should be tolerated\n%s", body)
	}
	if got := strings.Count(body, "\"type\":\"input_json_delta\""); got != 2 {
		t.Fatalf("input_json_delta count = %d, want 2 without duplicate late done\n%s", got, body)
	}
}

func TestHandleMessagesStreamLeavesNonToolOutputItemAddedUnaffected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("non-tool output_item.added should not trigger tool guard\n%s", body)
	}
	if !strings.Contains(body, `"type":"content_block_delta"`) || !strings.Contains(body, `"text":"hello"`) {
		t.Fatalf("non-tool output_item.added stream output missing expected text delta\n%s", body)
	}
}

func TestHandleMessagesStreamEmitsDoneTextWhenNoDeltaArrives(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_1\",\"content_index\":0,\"text\":\"hello from done\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "hello from done") {
		t.Fatalf("stream body missing done text fallback\n%s", body)
	}
}

func TestHandleMessagesStreamMapsIncompleteResponseToMessageStop(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.incomplete\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_stream\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "\"stop_reason\":\"max_tokens\"") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("incomplete response not translated correctly\n%s", body)
	}
}

func TestHandleMessagesStreamMapsFailedResponseToErrorEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_stream\",\"status\":\"failed\",\"error\":{\"message\":\"upstream failed\"}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream failed") {
		t.Fatalf("failed response not translated correctly\n%s", body)
	}
}

func TestHandleMessagesStreamRejectsWhitespaceFloodInFunctionCallArguments(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"                     \"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "excessive whitespace in function_call_arguments.delta") {
		t.Fatalf("whitespace flood not rejected\n%s", body)
	}
}

func TestHandleMessagesStreamEmitsWhitespaceOnlySnapshotExtension(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.content_part.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"hello\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.content_part.done\",\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"hello \"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `"text":"hello"`) || !strings.Contains(body, `"text":" "`) {
		t.Fatalf("whitespace-only snapshot extension should be preserved\n%s", body)
	}
}

func TestHandleMessagesStreamRejectsDeltaAfterToolDone(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "function_call_arguments.delta after done") {
		t.Fatalf("delta-after-done not rejected\n%s", body)
	}
}

func TestHandleMessagesStreamRejectsOversizedToolArguments(t *testing.T) {
	largeDelta := strings.Repeat("a", maxToolArgumentBytes+1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\""+largeDelta+"\"}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "oversized function_call_arguments") {
		t.Fatalf("oversized arguments not rejected\n%s", body)
	}
}

func TestHandleMessagesStreamRejectsConflictingDuplicateToolDone(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"query\\\":\\\"ctf\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"query\\\":\\\"other\\\"}\"}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "duplicate function_call_arguments.done with conflicting arguments") {
		t.Fatalf("conflicting duplicate done not rejected\n%s", body)
	}
}

func TestHandleMessagesStreamRejectsExcessiveEmptyDeltaFlood(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		for i := 0; i < maxToolEmptyDeltaCount+1; i++ {
			_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"\"}\n\n")
		}
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "excessive empty function_call_arguments.delta") {
		t.Fatalf("empty delta flood not rejected\n%s", body)
	}
}

func TestHandleMessagesStreamAllowsLongButBoundedToolArguments(t *testing.T) {
	delta := strings.Repeat("a", 1024)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\""+delta+"\"}\n\n")
		}
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("bounded long arguments should not be rejected\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop") || !strings.Contains(body, "\"type\":\"tool_use\"") {
		t.Fatalf("bounded long arguments should still translate normally\n%s", body)
	}
}

func TestHandleMessagesStreamAllowsFullArgumentsDoneWithoutDoubleCountFalsePositive(t *testing.T) {
	delta := strings.Repeat("a", 80*1024)
	full := delta + delta
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\""+delta+"\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\""+delta+"\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\""+full+"\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("full done arguments identical to accumulated args should not be double-counted\n%s", body)
	}
}

func TestHandleMessagesStreamAllowsDoneArgumentsThatExtendAccumulatedPrefixWithoutDoubleCountFalsePositive(t *testing.T) {
	prefix := strings.Repeat("a", 80*1024)
	suffix := strings.Repeat("b", 24*1024)
	full := prefix + suffix
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\""+prefix+"\"}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\""+full+"\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("full done arguments extending accumulated prefix should not be double-counted\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop") || !strings.Contains(body, "\"type\":\"tool_use\"") {
		t.Fatalf("prefix extension should still translate normally\n%s", body)
	}
}

func TestHandleMessagesNonStreamAllowsBoundarySizedFunctionCallArguments(t *testing.T) {
	allowedArgs := `{"q":"` + strings.Repeat("a", maxToolArgumentBytes-10) + `"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_boundary_args",
			Output: []OpenAIOutputItem{
				{
					Type:      "function_call",
					Name:      "ToolSearch",
					CallID:    "toolu_1",
					Arguments: allowedArgs,
				},
			},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "tool_use") {
		t.Fatalf("boundary-sized non-stream function_call args should still translate: %s", recorder.Body.String())
	}
}

func TestHandleMessagesStreamRejectsOversizedToolArgumentsFromOutputItemDone(t *testing.T) {
	largeArgs := strings.Repeat("a", maxToolArgumentBytes+1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\""+largeArgs+"\"}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "oversized function_call_arguments") {
		t.Fatalf("oversized output_item.done arguments not rejected\n%s", body)
	}
}

func TestHandleMessagesNonStreamRejectsOversizedFunctionCallArguments(t *testing.T) {
	largeArgs := strings.Repeat("a", maxToolArgumentBytes+1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_big_args",
			Output: []OpenAIOutputItem{
				{
					Type:      "function_call",
					Name:      "ToolSearch",
					CallID:    "toolu_1",
					Arguments: largeArgs,
				},
			},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "oversized function_call arguments in non-stream response") {
		t.Fatalf("non-stream oversized function_call args not rejected: %s", recorder.Body.String())
	}
}

func TestHandleMessagesStreamUsesOutputItemDoneForFunctionCallFallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"ToolSearch\",\"call_id\":\"toolu_1\",\"arguments\":\"{\\\"query\\\":\\\"ctftime\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "\"type\":\"tool_use\"") || !strings.Contains(body, "\\\"query\\\":\\\"ctftime\\\"") {
		t.Fatalf("output_item.done function_call fallback missing\n%s", body)
	}
}

func TestHandleMessagesStreamUsesOutputItemDoneForCompactionFallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"cmp_1\",\"type\":\"compaction\",\"encrypted_content\":\"opaque-compaction\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "\"type\":\"thinking\"") || !strings.Contains(body, "cm1#opaque-compaction@cmp_1") {
		t.Fatalf("output_item.done compaction fallback missing\n%s", body)
	}
}

func TestHandleMessagesStreamUsesOutputItemDoneForMessageFallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello fallback\"}]}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "hello fallback") {
		t.Fatalf("output_item.done message fallback missing\n%s", body)
	}
}

func TestHandleModelsIncludesCapabilities(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	data, ok := response["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("models data malformed: %#v", response["data"])
	}
	model, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("model entry malformed: %#v", data[0])
	}
	if model["id"] != "gpt-5.4" {
		t.Fatalf("model id = %#v, want gpt-5.4", model["id"])
	}
	if _, ok := model["capabilities"].(map[string]any); !ok {
		t.Fatalf("capabilities missing: %#v", model)
	}
	if _, ok := model["supported_endpoints"].([]any); !ok {
		t.Fatalf("supported_endpoints missing: %#v", model)
	}
}

func TestHandleModelsPassesThroughBackendModelsWhenAvailable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		writeJSONWithStatus(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"id":                  "gpt-5.4",
					"supported_endpoints": []string{"/responses", "/v1/messages"},
					"capabilities": map[string]any{
						"family": "gpt-5",
					},
				},
			},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(recorder, request)

	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	data := response["data"].([]any)
	model := data[0].(map[string]any)
	supported := model["supported_endpoints"].([]any)
	if len(supported) != 2 {
		t.Fatalf("supported_endpoints = %#v, want passthrough values", supported)
	}
}

func TestHandleMessagesUsesModelProfilesToAvoidFirstUnsupportedRetry(t *testing.T) {
	type captured struct {
		Path string
		Body map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			requests = append(requests, captured{Path: r.URL.Path})
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"adaptive_thinking":  false,
								"streaming":          false,
								"structured_outputs": false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_model_profile_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
		EnablePhaseCommentary:     true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"output_config":{"effort":"max"},
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"inspect","input":{"path":"report.json"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"stdout"},{"type":"json","json":{"severity":"high"}}]}]}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2 (models + single responses request)", len(requests))
	}
	body := requests[1].Body
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning should be pre-disabled by model profile: %#v", body)
	}
	if _, ok := body["include"]; ok {
		t.Fatalf("reasoning include should be pre-disabled by model profile: %#v", body)
	}
	if _, ok := body["stream"]; ok {
		t.Fatalf("stream should be pre-disabled by model profile: %#v", body)
	}
	output := body["input"].([]any)[1].(map[string]any)["output"]
	if _, ok := output.(string); !ok {
		t.Fatalf("structured output should be flattened by model profile: %#v", output)
	}
}

func TestHandleMessagesUsesResponsesFamilyPresetToPreDisableBridgeExtensions(t *testing.T) {
	type captured struct {
		Path string
		Body map[string]any
	}
	var requests []captured
	reasoningCarrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
	})
	compactionCarrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			requests = append(requests, captured{Path: r.URL.Path})
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"vendor":              "openai",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"family": "gpt-5",
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_family_preset_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
		EnablePhaseCommentary:     true,
	})

	bodyBytes, err := json.Marshal(AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "I will inspect this."},
				map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": reasoningCarrier},
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": compactionCarrier},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal anthropic request: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(bodyBytes))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2 (models + single responses request)", len(requests))
	}
	body := requests[1].Body
	if _, ok := body["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be pre-disabled by responses preset: %#v", body)
	}
	if _, ok := body["include"]; ok {
		t.Fatalf("reasoning include should be pre-disabled by responses preset: %#v", body)
	}
	if _, ok := body["context_management"]; ok {
		t.Fatalf("context_management should be pre-disabled by responses preset: %#v", body)
	}
	input := body["input"].([]any)
	hasCompaction := false
	hasPhase := false
	for _, item := range input {
		typed := item.(map[string]any)
		if typed["type"] == "compaction" {
			hasCompaction = true
		}
		if phase, ok := typed["phase"].(string); ok && phase != "" {
			hasPhase = true
		}
	}
	if hasCompaction {
		t.Fatalf("compaction input should be pre-disabled by responses preset: %#v", input)
	}
	if hasPhase {
		t.Fatalf("phase should be pre-disabled by responses preset: %#v", input)
	}
}

func TestHandleMessagesResponsesFamilyPresetAllowsPeriodicProbe(t *testing.T) {
	currentTime := time.Date(2026, 4, 20, 20, 0, 0, 0, time.UTC)
	type captured struct{ Body map[string]any }
	var requests []captured
	reasoningCarrier := encodeReasoningCarrier(OpenAIOutputItem{ID: "rs_1", Type: "reasoning", EncryptedContent: "opaque-reasoning"})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{ID: "resp_ok", Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}}, Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key", BackendModel: "gpt-5.4", EnableModelCapabilityInit: true, CapabilityReprobeTTL: 30 * time.Minute})
	proxy.now = func() time.Time { return currentTime }
	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"vendor":              "openai",
			"supported_endpoints": []string{"/responses"},
			"capabilities":        map[string]any{"family": "gpt-5"},
		}),
	})

	makeReq := func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"thinking","thinking":"brief reasoning","signature":"`+reasoningCarrier+`"}]},{"role":"user","content":"continue"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Claude-Code-Session-Id", "session-1")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	makeReq()
	makeReq()
	currentTime = currentTime.Add(31 * time.Minute)
	makeReq()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if _, ok := requests[0].Body["include"]; ok {
		t.Fatalf("first request should honor preset disable: %#v", requests[0].Body)
	}
	if _, ok := requests[1].Body["include"]; ok {
		t.Fatalf("second request within TTL should still honor preset disable: %#v", requests[1].Body)
	}
	if _, ok := requests[2].Body["include"]; !ok {
		t.Fatalf("third request after TTL should reprobe reasoning include: %#v", requests[2].Body)
	}
}

func TestBuildBackendRequestDoesNotApplyResponsesPresetForMixedEndpointFamily(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
		EnablePhaseCommentary:     true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"vendor":              "openai",
			"supported_endpoints": []string{"/responses", "/v1/messages"},
			"capabilities": map[string]any{
				"family":   "gpt-5",
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
		}),
	})

	reasoningCarrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
	})
	compactionCarrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")
	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "I will inspect this."},
				map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": reasoningCarrier},
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": compactionCarrier},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.PromptCacheKey != "session-1" {
		t.Fatalf("prompt_cache_key = %q, want session-1 when endpoint family is mixed", payload.PromptCacheKey)
	}
	if len(payload.Include) != 1 || payload.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("reasoning include should remain enabled for mixed endpoint family: %#v", payload.Include)
	}
	if len(payload.ContextManagement) != 1 || payload.ContextManagement[0].Type != "compaction" {
		t.Fatalf("context_management should remain enabled for mixed endpoint family: %#v", payload.ContextManagement)
	}
	hasCompaction := false
	for _, item := range payload.Input {
		if item.Type == "compaction" {
			hasCompaction = true
		}
	}
	if !hasCompaction {
		t.Fatalf("compaction input should remain enabled for mixed endpoint family: %#v", payload.Input)
	}
}

func TestHandleMessagesIgnoresFetchedModelProfilesWhenInitDisabled(t *testing.T) {
	type captured struct {
		Path string
		Body map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"adaptive_thinking":  false,
								"streaming":          false,
								"structured_outputs": false,
								"phase":              false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_flag_disabled_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		BackendModel:          "gpt-5.4",
		EnablePhaseCommentary: true,
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"output_config":{"effort":"max"},
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"I will inspect this."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}]},
			{"role":"user","content":"continue"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	body := requests[0].Body
	if _, ok := body["stream"]; !ok {
		t.Fatalf("stream should remain enabled when capability init disabled: %#v", body)
	}
	if _, ok := body["reasoning"]; !ok {
		t.Fatalf("reasoning should remain enabled when capability init disabled: %#v", body)
	}
	firstInput := body["input"].([]any)[0].(map[string]any)
	if firstInput["phase"] != "commentary" {
		t.Fatalf("phase should remain enabled when capability init disabled: %#v", firstInput)
	}
}

func TestHandleMessagesFallsBackToPreferredModelWhenFetchedPromptLimitTooLow(t *testing.T) {
	var responseModels []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-large",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{
								"max_prompt_tokens": 4096,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{
								"max_prompt_tokens": 10,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			responseModels = append(responseModels, asString(body["model"]))
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_prompt_limit_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"`+strings.Repeat("long prompt ", 20)+`"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(responseModels) != 1 {
		t.Fatalf("responses request count = %d, want 1 after prefetched profiles", len(responseModels))
	}
	if responseModels[0] != "gpt-5.4-large" {
		t.Fatalf("responses model = %q, want fallback preferred model gpt-5.4-large", responseModels[0])
	}
}

func TestHandleMessagesIgnoresFetchedPromptLimitFallbackWhenInitDisabled(t *testing.T) {
	type captured struct {
		Path  string
		Model string
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-large",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{
								"max_prompt_tokens": 4096,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"limits": map[string]any{
								"max_prompt_tokens": 10,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Model: asString(body["model"])})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_prompt_limit_disabled_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
		BackendModel:   "gpt-5.4",
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"`+strings.Repeat("long prompt ", 20)+`"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	if requests[0].Model != "gpt-5.4" {
		t.Fatalf("responses model = %q, want pinned model gpt-5.4 when init disabled", requests[0].Model)
	}
}

func TestHandleMessagesFallsBackToToolCapableModelWhenFetchedProfileDisablesToolCalls(t *testing.T) {
	type captured struct {
		Path  string
		Model string
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			requests = append(requests, captured{Path: r.URL.Path})
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-tools",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": true,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Model: asString(body["model"])})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_tool_calls_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2 (models + single responses request)", len(requests))
	}
	if requests[1].Model != "gpt-5.4-tools" {
		t.Fatalf("responses model = %q, want fallback tool-capable model gpt-5.4-tools", requests[1].Model)
	}
}

func TestHandleMessagesSetsParallelToolCallsWhenFetchedProfileAllows(t *testing.T) {
	var requests []OpenAIResponsesRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls":          true,
								"parallel_tool_calls": true,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body OpenAIResponsesRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, body)
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_parallel_tool_calls_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	if requests[0].ParallelToolCalls == nil || !*requests[0].ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %#v, want true", requests[0].ParallelToolCalls)
	}
}

func TestHandleMessagesCapsReasoningEffortWhenFetchedMaxThinkingBudgetTooLow(t *testing.T) {
	var requests []OpenAIResponsesRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"adaptive_thinking":   true,
								"max_thinking_budget": 1500,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body OpenAIResponsesRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, body)
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_thinking_budget_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"preserve this user content"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	if requests[0].Reasoning == nil || requests[0].Reasoning.Effort != "low" {
		t.Fatalf("reasoning = %#v, want low after fetched max thinking budget cap", requests[0].Reasoning)
	}
}

func TestHandleMessagesFallsBackToThinkingBudgetCompatibleModelWhenFetchedMinThinkingBudgetMismatches(t *testing.T) {
	var requests []OpenAIResponsesRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-flex",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"adaptive_thinking":   true,
								"min_thinking_budget": 512,
								"max_thinking_budget": 8192,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"adaptive_thinking":   true,
								"min_thinking_budget": 4096,
								"max_thinking_budget": 8192,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body OpenAIResponsesRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, body)
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_budget_fallback_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	modelsRecorder := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.Handler().ServeHTTP(modelsRecorder, modelsRequest)
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	if requests[0].Model != "gpt-5.4-flex" {
		t.Fatalf("responses model = %q, want thinking-budget-compatible fallback gpt-5.4-flex", requests[0].Model)
	}
}

func TestHandleMessagesKeepsToolsWhenFetchedProfilesCannotHandleToolCalls(t *testing.T) {
	type captured struct {
		Model string
		Tools []map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-tools-fallback",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body struct {
				Model string           `json:"model"`
				Tools []map[string]any `json:"tools"`
				Input []map[string]any `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Model: body.Model, Tools: body.Tools})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_tools_preserved",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1 after models prefetch", len(requests))
	}
	if requests[0].Model != "gpt-5.4" {
		t.Fatalf("responses model = %q, want current model when no fallback supports tools", requests[0].Model)
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0]["name"] != "Read" {
		t.Fatalf("tools should remain intact when fallback fails: %#v", requests[0].Tools)
	}
}

func TestHandleMessagesKeepsCurrentModelWhenFetchedToolCallSupportIsUnknown(t *testing.T) {
	type captured struct {
		Model string
		Tools []map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-tools",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": true,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{},
						},
					},
				},
			})
		case "/v1/responses":
			var body struct {
				Model string           `json:"model"`
				Tools []map[string]any `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Model: body.Model, Tools: body.Tools})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_tools_unknown_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1 after models prefetch", len(requests))
	}
	if requests[0].Model != "gpt-5.4" {
		t.Fatalf("responses model = %q, want current model when tool_calls support is unknown", requests[0].Model)
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0]["name"] != "Read" {
		t.Fatalf("tools should remain intact when support is unknown: %#v", requests[0].Tools)
	}
}

func TestHandleMessagesPromptLimitAndToolsFallbackDoesNotPolluteCurrentModelScope(t *testing.T) {
	type captured struct {
		Model string
		Tools []map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-tools-large",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": true,
							},
							"limits": map[string]any{
								"max_prompt_tokens": 4096,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
							"limits": map[string]any{
								"max_prompt_tokens": 10,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body struct {
				Model string           `json:"model"`
				Tools []map[string]any `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Model: body.Model, Tools: body.Tools})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_prompt_tools_scope_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	longRecorder := httptest.NewRecorder()
	longRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"`+strings.Repeat("long prompt ", 20)+`"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	longRequest.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(longRecorder, longRequest)

	if longRecorder.Code != http.StatusOK {
		t.Fatalf("long request status = %d, body = %s", longRecorder.Code, longRecorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count after long request = %d, want 1", len(requests))
	}
	if requests[0].Model != "gpt-5.4-tools-large" {
		t.Fatalf("long request should fallback to tool-capable prompt-safe model: %#v", requests[0])
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0]["name"] != "Read" {
		t.Fatalf("long request tools lost during fallback: %#v", requests[0].Tools)
	}

	requests = nil
	shortRecorder := httptest.NewRecorder()
	shortRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"short"}]
	}`))
	shortRequest.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(shortRecorder, shortRequest)

	if shortRecorder.Code != http.StatusOK {
		t.Fatalf("short request status = %d, body = %s", shortRecorder.Code, shortRecorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count after short request = %d, want 1", len(requests))
	}
	if requests[0].Model != "gpt-5.4" {
		t.Fatalf("prompt-limit + tools fallback should not pollute current model scope: %#v", requests[0])
	}
	if len(requests[0].Tools) != 0 {
		t.Fatalf("short request should remain tool-free: %#v", requests[0].Tools)
	}
}

func TestHandleMessagesKeepsToolsWhenFetchedProfilesDoNotSupportToolCalls(t *testing.T) {
	type captured struct {
		Path  string
		Model string
		Tools []any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			requests = append(requests, captured{Path: r.URL.Path})
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-fallback",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			var tools []any
			if rawTools, ok := body["tools"].([]any); ok {
				tools = rawTools
			}
			requests = append(requests, captured{Path: r.URL.Path, Model: asString(body["model"]), Tools: tools})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_keep_tools_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2 (models + single responses request)", len(requests))
	}
	if requests[1].Model != "gpt-5.4" {
		t.Fatalf("responses model = %q, want current model when no fallback supports tools", requests[1].Model)
	}
	if len(requests[1].Tools) != 1 {
		t.Fatalf("tools should remain intact when no fallback supports tools: %#v", requests[1].Tools)
	}
}

func TestHandleMessagesKeepsCurrentModelWhenFetchedProfileToolCallsUnknown(t *testing.T) {
	type captured struct {
		Path  string
		Model string
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			requests = append(requests, captured{Path: r.URL.Path})
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-fallback",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": true,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Model: asString(body["model"])})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_tool_unknown_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2 (models + single responses request)", len(requests))
	}
	if requests[1].Model != "gpt-5.4" {
		t.Fatalf("responses model = %q, want current model when tool_calls support is unknown", requests[1].Model)
	}
}

func TestHandleMessagesKeepsToolsWhenFetchedProfilesOfferNoToolCapableFallback(t *testing.T) {
	type captured struct {
		Path string
		Body map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4-no-tools",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"tool_calls": false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			requests = append(requests, captured{Path: r.URL.Path, Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_tool_calls_preserved",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"keep my tools intact"}],
		"tools":[{"name":"Read","description":"read file","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("responses request count = %d, want 1", len(requests))
	}
	if got := asString(requests[0].Body["model"]); got != "gpt-5.4" {
		t.Fatalf("responses model = %q, want current model when fetched profiles offer no tool-capable fallback", got)
	}
	tools, _ := requests[0].Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools should remain intact when fetched fallback fails: %#v", requests[0].Body["tools"])
	}
	input, _ := requests[0].Body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input should remain intact when fetched fallback fails: %#v", requests[0].Body["input"])
	}
	item, _ := input[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("prompt content should remain intact when fetched fallback fails: %#v", item["content"])
	}
	part, _ := content[0].(map[string]any)
	if got := asString(part["text"]); got != "keep my tools intact" {
		t.Fatalf("prompt text = %q, want original prompt", got)
	}
	if marshaled, err := json.Marshal(requests[0].Body["input"]); err != nil {
		t.Fatalf("marshal input: %v", err)
	} else if strings.Contains(string(marshaled), "Respond with TEXT ONLY") {
		t.Fatalf("prompt should not be rewritten to text-only guard: %s", marshaled)
	}
}

func TestHandleMessagesRoutesWarmupRequestsToConfiguredWarmupModel(t *testing.T) {
	var models []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		models = append(models, asString(body["model"]))
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_warmup_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:     backend.URL,
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "rawchat-key",
		BackendWarmupModel: "gpt-5.4-mini",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", "warmup-beta")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(models) != 1 || models[0] != "gpt-5.4-mini" {
		t.Fatalf("warmup model routing incorrect: %#v", models)
	}
}

func TestHandleMessagesDoesNotRouteWarmupModelForCompactRequests(t *testing.T) {
	var models []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		models = append(models, asString(body["model"]))
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_compact_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:     backend.URL,
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "rawchat-key",
		BackendModel:       "gpt-5.4",
		BackendWarmupModel: "gpt-5.4-mini",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nYour task is to create a detailed summary of the conversation so far\n\nPending Tasks:\n\nCurrent Work:"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "warmup-beta")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(models) != 1 || models[0] != "gpt-5.4" {
		t.Fatalf("compact request should not use warmup model: %#v", models)
	}
}

func TestHandleMessagesDoesNotRouteWarmupModelWhenBackendModelIsFixed(t *testing.T) {
	var models []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		models = append(models, asString(body["model"]))
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_fixed_model_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:     backend.URL,
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "rawchat-key",
		BackendModel:       "gpt-5.4",
		BackendWarmupModel: "gpt-5.4-mini",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "warmup-beta")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(models) != 1 || models[0] != "gpt-5.4" {
		t.Fatalf("fixed backend model should win over warmup routing: %#v", models)
	}
}

func TestWarmupCapabilityDowngradeDoesNotPolluteNormalModel(t *testing.T) {
	type captured struct {
		Model          string
		PromptCacheKey any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, captured{
			Model:          asString(body["model"]),
			PromptCacheKey: body["prompt_cache_key"],
		})
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: prompt_cache_key","type":"invalid_request_error","param":"prompt_cache_key"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_scope_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:     backend.URL,
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "rawchat-key",
		BackendWarmupModel: "gpt-5.4-mini",
	})

	warmupRecorder := httptest.NewRecorder()
	warmupRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	warmupRequest.Header.Set("Content-Type", "application/json")
	warmupRequest.Header.Set("anthropic-beta", "warmup-beta")
	warmupRequest.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(warmupRecorder, warmupRequest)

	if warmupRecorder.Code != http.StatusOK {
		t.Fatalf("warmup status = %d, body = %s", warmupRecorder.Code, warmupRecorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("warmup request count = %d, want 2 (retry expected)", len(requests))
	}
	if requests[0].Model != "gpt-5.4-mini" || requests[1].Model != "gpt-5.4-mini" {
		t.Fatalf("warmup should stay on mini model: %#v", requests)
	}
	if requests[0].PromptCacheKey != "session-1" {
		t.Fatalf("first warmup request missing prompt cache key: %#v", requests[0])
	}
	if requests[1].PromptCacheKey != nil {
		t.Fatalf("second warmup retry should drop prompt cache key: %#v", requests[1])
	}

	normalRecorder := httptest.NewRecorder()
	normalRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"normal request"}]
	}`))
	normalRequest.Header.Set("Content-Type", "application/json")
	normalRequest.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(normalRecorder, normalRequest)

	if normalRecorder.Code != http.StatusOK {
		t.Fatalf("normal status = %d, body = %s", normalRecorder.Code, normalRecorder.Body.String())
	}
	if len(requests) != 3 {
		t.Fatalf("normal request count = %d, want 3", len(requests))
	}
	if requests[2].Model != "gpt-5.4" {
		t.Fatalf("normal request should use its own model: %#v", requests[2])
	}
	if requests[2].PromptCacheKey != "session-1" {
		t.Fatalf("warmup downgrade should not pollute normal model scope: %#v", requests[2])
	}
}

func TestOptionsForRequestUsesSeededModelCapabilitiesForInitialFlags(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		EnableModelCapabilityInit: true,
		EnablePhaseCommentary:     true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"streaming":          false,
					"adaptive_thinking":  false,
					"structured_outputs": false,
					"phase":              false,
				},
			},
		}),
	})

	opts := proxy.optionsForRequest(AnthropicMessagesRequest{
		Model:  "gpt-5.4",
		Stream: true,
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "I will inspect this."},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				},
			},
		},
	}, nil)

	if opts.EnableBackendStreaming {
		t.Fatalf("EnableBackendStreaming = true, want false after seeded model capabilities")
	}
	if opts.EnableReasoning {
		t.Fatalf("EnableReasoning = true, want false after seeded model capabilities")
	}
	if opts.EnablePhaseCommentary {
		t.Fatalf("EnablePhaseCommentary = true, want false after seeded model capabilities")
	}
	if opts.PreserveStructuredOutput {
		t.Fatalf("PreserveStructuredOutput = true, want false after seeded model capabilities")
	}
}

func TestOptionsForRequestIgnoresSeededModelCapabilitiesWhenInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		EnablePhaseCommentary: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"streaming":          false,
					"adaptive_thinking":  false,
					"structured_outputs": false,
					"phase":              false,
				},
			},
		}),
	})

	opts := proxy.optionsForRequest(AnthropicMessagesRequest{
		Model:  "gpt-5.4",
		Stream: true,
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "I will inspect this."},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				},
			},
		},
	}, nil)

	if !opts.EnableBackendStreaming || !opts.EnableReasoning || !opts.EnablePhaseCommentary || !opts.PreserveStructuredOutput {
		t.Fatalf("seeded profiles should be ignored when init disabled: %+v", opts)
	}
}

func TestOptionsForRequestWithoutSeededCapabilitiesKeepsOrdinaryFlagsEnabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		EnablePhaseCommentary: true,
	})

	opts := proxy.optionsForRequest(AnthropicMessagesRequest{
		Model:  "gpt-5.4",
		Stream: true,
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "I will inspect this."},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				},
			},
		},
	}, nil)

	if !opts.EnableBackendStreaming {
		t.Fatalf("EnableBackendStreaming = false, want true for ordinary request")
	}
	if !opts.EnableReasoning {
		t.Fatalf("EnableReasoning = false, want true for ordinary request")
	}
	if !opts.EnablePhaseCommentary {
		t.Fatalf("EnablePhaseCommentary = false, want true for ordinary request")
	}
	if !opts.PreserveStructuredOutput {
		t.Fatalf("PreserveStructuredOutput = false, want true for ordinary request")
	}
}

func TestHandleMessagesAvoidsFirstRetryWhenSeededModelCapabilitiesDisableUnsupportedFields(t *testing.T) {
	type capturedRequest struct {
		Path string
		Body map[string]any
	}

	var responseRequests []capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/v1/responses"},
						"capabilities": map[string]any{
							"supports": map[string]any{
								"streaming":          false,
								"adaptive_thinking":  false,
								"structured_outputs": false,
								"phase":              false,
							},
						},
					},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			responseRequests = append(responseRequests, capturedRequest{Path: r.URL.Path, Body: body})
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_seeded_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		EnableModelCapabilityInit: true,
		EnablePhaseCommentary:     true,
	})
	if _, ok := proxy.fetchBackendModels(); !ok {
		t.Fatalf("fetchBackendModels() = false, want true")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"stream":true,
		"output_config":{"effort":"max"},
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"I will inspect this."},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"stdout"},
					{"type":"json","json":{"severity":"high"}}
				]}
			]}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(responseRequests) != 1 {
		t.Fatalf("response request count = %d, want 1", len(responseRequests))
	}

	body := responseRequests[0].Body
	if _, ok := body["stream"]; ok {
		t.Fatalf("stream still present in first request: %#v", body)
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning still present in first request: %#v", body)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) < 3 {
		t.Fatalf("input malformed: %#v", body["input"])
	}
	if phase, ok := input[0].(map[string]any)["phase"]; ok && strings.TrimSpace(asString(phase)) != "" {
		t.Fatalf("phase still present in first request: %#v", input[0])
	}
	if _, ok := input[2].(map[string]any)["output"].([]any); ok {
		t.Fatalf("structured tool output still preserved in first request: %#v", input[2])
	}
}

func TestBuildBackendRequestRoutesWarmupNoToolsAnthropicBetaToWarmupModel(t *testing.T) {
	cfg := Config{
		BackendBaseURL:     "https://example.com/codex",
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "test-key",
		BackendWarmupModel: "gpt-5.4-mini",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, http.Header{"Anthropic-Beta": []string{"client-tools-2025-01-01"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Model != cfg.BackendWarmupModel {
		t.Fatalf("model = %q, want %q", payload.Model, cfg.BackendWarmupModel)
	}
}

func TestBuildBackendRequestDoesNotUseWarmupModelForOrdinaryRequests(t *testing.T) {
	cfg := Config{
		BackendBaseURL:     "https://example.com/codex",
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "test-key",
		BackendWarmupModel: "gpt-5.4-mini",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Tools: []AnthropicTool{
			{Name: "bash", Description: "run shell", InputSchema: map[string]any{"type": "object"}},
		},
	}, http.Header{"anthropic-beta": []string{"client-tools-2025-01-01"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Model == cfg.BackendWarmupModel {
		t.Fatalf("ordinary request unexpectedly routed to warmup model %q", payload.Model)
	}
}

func TestBuildBackendRequestDoesNotUseWarmupModelForMultiTurnTextHistory(t *testing.T) {
	cfg := Config{
		BackendBaseURL:     "https://example.com/codex",
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "test-key",
		BackendWarmupModel: "gpt-5.4-mini",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "gpt-5.4",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "first turn"},
			{Role: "assistant", Content: "ack"},
			{Role: "user", Content: "follow up"},
		},
	}, http.Header{"anthropic-beta": []string{"client-tools-2025-01-01"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("multi-turn text request should stay on main model, got %q", payload.Model)
	}
}

func TestBuildBackendRequestDoesNotUseWarmupModelWhenCompactionCarrierPresent(t *testing.T) {
	cfg := Config{
		BackendBaseURL:     "https://example.com/codex",
		BackendPath:        "/v1/responses",
		BackendAPIKey:      "test-key",
		BackendWarmupModel: "gpt-5.4-mini",
	}
	carrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "gpt-5.4",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": defaultThinkingText, "signature": carrier},
			}},
			{Role: "user", Content: "continue"},
		},
	}, http.Header{"anthropic-beta": []string{"client-tools-2025-01-01"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("compaction-carrier request should stay on main model, got %q", payload.Model)
	}
}

func TestBuildBackendRequestPrimesProfilesBeforeUsingConfiguredWarmupModel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					normalizeModelDescriptor(map[string]any{
						"id":                  "gpt-5.4",
						"supported_endpoints": []string{"/v1/responses"},
						"capabilities":        map[string]any{"supports": map[string]any{"streaming": true}},
					}),
					normalizeModelDescriptor(map[string]any{
						"id":                  "gpt-5.4-mini",
						"supported_endpoints": []string{"/v1/chat/completions"},
						"capabilities":        map[string]any{"supports": map[string]any{"streaming": true}},
					}),
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendWarmupModel:        "gpt-5.4-mini",
		EnableModelCapabilityInit: true,
	})

	req, err := proxy.buildBackendRequest(context.Background(), AnthropicMessagesRequest{
		Model:    "gpt-5.4",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, http.Header{"anthropic-beta": []string{"client-tools-2025-01-01"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("warmup request should fall back to main model when configured warmup profile is incompatible, got %q", payload.Model)
	}
}

func TestClassifyCapabilityFailureRecognizesProviderStyleVariants(t *testing.T) {
	proxy := New(Config{})
	cases := []struct {
		name    string
		status  int
		body    string
		opts    backendRequestOptions
		payload OpenAIResponsesRequest
		want    string
	}{
		{
			name:    "metadata unknown parameter with param field",
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"Unknown parameter: metadata","type":"invalid_request_error","param":"metadata"}}`,
			payload: OpenAIResponsesRequest{Metadata: map[string]string{"trace": "abc"}},
			want:    "metadata",
		},
		{
			name:    "stream unrecognized request argument",
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"Unrecognized request argument supplied: stream","type":"invalid_request_error"}}`,
			payload: OpenAIResponsesRequest{Stream: true},
			want:    "stream",
		},
		{
			name:    "reasoning param only with whitespace",
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"This field is not supported by this backend","type":"invalid_request_error","param": "reasoning"}}`,
			payload: OpenAIResponsesRequest{Reasoning: &OpenAIReasoning{Effort: "high"}},
			want:    "reasoning",
		},
		{
			name:    "prompt cache key unexpected field with supported status",
			status:  http.StatusUnprocessableEntity,
			body:    `{"error":{"message":"Additional properties are not allowed ('prompt_cache_key' was unexpected)","type":"invalid_request_error"}}`,
			payload: OpenAIResponsesRequest{PromptCacheKey: "session-1"},
			want:    "prompt_cache_key",
		},
		{
			name:    "context management unknown parameter on 501",
			status:  http.StatusNotImplemented,
			body:    `{"error":{"message":"Unknown parameter: context_management","type":"invalid_request_error"}}`,
			payload: OpenAIResponsesRequest{ContextManagement: []OpenAIContextManagementItem{{Type: "compaction", CompactThreshold: 1000}}},
			want:    "context_management",
		},
		{
			name:   "model not found on 404",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"Model not found","type":"invalid_request_error"}}`,
			opts: backendRequestOptions{
				UsesRequestModelPassthrough: true,
			},
			want: "model",
		},
		{
			name:   "model_not_found code style on 404",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"Requested resource does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			opts: backendRequestOptions{
				UsesRequestModelPassthrough: true,
			},
			want: "model",
		},
		{
			name:    "compaction input unknown type on 422",
			status:  http.StatusUnprocessableEntity,
			body:    `{"error":{"message":"Unknown input item type: compaction","type":"invalid_request_error","param":"input[0].type"}}`,
			payload: OpenAIResponsesRequest{Input: []OpenAIInputItem{{Type: "compaction", ID: "cmp_123", Summary: []OpenAIReasoningPart{{Type: "summary_text", Text: "summary"}}, EncryptedContent: "opaque"}}},
			want:    "compaction_input",
		},
		{
			name:    "compaction input unrecognized type on 422",
			status:  http.StatusUnprocessableEntity,
			body:    `{"error":{"message":"Unrecognized input item type: compaction","type":"invalid_request_error","param":"input[0].type"}}`,
			payload: OpenAIResponsesRequest{Input: []OpenAIInputItem{{Type: "compaction", ID: "cmp_123", Summary: []OpenAIReasoningPart{{Type: "summary_text", Text: "summary"}}, EncryptedContent: "opaque"}}},
			want:    "compaction_input",
		},
		{
			name:    "compaction input invalid enum with indexed param",
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"Invalid value for input[0].type: expected one of message, reasoning","type":"invalid_request_error","param":"input[0].type"}}`,
			payload: OpenAIResponsesRequest{Input: []OpenAIInputItem{{Type: "compaction", ID: "cmp_123", EncryptedContent: "opaque"}}},
			want:    "compaction_input",
		},
		{
			name:    "unsupported status does not classify",
			status:  http.StatusInternalServerError,
			body:    `{"error":{"message":"Unknown parameter: metadata","type":"invalid_request_error","param":"metadata"}}`,
			payload: OpenAIResponsesRequest{Metadata: map[string]string{"trace": "abc"}},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.classifyCapabilityFailure(tc.status, []byte(tc.body), tc.opts, tc.payload)
			if got != tc.want {
				t.Fatalf("classifyCapabilityFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandleMessagesRetriesWithoutMetadataAfterUnsupportedParameterMetadata(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_ok",
			Output: []OpenAIOutputItem{{
				Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}},
			}},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if _, ok := requests[0]["metadata"]; !ok {
		t.Fatalf("first request missing metadata: %#v", requests[0])
	}
	if _, ok := requests[1]["metadata"]; ok {
		t.Fatalf("second request still has metadata: %#v", requests[1])
	}

	// sticky disable
	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"def"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)
	if len(requests) != 3 {
		t.Fatalf("sticky disable request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["metadata"]; ok {
		t.Fatalf("third request still has metadata after disable: %#v", requests[2])
	}
}

func TestHandleMessagesRetriesWithoutMetadataAfterUnknownParameterMetadata(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if _, ok := requests[0]["metadata"]; !ok {
		t.Fatalf("first request missing metadata: %#v", requests[0])
	}
	if _, ok := requests[1]["metadata"]; ok {
		t.Fatalf("second request still has metadata after provider-style downgrade: %#v", requests[1])
	}
}

func TestHandleMessagesRetriesWithoutMetadataWhenUserMetadataForwardingDisabled(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_ok",
			Output: []OpenAIOutputItem{{
				Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}},
			}},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:                backend.URL,
		BackendPath:                   "/v1/responses",
		BackendAPIKey:                 "rawchat-key",
		EnableBackendMetadata:         true,
		DisableUserMetadataForwarding: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	firstMetadata, ok := requests[0]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("first request missing metadata: %#v", requests[0])
	}
	if _, ok := firstMetadata["trace"]; ok {
		t.Fatalf("user metadata should be omitted when forwarding disabled: %#v", firstMetadata)
	}
	if _, ok := firstMetadata["claude_code_root_session_id"]; !ok {
		t.Fatalf("bridge metadata should remain when forwarding disabled: %#v", firstMetadata)
	}
	if _, ok := requests[1]["metadata"]; ok {
		t.Fatalf("second request still has metadata: %#v", requests[1])
	}

	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"def"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	request2.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(recorder2, request2)
	if len(requests) != 3 {
		t.Fatalf("sticky disable request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["metadata"]; ok {
		t.Fatalf("third request still has metadata after disable: %#v", requests[2])
	}
}

func TestHandleMessagesDoesNotSendContinuityMetadataWhenDisabled(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		EnableBackendMetadata:     true,
		DisableContinuityMetadata: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "session-1")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	metadata, ok := requests[0]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %#v", requests[0])
	}
	if metadata["trace"] != "abc" {
		t.Fatalf("user metadata should remain when continuity disabled: %#v", metadata)
	}
	for key := range metadata {
		if strings.HasPrefix(key, "claude_code_") {
			t.Fatalf("continuity metadata should be absent when disabled: %#v", metadata)
		}
	}
}

func TestHandleMessagesDoesNotReintroduceContinuityMetadataAfterMetadataTTLReprobeWhenDisabled(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC)
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		EnableBackendMetadata:     true,
		DisableContinuityMetadata: true,
		CapabilityReprobeTTL:      30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }

	makeRequest := func(trace string) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"claude-sonnet-4-5",
			"metadata":{"trace":"`+trace+`"},
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Claude-Code-Session-Id", "session-1")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	makeRequest("abc")
	makeRequest("def")
	currentTime = currentTime.Add(31 * time.Minute)
	makeRequest("ghi")

	if len(requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(requests))
	}
	if _, ok := requests[1]["metadata"]; ok {
		t.Fatalf("second request should drop metadata after unsupported retry: %#v", requests[1])
	}
	reprobed, ok := requests[3]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("reprobed metadata missing: %#v", requests[3])
	}
	if reprobed["trace"] != "ghi" {
		t.Fatalf("user metadata should reprobe after TTL: %#v", reprobed)
	}
	for key := range reprobed {
		if strings.HasPrefix(key, "claude_code_") {
			t.Fatalf("continuity metadata should not reappear after TTL reprobe when disabled: %#v", reprobed)
		}
	}
}

func TestBuildBackendRequestAnonymousModeSuppressesAllIdentityFields(t *testing.T) {
	forward := true
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnableBackendMetadata: true,
		AnonymousMode:         true,
		ForwardUserMetadata:   &forward,
		UserMetadataAllowlist: []string{"trace"},
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{
			"trace":   "abc",
			"user_id": `{"device_id":"dev-1","account_uuid":"","session_id":"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752"}`,
		},
	}, http.Header{
		"X-Claude-Code-Session-Id":  []string{"session-1"},
		"X-Claude-Code-Model":       []string{"claude-sonnet-4-5"},
		"X-Claude-Code-Config-Hash": []string{"cfg-1"},
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Metadata != nil {
		t.Fatalf("metadata should be omitted in anonymous mode: %#v", payload.Metadata)
	}
	if payload.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key should be omitted in anonymous mode: %q", payload.PromptCacheKey)
	}
}

func TestHandleMessagesAnonymousModeOmitsIdentityFields(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
		AnonymousMode:         true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"metadata":{"trace":"abc","user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"\",\"session_id\":\"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752\"}"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "session-1")
	request.Header.Set("X-Claude-Code-Model", "claude-sonnet-4-5")
	request.Header.Set("X-Claude-Code-Config-Hash", "cfg-1")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if _, ok := requests[0]["metadata"]; ok {
		t.Fatalf("metadata should be omitted in anonymous mode: %#v", requests[0])
	}
	if _, ok := requests[0]["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be omitted in anonymous mode: %#v", requests[0])
	}
}

func TestHandleMessagesAnonymousModeDoesNotConsumeMetadataTTLReprobeLease(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 6, 0, 0, 0, time.UTC)
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
		AnonymousMode:         true,
		CapabilityReprobeTTL:  30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	proxy.caps.Metadata = capabilityUnsupported
	proxy.unsupportedUntil[capabilityCooldownKey("global", "metadata")] = currentTime.Add(-1 * time.Minute)

	send := func(trace string) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"gpt-5.4",
			"metadata":{"trace":"`+trace+`"},
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	send("anon")
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if _, ok := requests[0]["metadata"]; ok {
		t.Fatalf("metadata should stay omitted in anonymous mode: %#v", requests[0])
	}
	if _, ok := proxy.reprobeUntil[capabilityReprobeLeaseKey("global", "metadata")]; ok {
		t.Fatalf("anonymous request should not consume metadata TTL reprobe lease")
	}

	proxy.cfg.AnonymousMode = false
	send("normal")
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if _, ok := requests[1]["metadata"]; !ok {
		t.Fatalf("first non-anonymous request should still reprobe metadata immediately: %#v", requests[1])
	}
}

func TestHandleMessagesAnonymousModeDoesNotConsumePromptCacheKeyTTLReprobeLease(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 6, 0, 0, 0, time.UTC)
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:       backend.URL,
		BackendPath:          "/v1/responses",
		BackendAPIKey:        "rawchat-key",
		BackendModel:         "gpt-5.4",
		AnonymousMode:        true,
		CapabilityReprobeTTL: 30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	scopeKey := backendCapabilityScopeKey("gpt-5.4")
	proxy.scopedCaps[scopeKey] = scopedRuntimeCapabilities{PromptCacheKey: capabilityUnsupported}
	proxy.unsupportedUntil[capabilityCooldownKey(scopeKey, "prompt_cache_key")] = currentTime.Add(-1 * time.Minute)

	send := func(sessionID string) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"claude-sonnet-4-5",
			"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"\",\"session_id\":\"`+sessionID+`\"}"},
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	send("anon-session")
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if _, ok := requests[0]["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should stay omitted in anonymous mode: %#v", requests[0])
	}
	if _, ok := proxy.reprobeUntil[capabilityReprobeLeaseKey(scopeKey, "prompt_cache_key")]; ok {
		t.Fatalf("anonymous request should not consume prompt_cache_key TTL reprobe lease")
	}

	proxy.cfg.AnonymousMode = false
	send("restored-session")
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if got := requests[1]["prompt_cache_key"]; got != "restored-session" {
		t.Fatalf("first non-anonymous request should still reprobe prompt_cache_key immediately: %#v", requests[1])
	}
}

func TestHandleMessagesKeepsMetadataDisabledWithinTTLWindow(t *testing.T) {
	currentTime := time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC)
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_ok",
			Output: []OpenAIOutputItem{{
				Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}},
			}},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
		CapabilityReprobeTTL:  30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	currentTime = currentTime.Add(10 * time.Minute)
	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"def"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)

	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["metadata"]; ok {
		t.Fatalf("metadata should remain disabled within TTL window: %#v", requests[2])
	}
}

func TestHandleMessagesReprobesMetadataAfterTTLAndRestoresOnSuccess(t *testing.T) {
	currentTime := time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC)
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: metadata","type":"invalid_request_error","param":"metadata"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID: "resp_ok",
			Output: []OpenAIOutputItem{{
				Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}},
			}},
			Usage: OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
		CapabilityReprobeTTL:  30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	currentTime = currentTime.Add(31 * time.Minute)
	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"def"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)

	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["metadata"]; !ok {
		t.Fatalf("metadata should reprobe after TTL expiry: %#v", requests[2])
	}

	currentTime = currentTime.Add(1 * time.Minute)
	recorder3 := httptest.NewRecorder()
	request3 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"metadata":{"trace":"ghi"},
		"messages":[{"role":"user","content":"hi third"}]
	}`))
	request3.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder3, request3)
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(requests))
	}
	if _, ok := requests[3]["metadata"]; !ok {
		t.Fatalf("metadata should remain restored after successful reprobe: %#v", requests[3])
	}
}

func TestOptionsForRequestMetadataTTLReprobeUsesSingleLeader(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 1, 0, 0, 0, time.UTC)
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		EnableBackendMetadata: true,
		CapabilityReprobeTTL:  30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	proxy.caps.Metadata = capabilityUnsupported
	proxy.unsupportedUntil[capabilityCooldownKey("global", "metadata")] = currentTime.Add(-1 * time.Minute)

	req := AnthropicMessagesRequest{
		Model:    "gpt-5.4",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
	}

	first := proxy.optionsForRequest(req, nil)
	second := proxy.optionsForRequest(req, nil)

	if !first.EnableMetadata {
		t.Fatalf("first request after TTL should become reprobe leader")
	}
	if second.EnableMetadata {
		t.Fatalf("second request during reprobe lease should remain disabled")
	}
}

func TestOptionsForRequestModelPassthroughTTLLeaseStaysRequestScoped(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 1, 0, 0, 0, time.UTC)
	proxy := New(Config{
		BackendBaseURL:       "https://example.com/codex",
		BackendPath:          "/v1/responses",
		BackendAPIKey:        "test-key",
		CapabilityReprobeTTL: 30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	reqA := requestScopeKey(proxy.cfg, AnthropicMessagesRequest{Model: "claude-sonnet-4-5"})
	reqB := requestScopeKey(proxy.cfg, AnthropicMessagesRequest{Model: "claude-haiku-4-5"})
	proxy.caps.PreferredModel = "gpt-5.4"
	proxy.scopedCaps[reqA] = scopedRuntimeCapabilities{ModelPassthrough: capabilityUnsupported}
	proxy.unsupportedUntil[capabilityCooldownKey(reqA, "model")] = currentTime.Add(-1 * time.Minute)

	optsA1 := proxy.optionsForRequest(AnthropicMessagesRequest{Model: "claude-sonnet-4-5", Messages: []AnthropicMessage{{Role: "user", Content: "hi"}}}, nil)
	optsA2 := proxy.optionsForRequest(AnthropicMessagesRequest{Model: "claude-sonnet-4-5", Messages: []AnthropicMessage{{Role: "user", Content: "hi"}}}, nil)
	optsB := proxy.optionsForRequest(AnthropicMessagesRequest{Model: "claude-haiku-4-5", Messages: []AnthropicMessage{{Role: "user", Content: "hi"}}}, nil)

	if optsA1.Model != "claude-sonnet-4-5" {
		t.Fatalf("first request for model A should reprobe passthrough: %+v", optsA1)
	}
	if optsA2.Model != "gpt-5.4" {
		t.Fatalf("second request for model A should honor reprobe lease and stay downgraded: %+v", optsA2)
	}
	if optsB.Model != "claude-haiku-4-5" {
		t.Fatalf("request-scope lease should not affect model B: %+v", optsB)
	}
	if _, ok := proxy.reprobeUntil[capabilityReprobeLeaseKey(reqB, "model")]; ok {
		t.Fatalf("model B should not acquire reprobe lease from model A")
	}
}

func TestOptionsForRequestInputItemPersistenceTTLLeaseStaysBackendScoped(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 1, 0, 0, 0, time.UTC)
	proxy := New(Config{
		BackendBaseURL:       "https://example.com/codex",
		BackendPath:          "/v1/responses",
		BackendAPIKey:        "test-key",
		CapabilityReprobeTTL: 30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }

	scopeA := backendCapabilityScopeKey("claude-sonnet-4-5")
	scopeB := backendCapabilityScopeKey("claude-haiku-4-5")
	proxy.scopedCaps[scopeA] = scopedRuntimeCapabilities{InputItemPersistence: capabilityUnsupported}
	proxy.scopedCaps[scopeB] = scopedRuntimeCapabilities{InputItemPersistence: capabilityUnsupported}
	proxy.unsupportedUntil[capabilityCooldownKey(scopeA, "input_item_persistence")] = currentTime.Add(-1 * time.Minute)
	proxy.unsupportedUntil[capabilityCooldownKey(scopeB, "input_item_persistence")] = currentTime.Add(-1 * time.Minute)

	reqNoHistory := AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
	}
	if opts := proxy.optionsForRequest(reqNoHistory, nil); opts.StripRoundTripItemIDs {
		t.Fatalf("request without round-trip history should not strip ids: %+v", opts)
	}
	if _, ok := proxy.reprobeUntil[capabilityReprobeLeaseKey(scopeA, "input_item_persistence")]; ok {
		t.Fatalf("request without round-trip history should not consume input_item_persistence reprobe lease")
	}

	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
		Summary:          []OpenAIReasoningPart{{Type: "summary_text", Text: "brief reasoning"}},
	})
	reqWithHistoryA := AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": carrier}}},
			{Role: "user", Content: "continue"},
		},
	}
	firstA := proxy.optionsForRequest(reqWithHistoryA, nil)
	secondA := proxy.optionsForRequest(reqWithHistoryA, nil)
	if firstA.StripRoundTripItemIDs {
		t.Fatalf("first history request for scope A should reprobe before stripping ids: %+v", firstA)
	}
	if !secondA.StripRoundTripItemIDs {
		t.Fatalf("second history request for scope A should honor reprobe lease and strip ids: %+v", secondA)
	}

	reqWithHistoryB := AnthropicMessagesRequest{
		Model: "claude-haiku-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": carrier}}},
			{Role: "user", Content: "continue"},
		},
	}
	firstB := proxy.optionsForRequest(reqWithHistoryB, nil)
	if firstB.StripRoundTripItemIDs {
		t.Fatalf("backend scope B should retain its own first reprobe attempt: %+v", firstB)
	}
	if _, ok := proxy.reprobeUntil[capabilityReprobeLeaseKey(scopeB, "input_item_persistence")]; !ok {
		t.Fatalf("backend scope B should acquire its own reprobe lease")
	}
}

func TestHandleMessagesContextManagementTTLReprobeStillKeepsLatestCompactionShrinking(t *testing.T) {
	currentTime := time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC)
	type captured struct {
		ContextManagement any
		Input             []any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		var input []any
		if rawInput, ok := body["input"].([]any); ok {
			input = rawInput
		}
		requests = append(requests, captured{
			ContextManagement: body["context_management"],
			Input:             input,
		})
		if body["context_management"] != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: context_management","type":"invalid_request_error","param":"context_management"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_compaction_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:            backend.URL,
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "rawchat-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
		CapabilityReprobeTTL:      30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id": "gpt-5.4",
			"capabilities": map[string]any{
				"supports": map[string]any{},
				"limits": map[string]any{
					"max_prompt_tokens": 10000,
				},
			},
			"supported_endpoints": []string{"/v1/responses"},
		}),
	})

	oldCarrier := encodeCompactionCarrier("cmp_old", "opaque-old")
	newCarrier := encodeCompactionCarrier("cmp_new", "opaque-new")
	makeRequest := func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"claude-sonnet-4-5",
			"messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+oldCarrier+`"}]},
				{"role":"user","content":"before latest"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"Thinking...","signature":"`+newCarrier+`"}]},
				{"role":"user","content":"after latest"}
			]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	makeRequest() // unsupported
	currentTime = currentTime.Add(10 * time.Minute)
	makeRequest() // still disabled
	currentTime = currentTime.Add(21 * time.Minute)
	makeRequest() // reprobe unsupported again

	if len(requests) != 5 {
		t.Fatalf("request count = %d, want 5", len(requests))
	}
	if requests[1].ContextManagement != nil {
		t.Fatalf("second request should keep context_management disabled within TTL: %#v", requests[1].ContextManagement)
	}
	if requests[3].ContextManagement == nil {
		t.Fatalf("third request should reprobe context_management after TTL expiry")
	}
	for i, req := range requests {
		if len(req.Input) != 2 {
			t.Fatalf("request %d input count = %d, want 2", i, len(req.Input))
		}
		first := req.Input[0].(map[string]any)
		if first["type"] != "compaction" || first["id"] != "cmp_new" {
			t.Fatalf("request %d should still be shrunk to latest compaction: %#v", i, req.Input)
		}
	}
}

func TestHandleMessagesModelPassthroughTTLOnlyAffectsRequestScope(t *testing.T) {
	currentTime := time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC)
	var requests []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": "gpt-5.4"}},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			model := asString(body["model"])
			requests = append(requests, model)
			if model == "claude-sonnet-4-5" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"Invalid model: claude-sonnet-4-5","type":"invalid_request_error","param":"model"}}`)
				return
			}
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:       backend.URL,
		BackendPath:          "/v1/responses",
		BackendAPIKey:        "rawchat-key",
		CapabilityReprobeTTL: 30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }

	makeRequest := func(model string) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"`+model+`",
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}

	makeRequest("claude-sonnet-4-5") // fail then fallback
	currentTime = currentTime.Add(31 * time.Minute)
	makeRequest("claude-sonnet-4-5") // reprobe again
	makeRequest("claude-haiku-4-5")  // separate request scope

	if len(requests) < 5 {
		t.Fatalf("request count = %d, want at least 5", len(requests))
	}
	if requests[0] != "claude-sonnet-4-5" || requests[1] != "gpt-5.4" || requests[2] != "claude-sonnet-4-5" || requests[3] != "gpt-5.4" {
		t.Fatalf("model A should reprobe independently after TTL: %#v", requests)
	}
	if requests[4] != "claude-haiku-4-5" {
		t.Fatalf("model B should keep its own request scope and not inherit model A cooldown: %#v", requests)
	}
}

func TestHandleMessagesMetadataTTLReprobeUsesSingleLeaderUnderConcurrency(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 3, 0, 0, 0, time.UTC)
	type captured struct {
		metadata bool
	}
	var (
		mu       sync.Mutex
		requests []captured
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		mu.Lock()
		requests = append(requests, captured{metadata: body["metadata"] != nil})
		withMetadata := body["metadata"] != nil
		mu.Unlock()
		if withMetadata {
			time.Sleep(100 * time.Millisecond)
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnableBackendMetadata: true,
		CapabilityReprobeTTL:  30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	proxy.caps.Metadata = capabilityUnsupported
	proxy.unsupportedUntil[capabilityCooldownKey("global", "metadata")] = currentTime.Add(-1 * time.Minute)

	runRequest := func() string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"gpt-5.4",
			"metadata":{"trace":"abc"},
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = runRequest() }()
	go func() { defer wg.Done(); _ = runRequest() }()
	wg.Wait()

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	metadataCount := 0
	for _, req := range requests {
		if req.metadata {
			metadataCount++
		}
	}
	if metadataCount != 1 {
		t.Fatalf("expected exactly one leader reprobe with metadata, got %d from %#v", metadataCount, requests)
	}
}

func TestHandleMessagesStreamTTLReprobeUsesSingleLeaderAndKeepsAnthropicSSE(t *testing.T) {
	currentTime := time.Date(2026, 4, 17, 3, 0, 0, 0, time.UTC)
	type captured struct {
		stream bool
	}
	var (
		mu       sync.Mutex
		requests []captured
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		mu.Lock()
		requests = append(requests, captured{stream: body["stream"] != nil})
		withStream := body["stream"] != nil
		mu.Unlock()
		if withStream {
			time.Sleep(100 * time.Millisecond)
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_stream",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:       backend.URL,
		BackendPath:          "/v1/responses",
		BackendAPIKey:        "rawchat-key",
		CapabilityReprobeTTL: 30 * time.Minute,
	})
	proxy.now = func() time.Time { return currentTime }
	scopeKey := backendCapabilityScopeKey("gpt-5.4")
	proxy.scopedCaps[scopeKey] = scopedRuntimeCapabilities{BackendStreaming: capabilityUnsupported}
	proxy.unsupportedUntil[capabilityCooldownKey(scopeKey, "stream")] = currentTime.Add(-1 * time.Minute)

	runRequest := func() string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model":"gpt-5.4",
			"stream":true,
			"messages":[{"role":"user","content":"hi"}]
		}`))
		request.Header.Set("Content-Type", "application/json")
		proxy.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}

	var (
		wg      sync.WaitGroup
		results = make([]string, 2)
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = runRequest()
		}(i)
	}
	wg.Wait()

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	streamCount := 0
	for _, req := range requests {
		if req.stream {
			streamCount++
		}
	}
	if streamCount != 1 {
		t.Fatalf("expected exactly one stream reprobe leader, got %d from %#v", streamCount, requests)
	}
	for i, body := range results {
		if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
			t.Fatalf("request %d did not preserve Anthropic SSE wrapper\n%s", i, body)
		}
	}
}
func TestHandleMessagesRetriesWithoutPromptCacheKeyAfterUnsupportedParameterPromptCacheKey(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: prompt_cache_key","type":"invalid_request_error","param":"prompt_cache_key"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_prompt_cache_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"\",\"session_id\":\"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752\"}"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if requests[0]["prompt_cache_key"] != "2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752" {
		t.Fatalf("first request missing prompt_cache_key: %#v", requests[0])
	}
	if _, ok := requests[1]["prompt_cache_key"]; ok {
		t.Fatalf("second request still has prompt_cache_key: %#v", requests[1])
	}

	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"\",\"session_id\":\"2c4e1cf0-7a67-4d2e-9a4b-1d16d3f44752\"}"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)

	if recorder2.Code != http.StatusOK {
		t.Fatalf("second round status = %d, body = %s", recorder2.Code, recorder2.Body.String())
	}
	if len(requests) != 3 {
		t.Fatalf("sticky disable request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["prompt_cache_key"]; ok {
		t.Fatalf("third request still has prompt_cache_key after disable: %#v", requests[2])
	}
}

func TestHandleMessagesRetriesWithoutReasoningAfterUnsupportedParameterReasoning(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: reasoning","type":"invalid_request_error","param":"reasoning"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_reasoning_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if _, ok := requests[0]["reasoning"]; !ok {
		t.Fatalf("first request missing reasoning: %#v", requests[0])
	}
	if _, ok := requests[1]["reasoning"]; ok {
		t.Fatalf("second request still has reasoning: %#v", requests[1])
	}

	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)

	if recorder2.Code != http.StatusOK {
		t.Fatalf("second round status = %d, body = %s", recorder2.Code, recorder2.Body.String())
	}
	if len(requests) != 3 {
		t.Fatalf("sticky disable request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["reasoning"]; ok {
		t.Fatalf("third request still has reasoning after disable: %#v", requests[2])
	}

	recorder3 := httptest.NewRecorder()
	request3 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-haiku-4-5",
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"fresh model"}]
	}`))
	request3.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder3, request3)

	if recorder3.Code != http.StatusOK {
		t.Fatalf("third round status = %d, body = %s", recorder3.Code, recorder3.Body.String())
	}
	if len(requests) != 4 {
		t.Fatalf("cross-model request count = %d, want 4", len(requests))
	}
	if _, ok := requests[3]["reasoning"]; !ok {
		t.Fatalf("different request model should still send reasoning: %#v", requests[3])
	}
}

func TestHandleMessagesRetriesWithoutReasoningIncludeAfterUnsupportedParameterInclude(t *testing.T) {
	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
		Summary:          []OpenAIReasoningPart{{Type: "summary_text", Text: "brief reasoning"}},
	})
	reqBody, err := json.Marshal(AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": carrier},
				map[string]any{"type": "text", "text": "continuing"},
			}},
			{Role: "user", Content: "continue"},
		},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported include value reasoning.encrypted_content","type":"invalid_request_error","param":"include[0]"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_reasoning_include_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	firstInclude, ok := requests[0]["include"].([]any)
	if !ok || len(firstInclude) != 1 || firstInclude[0] != "reasoning.encrypted_content" {
		t.Fatalf("first request include incorrect: %#v", requests[0]["include"])
	}
	if _, ok := requests[1]["include"]; ok {
		t.Fatalf("second request still has include: %#v", requests[1])
	}
}

func TestHandleMessagesRetriesWithoutPersistedHistoryIDsAfterStatelessSessionError(t *testing.T) {
	carrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_legacy",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
		Summary:          []OpenAIReasoningPart{{Type: "summary_text", Text: "brief reasoning"}},
	})
	reqBody, err := json.Marshal(AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "thinking", "thinking": "brief reasoning", "signature": carrier},
				map[string]any{"type": "text", "text": "continuing"},
			}},
			{Role: "user", Content: "continue"},
		},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)

		input, ok := body["input"].([]any)
		if !ok || len(input) == 0 {
			t.Fatalf("missing input payload: %#v", body)
		}
		firstItem, ok := input[0].(map[string]any)
		if !ok {
			t.Fatalf("first input item shape = %#v", input[0])
		}
		if store, ok := body["store"].(bool); !ok || !store {
			t.Fatalf("store should be enabled on both attempts: %#v", body["store"])
		}

		if len(requests) == 1 {
			if firstItem["id"] != "rs_legacy" {
				t.Fatalf("first retry attempt should preserve original reasoning item id: %#v", firstItem)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "{\"error\":{\"code\":null,\"message\":\"{\\n  \\\"error\\\": {\\n    \\\"message\\\": \\\"Item with id 'rs_legacy' not found. Items are not persisted when `store` is set to false. Try again with `store` set to true, or remove this item from your input.\\\",\\n    \\\"type\\\": \\\"invalid_request_error\\\",\\n    \\\"param\\\": \\\"input\\\",\\n    \\\"code\\\": null\\n  }\\n}（traceid: cf78a7283868a718343b4b489c043dac）\",\"param\":null,\"type\":\"invalid_request_error\"}}")
			return
		}

		if _, ok := firstItem["id"]; ok {
			t.Fatalf("second retry attempt should strip persisted reasoning ids after stateless-session failure: %#v", firstItem)
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_persistence_retry_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
}

func TestHandleMessagesRetriesWithoutPhaseAfterUnsupportedParameterPhase(t *testing.T) {
	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: phase","type":"invalid_request_error","param":"input[0].phase"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL:        backend.URL,
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "rawchat-key",
		EnablePhaseCommentary: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"I will inspect this."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}]},
			{"role":"user","content":"continue"}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	firstInput := requests[0]["input"].([]any)[0].(map[string]any)
	if firstInput["phase"] != "commentary" {
		t.Fatalf("first request missing commentary phase: %#v", firstInput)
	}
	secondInput := requests[1]["input"].([]any)[0].(map[string]any)
	if _, ok := secondInput["phase"]; ok {
		t.Fatalf("second request still has phase: %#v", secondInput)
	}
}

func TestHandleMessagesRetriesWithoutStructuredToolOutputAfterUnsupportedFunctionCallOutputOutput(t *testing.T) {
	reqBody, err := json.Marshal(AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "inspect",
					"input": map[string]any{"path": "report.json"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "text", "text": "stdout"},
						map[string]any{"type": "json", "json": map[string]any{"severity": "high"}},
					},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	var requests []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid type for function_call_output.output","type":"invalid_request_error","param":"input[1].output"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_tool_output_ok",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
			Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	firstOutput := requests[0]["input"].([]any)[1].(map[string]any)["output"]
	if _, ok := firstOutput.([]any); !ok {
		t.Fatalf("first request output should preserve structure: %#v", firstOutput)
	}
	secondOutput := requests[1]["input"].([]any)[1].(map[string]any)["output"]
	if _, ok := secondOutput.(string); !ok {
		t.Fatalf("second request output should flatten to string: %#v", secondOutput)
	}
	if got := secondOutput.(string); !strings.Contains(got, "stdout") {
		t.Fatalf("flattened output should contain tool text, got %q", got)
	}

	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("second round status = %d, body = %s", recorder2.Code, recorder2.Body.String())
	}
	if len(requests) != 3 {
		t.Fatalf("sticky disable request count = %d, want 3", len(requests))
	}
	if _, ok := requests[2]["input"].([]any)[1].(map[string]any)["output"].(string); !ok {
		t.Fatalf("third request output should remain flattened after disable: %#v", requests[2])
	}
}

func TestHandleMessagesStreamRetriesWithStreamingDisabledAfterUnsupportedParameterStream(t *testing.T) {
	type captured struct {
		Accept string
		Body   map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, captured{Accept: r.Header.Get("Accept"), Body: body})
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: stream","type":"invalid_request_error","param":"stream"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_json_stream",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "hello from json"}}}},
			Usage:  OpenAIUsage{InputTokens: 3, OutputTokens: 2},
		})
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if requests[0].Accept != "text/event-stream" {
		t.Fatalf("first accept = %q, want text/event-stream", requests[0].Accept)
	}
	if requests[1].Accept != "application/json" {
		t.Fatalf("second accept = %q, want application/json", requests[1].Accept)
	}
	if stream, _ := requests[0].Body["stream"].(bool); !stream {
		t.Fatalf("first request stream flag not set: %#v", requests[0].Body)
	}
	if _, ok := requests[1].Body["stream"]; ok {
		t.Fatalf("second request should omit stream after downgrade: %#v", requests[1].Body)
	}
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") || !strings.Contains(body, "hello from json") {
		t.Fatalf("client did not receive Anthropic SSE after backend stream downgrade\n%s", body)
	}
}

func TestHandleMessagesStreamRetriesWithStreamingDisabledAfterUnrecognizedRequestArgumentStream(t *testing.T) {
	type captured struct {
		Accept string
		Body   map[string]any
	}
	var requests []captured
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		requests = append(requests, captured{Accept: r.Header.Get("Accept"), Body: body})
		if len(requests) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: stream","type":"invalid_request_error"}}`)
			return
		}
		writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
			ID:     "resp_json_stream",
			Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "hello from json"}}}},
			Usage:  OpenAIUsage{InputTokens: 3, OutputTokens: 2},
		})
	}))
	defer backend.Close()

	proxy := New(Config{BackendBaseURL: backend.URL, BackendPath: "/v1/responses", BackendAPIKey: "rawchat-key"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, body)
	}
	if len(requests) != 2 {
		t.Fatalf("backend request count = %d, want 2", len(requests))
	}
	if requests[0].Accept != "text/event-stream" {
		t.Fatalf("first accept = %q, want text/event-stream", requests[0].Accept)
	}
	if requests[1].Accept != "application/json" {
		t.Fatalf("second accept = %q, want application/json", requests[1].Accept)
	}
	if stream, _ := requests[0].Body["stream"].(bool); !stream {
		t.Fatalf("first request stream flag not set: %#v", requests[0].Body)
	}
	if _, ok := requests[1].Body["stream"]; ok {
		t.Fatalf("second request should omit stream after provider-style downgrade: %#v", requests[1].Body)
	}
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") || !strings.Contains(body, "hello from json") {
		t.Fatalf("client did not receive Anthropic SSE after provider-style backend stream downgrade\n%s", body)
	}
}

func TestHandleMessagesRetriesWithPreferredModelAfterUnsupportedPassthroughModel(t *testing.T) {
	var (
		responseModels []string
		modelsRequests int
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelsRequests++
			writeJSONWithStatus(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "gpt-5.4"},
					{"id": "gpt-5.4-mini"},
				},
			})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode backend request: %v", err)
			}
			responseModels = append(responseModels, asString(body["model"]))
			if len(responseModels) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"Invalid model: claude-sonnet-4-5","type":"invalid_request_error","param":"model"}}`)
				return
			}
			writeJSONWithStatus(w, http.StatusOK, OpenAIResponsesResponse{
				ID:     "resp_model_ok",
				Output: []OpenAIOutputItem{{Type: "message", Role: "assistant", Content: []OpenAIOutputContent{{Type: "output_text", Text: "ok"}}}},
				Usage:  OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	proxy := New(Config{
		BackendBaseURL: backend.URL,
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "rawchat-key",
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if modelsRequests != 1 {
		t.Fatalf("models request count = %d, want 1", modelsRequests)
	}
	if len(responseModels) != 2 {
		t.Fatalf("response request count = %d, want 2", len(responseModels))
	}
	if responseModels[0] != "claude-sonnet-4-5" {
		t.Fatalf("first request model = %q, want passthrough model", responseModels[0])
	}
	if responseModels[1] != "gpt-5.4" {
		t.Fatalf("second request model = %q, want preferred backend model", responseModels[1])
	}

	recorder2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-haiku-4-5",
		"messages":[{"role":"user","content":"hi again"}]
	}`))
	request2.Header.Set("Content-Type", "application/json")
	proxy.Handler().ServeHTTP(recorder2, request2)

	if recorder2.Code != http.StatusOK {
		t.Fatalf("second round status = %d, body = %s", recorder2.Code, recorder2.Body.String())
	}
	if modelsRequests != 1 {
		t.Fatalf("models request count after second request = %d, want 1", modelsRequests)
	}
	if len(responseModels) != 3 {
		t.Fatalf("second request should not inherit previous fallback, request count = %d, want 3", len(responseModels))
	}
	if responseModels[2] != "claude-haiku-4-5" {
		t.Fatalf("third request model = %q, want new request model passthrough", responseModels[2])
	}
}

func TestBuildBackendRequestHonorsPreseededCapabilityState(t *testing.T) {
	newReq := func() AnthropicMessagesRequest {
		return AnthropicMessagesRequest{
			Model:  "claude-sonnet-4-5",
			Stream: true,
			OutputConfig: &AnthropicOutputConfig{
				Effort: "max",
			},
			Messages: []AnthropicMessage{
				{
					Role: "assistant",
					Content: []any{
						map[string]any{"type": "text", "text": "I will inspect this."},
						map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
					},
				},
				{Role: "user", Content: "continue"},
			},
		}
	}

	t.Run("seeded capability disable avoids first retry shape", func(t *testing.T) {
		proxy := New(Config{
			BackendBaseURL:        "https://example.com/codex",
			BackendPath:           "/v1/responses",
			BackendAPIKey:         "test-key",
			EnablePhaseCommentary: true,
		})

		req := newReq()
		scopeKey := backendCapabilityScopeKey(proxy.cfg.EffectiveBackendModel(req.Model))
		proxy.scopedCaps[scopeKey] = scopedRuntimeCapabilities{
			BackendStreaming:     capabilityUnsupported,
			StructuredToolOutput: capabilityUnsupported,
			Reasoning:            capabilityUnsupported,
			ReasoningInclude:     capabilityUnsupported,
			Phase:                capabilityUnsupported,
		}

		payload, _, err := proxy.buildBackendRequestWithOptions(context.Background(), req, nil, proxy.optionsForRequest(req, nil))
		if err != nil {
			t.Fatalf("build request with seeded caps: %v", err)
		}

		if payload.Stream {
			t.Fatalf("stream should be disabled by seeded caps: %#v", payload)
		}
		if payload.Reasoning != nil {
			t.Fatalf("reasoning should be disabled by seeded caps: %#v", payload.Reasoning)
		}
		if len(payload.Include) != 0 {
			t.Fatalf("include should be empty when reasoning/include is pre-disabled: %#v", payload.Include)
		}
		if len(payload.Input) == 0 {
			t.Fatalf("input should not be empty")
		}
		if payload.Input[0].Phase != "" {
			t.Fatalf("phase should be omitted when commentary is pre-disabled: %#v", payload.Input[0])
		}
	})

	t.Run("ordinary request remains unaffected without seeded caps", func(t *testing.T) {
		proxy := New(Config{
			BackendBaseURL:        "https://example.com/codex",
			BackendPath:           "/v1/responses",
			BackendAPIKey:         "test-key",
			EnablePhaseCommentary: true,
		})

		req := newReq()
		payload, _, err := proxy.buildBackendRequestWithOptions(context.Background(), req, nil, proxy.optionsForRequest(req, nil))
		if err != nil {
			t.Fatalf("build ordinary request: %v", err)
		}

		if !payload.Stream {
			t.Fatalf("ordinary request should keep backend streaming enabled: %#v", payload)
		}
		if payload.Reasoning == nil {
			t.Fatalf("ordinary request should retain reasoning when not pre-disabled")
		}
		if len(payload.Input) == 0 {
			t.Fatalf("input should not be empty")
		}
		if payload.Input[0].Phase != "commentary" {
			t.Fatalf("ordinary request should still infer commentary phase: %#v", payload.Input[0])
		}
	})
}

func TestHandleCountTokensUsesAnthropicWhenConfigured(t *testing.T) {
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("anthropic path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "ant-key" {
			t.Fatalf("x-api-key = %q, want ant-key", got)
		}
		var req AnthropicCountTokensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode anthropic count_tokens request: %v", err)
		}
		if req.Model != "claude-sonnet-4-5" {
			t.Fatalf("anthropic model = %q, want claude-sonnet-4-5", req.Model)
		}
		writeJSONWithStatus(w, http.StatusOK, AnthropicCountTokensResponse{InputTokens: 321})
	}))
	defer anthropic.Close()

	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		AnthropicAPIBaseURL:   anthropic.URL,
		AnthropicAPIKey:       "ant-key",
		ClaudeTokenMultiplier: defaultClaudeTokenMultiplier,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response AnthropicCountTokensResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens != 321 {
		t.Fatalf("input_tokens = %d, want 321", response.InputTokens)
	}
}

func TestHandleCountTokensFallsBackToEstimateForClaude(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		ClaudeTokenMultiplier: 2,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello world"}]
	}`))
	request.Header.Set("Content-Type", "application/json")

	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response AnthropicCountTokensResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens != 6 {
		t.Fatalf("input_tokens = %d, want 6", response.InputTokens)
	}
}

func TestHandleCountTokensSkipsClaudeToolOverheadForMCPTools(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		ClaudeTokenMultiplier: 1,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello world"}],
		"tools":[{"name":"mcp__ctftime__get_upcoming_ctfs","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "client-tools-2025-01-01")

	proxy.Handler().ServeHTTP(recorder, request)

	var response AnthropicCountTokensResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens >= 346 {
		t.Fatalf("expected MCP tool path to skip fixed overhead, got %d", response.InputTokens)
	}
}

func TestHandleCountTokensAddsClaudeToolOverheadForNonMCPTools(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		ClaudeTokenMultiplier: 1,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello world"}],
		"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "client-tools-2025-01-01")

	proxy.Handler().ServeHTTP(recorder, request)

	var response AnthropicCountTokensResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens <= 346 {
		t.Fatalf("expected tool overhead in input_tokens, got %d", response.InputTokens)
	}
}

func TestParseSubagentMarkerFromText(t *testing.T) {
	text := `<system-reminder>
__SUBAGENT_MARKER__{"session_id":"root-session","agent_id":"agent-1","agent_type":"researcher"}
</system-reminder>`

	marker, ok := parseSubagentMarkerFromText(text)
	if !ok {
		t.Fatalf("parseSubagentMarkerFromText() = false, want true")
	}
	if marker.SessionID != "root-session" || marker.AgentID != "agent-1" || marker.AgentType != "researcher" {
		t.Fatalf("marker = %#v", marker)
	}
}

func TestBuildBackendRequestStripsSystemCacheControlScope(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		System: []any{
			map[string]any{
				"type": "text",
				"text": "Rule one",
				"cache_control": map[string]any{
					"scope": "ephemeral",
					"foo":   "bar",
				},
			},
		},
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Instructions != "Rule one" {
		t.Fatalf("instructions = %q, want Rule one", payload.Instructions)
	}
}

func TestBuildBackendRequestFiltersInvalidAssistantThinkingBlocks(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "thinking", "thinking": "Thinking...", "signature": "opaque@123"},
					map[string]any{"type": "text", "text": "hello"},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 1 || payload.Input[0].Role != "assistant" {
		t.Fatalf("assistant input malformed: %#v", payload.Input)
	}
	if len(payload.Input[0].Content) != 1 || payload.Input[0].Content[0].Text != "hello" {
		t.Fatalf("invalid thinking block should be filtered: %#v", payload.Input[0])
	}
}

func TestBuildBackendRequestSkipsReasoningWhenToolChoiceForcesTool(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:        "claude-sonnet-4-5",
		OutputConfig: &AnthropicOutputConfig{Effort: "max"},
		ToolChoice:   &AnthropicToolChoice{Type: "any"},
		Messages:     []AnthropicMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if payload.Reasoning != nil {
		t.Fatalf("reasoning = %#v, want nil when tool_choice forces tool", payload.Reasoning)
	}
}

func TestBuildBackendRequestAcceptsSingletonContentBlock(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: map[string]any{"type": "text", "text": "hi"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	if payload.Input[0].Role != "user" || len(payload.Input[0].Content) != 1 || payload.Input[0].Content[0].Text != "hi" {
		t.Fatalf("singleton content mapping incorrect: %#v", payload.Input[0])
	}
}

func TestBuildBackendRequestOmitsMetadataByDefault(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
		Metadata: map[string]any{"trace": "abc"},
	}, http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Metadata != nil {
		t.Fatalf("metadata = %#v, want nil by default", payload.Metadata)
	}
}

func TestBuildBackendRequestMapsSystemToInstructions(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		System: []any{
			map[string]any{"type": "text", "text": "Rule one"},
			map[string]any{"type": "text", "text": "Rule two"},
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Instructions != "Rule one\n\nRule two" {
		t.Fatalf("instructions = %q, want joined system text", payload.Instructions)
	}
	if len(payload.Input) != 1 || payload.Input[0].Role != "user" {
		t.Fatalf("user input mapping incorrect: %#v", payload.Input)
	}
}

func TestBuildBackendRequestMapsThinkingToReasoningEffort(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:    "claude-sonnet-4-5",
		Thinking: &AnthropicThinking{Type: "enabled", BudgetTokens: 9000},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Reasoning == nil || payload.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %#v, want high", payload.Reasoning)
	}
}

func TestBuildBackendRequestMapsOutputConfigEffortToReasoning(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model:        "claude-sonnet-4-5",
		OutputConfig: &AnthropicOutputConfig{Effort: "max"},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if payload.Reasoning == nil || payload.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %#v, want high", payload.Reasoning)
	}
}

func TestBuildBackendRequestSetsParallelToolCallsOnlyWhenProfileAllows(t *testing.T) {
	newToolRequest := func() AnthropicMessagesRequest {
		return AnthropicMessagesRequest{
			Model: "gpt-5.4",
			Messages: []AnthropicMessage{
				{Role: "user", Content: "run the tool"},
			},
			Tools: []AnthropicTool{
				{
					Name:        "lookup",
					Description: "look things up",
					InputSchema: map[string]any{
						"type": "object",
					},
				},
			},
		}
	}
	wantTrue := true

	tests := []struct {
		name      string
		supports  any
		withTools bool
		want      *bool
	}{
		{
			name:      "true with tools sends flag",
			supports:  true,
			withTools: true,
			want:      &wantTrue,
		},
		{
			name:      "false omits flag",
			supports:  false,
			withTools: true,
			want:      nil,
		},
		{
			name:      "nil omits flag",
			supports:  nil,
			withTools: true,
			want:      nil,
		},
		{
			name:      "true without tools omits flag",
			supports:  true,
			withTools: false,
			want:      nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy := New(Config{
				BackendBaseURL:            "https://example.com/codex",
				BackendPath:               "/v1/responses",
				BackendAPIKey:             "test-key",
				BackendModel:              "gpt-5.4",
				EnableModelCapabilityInit: true,
			})

			supports := map[string]any{}
			if tc.supports != nil {
				supports["parallel_tool_calls"] = tc.supports
			}
			proxy.seedCapabilitiesFromModels([]map[string]any{
				normalizeModelDescriptor(map[string]any{
					"id":                  "gpt-5.4",
					"supported_endpoints": []string{"/v1/responses"},
					"capabilities": map[string]any{
						"supports": supports,
					},
				}),
			})

			req := newToolRequest()
			if !tc.withTools {
				req.Tools = nil
			}

			payload, _, err := proxy.buildBackendRequestWithOptions(context.Background(), req, nil, proxy.optionsForRequest(req, nil))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}

			if !reflect.DeepEqual(payload.ParallelToolCalls, tc.want) {
				t.Fatalf("parallel_tool_calls = %#v, want %#v", payload.ParallelToolCalls, tc.want)
			}
		})
	}
}

func TestBuildBackendRequestCapsReasoningEffortToMaxThinkingBudgetWithoutChangingUserContent(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL:            "https://example.com/codex",
		BackendPath:               "/v1/responses",
		BackendAPIKey:             "test-key",
		BackendModel:              "gpt-5.4",
		EnableModelCapabilityInit: true,
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"max_thinking_budget": 1500,
				},
			},
		}),
	})

	req := AnthropicMessagesRequest{
		Model: "gpt-5.4",
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "preserve this user content"},
		},
	}

	payload, _, err := proxy.buildBackendRequestWithOptions(context.Background(), req, nil, proxy.optionsForRequest(req, nil))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if payload.Reasoning == nil || payload.Reasoning.Effort != "low" {
		t.Fatalf("reasoning = %#v, want low after max_thinking_budget clamp", payload.Reasoning)
	}
	if len(payload.Input) != 1 || len(payload.Input[0].Content) != 1 {
		t.Fatalf("input mapping incorrect: %#v", payload.Input)
	}
	if got := payload.Input[0].Content[0].Text; got != "preserve this user content" {
		t.Fatalf("user content mutated: %q", got)
	}
}

func TestBuildBackendRequestIgnoresThinkingBudgetAndParallelToolCallsWhenCapabilityInitDisabled(t *testing.T) {
	proxy := New(Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	})

	proxy.seedCapabilitiesFromModels([]map[string]any{
		normalizeModelDescriptor(map[string]any{
			"id":                  "gpt-5.4",
			"supported_endpoints": []string{"/v1/responses"},
			"capabilities": map[string]any{
				"supports": map[string]any{
					"adaptive_thinking":   true,
					"parallel_tool_calls": true,
					"max_thinking_budget": 1500,
				},
			},
		}),
	})

	req := AnthropicMessagesRequest{
		Model: "gpt-5.4",
		OutputConfig: &AnthropicOutputConfig{
			Effort: "max",
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "leave this content alone"},
		},
		Tools: []AnthropicTool{
			{
				Name:        "lookup",
				Description: "look things up",
				InputSchema: map[string]any{"type": "object"},
			},
		},
	}

	payload, _, err := proxy.buildBackendRequestWithOptions(context.Background(), req, nil, proxy.optionsForRequest(req, nil))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if payload.Reasoning == nil || payload.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %#v, want high when capability init disabled", payload.Reasoning)
	}
	if payload.ParallelToolCalls != nil {
		t.Fatalf("parallel_tool_calls = %#v, want nil when capability init disabled", payload.ParallelToolCalls)
	}
	if got := payload.Input[0].Content[0].Text; got != "leave this content alone" {
		t.Fatalf("user content mutated: %q", got)
	}
}

func TestBuildBackendRequestAddsAssistantPhaseWhenEnabled(t *testing.T) {
	cfg := Config{
		BackendBaseURL:        "https://example.com/codex",
		BackendPath:           "/v1/responses",
		BackendAPIKey:         "test-key",
		BackendModel:          "gpt-5.4",
		EnablePhaseCommentary: true,
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "I will inspect this."},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if payload.Input[0].Phase != "commentary" {
		t.Fatalf("assistant phase = %q, want commentary", payload.Input[0].Phase)
	}
}

func TestBuildBackendRequestUsesOutputTextForAssistantHistory(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5.4",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "world"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if payload.Input[1].Role != "assistant" {
		t.Fatalf("assistant role mapping incorrect: %#v", payload.Input[1])
	}
	if got := payload.Input[1].Content[0].Type; got != "output_text" {
		t.Fatalf("assistant content type = %q, want output_text", got)
	}
}

func TestBuildBackendRequestConvertsDocumentContext(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{
					"type":    "document",
					"context": "Please follow the spec in this file.",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "text/plain",
						"data":       "SGVsbG8=",
					},
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	content := payload.Input[0].Content
	if len(content) != 2 {
		t.Fatalf("content item count = %d, want 2", len(content))
	}
	if content[0].Type != "input_text" || content[0].Text != "Please follow the spec in this file." {
		t.Fatalf("document context mapping incorrect: %#v", content[0])
	}
	if content[1].Type != "input_file" {
		t.Fatalf("document item mapping incorrect: %#v", content[1])
	}
}

func TestBuildBackendRequestConvertsToolResultWithNonTextBlocks(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "read",
					"input": map[string]any{"path": "note.txt"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "url",
								"url":  "https://example.com/i.png",
							},
						},
						map[string]any{"type": "text", "text": "ok"},
					},
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if payload.Input[1].Type != "function_call_output" {
		t.Fatalf("tool result mapping incorrect: %#v", payload.Input[1])
	}
	output := toolOutputStringForTest(t, payload.Input[1].Output)
	if !strings.Contains(output, "ok") || !strings.Contains(output, "[image url=https://example.com/i.png]") {
		t.Fatalf("flattened output missing expected summary: %q", output)
	}
}

func TestBuildBackendRequestConvertsToolReferenceInToolResult(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "ToolSearch",
					"input": map[string]any{"query": "ctftime"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "tool_reference", "tool_name": "mcp__ctftime__get_upcoming_ctfs"},
					},
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if got := toolOutputStringForTest(t, payload.Input[1].Output); !strings.Contains(got, "Tool mcp__ctftime__get_upcoming_ctfs loaded") {
		t.Fatalf("tool reference summary missing: %q", got)
	}
}

func TestBuildBackendRequestUnwrapsStructuredToolResultString(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "mcp__ctftime__get_upcoming_ctfs",
					"input": map[string]any{"days_ahead": 30},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     "{\"result\":\"# Upcoming CTF Events\\n\\n### D^3CTF 2026\"}",
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if got := toolOutputStringForTest(t, payload.Input[1].Output); got != "# Upcoming CTF Events\n\n### D^3CTF 2026" {
		t.Fatalf("structured tool result unwrap incorrect: %q", got)
	}
}

func TestBuildBackendRequestStripsToolReferenceBoundaryText(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "ToolSearch", "input": map[string]any{"query": "ctftime"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "tool_reference", "tool_name": "mcp__ctftime__get_upcoming_ctfs"},
					},
				},
				map[string]any{"type": "text", "text": "Tool loaded."},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if got := toolOutputStringForTest(t, payload.Input[1].Output); strings.Contains(got, "Tool loaded.") {
		t.Fatalf("unexpected tool boundary text in output: %q", got)
	}
}

func TestBuildBackendRequestMergesTextIntoToolResult(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Skill", "input": map[string]any{"name": "note"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Launching skill: foo"},
				map[string]any{"type": "text", "text": "follow-up details"},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 2 {
		t.Fatalf("input item count = %d, want 2", len(payload.Input))
	}
	if got := toolOutputStringForTest(t, payload.Input[1].Output); got != "Launching skill: foo\n\nfollow-up details" {
		t.Fatalf("merged tool result incorrect: %q", got)
	}
}

func TestBuildBackendRequestConvertsImageFileIDAndDocumentTextSource(t *testing.T) {
	cfg := Config{
		BackendBaseURL: "https://example.com/codex",
		BackendPath:    "/v1/responses",
		BackendAPIKey:  "test-key",
		BackendModel:   "gpt-5-codex",
	}

	req, err := NewBackendRequestForTest(context.Background(), cfg, AnthropicMessagesRequest{
		Model: "claude-sonnet-4-5",
		Messages: []AnthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":    "file",
						"file_id": "img_1",
					},
				},
				map[string]any{
					"type": "document",
					"source": map[string]any{
						"type": "text",
						"data": "hello",
					},
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var payload OpenAIResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend request: %v", err)
	}

	if len(payload.Input) != 1 {
		t.Fatalf("input item count = %d, want 1", len(payload.Input))
	}
	content := payload.Input[0].Content
	if len(content) != 2 {
		t.Fatalf("content item count = %d, want 2", len(content))
	}
	if content[0].Type != "input_image" || content[0].FileID != "img_1" {
		t.Fatalf("image file_id mapping incorrect: %#v", content[0])
	}
	if content[1].Type != "input_text" || content[1].Text != "hello" {
		t.Fatalf("document text source mapping incorrect: %#v", content[1])
	}
}
