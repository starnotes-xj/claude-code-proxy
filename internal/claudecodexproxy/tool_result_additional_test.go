package claudecodexproxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeToolSchemaAddsEmptyPropertiesForObjectSchemas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input any
	}{
		{
			name:  "nil schema",
			input: nil,
		},
		{
			name:  "plain map object",
			input: map[string]any{"type": "object"},
		},
		{
			name: "raw message map object",
			input: map[string]json.RawMessage{
				"type": json.RawMessage(`"object"`),
			},
		},
		{
			name: "struct object",
			input: struct {
				Type string `json:"type"`
			}{Type: "object"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			normalized, ok := normalizeToolSchema(tc.input).(map[string]any)
			if !ok {
				t.Fatalf("normalizeToolSchema(%T) did not return map[string]any: %#v", tc.input, normalizeToolSchema(tc.input))
			}
			if got := asString(normalized["type"]); !strings.EqualFold(got, "object") {
				t.Fatalf("type = %q, want object", got)
			}
			properties, ok := normalized["properties"].(map[string]any)
			if !ok {
				t.Fatalf("properties missing or wrong type: %#v", normalized["properties"])
			}
			if len(properties) != 0 {
				t.Fatalf("properties = %#v, want empty object", properties)
			}
		})
	}
}

func TestNormalizeToolSchemaLeavesNonObjectSchemasUntouched(t *testing.T) {
	t.Parallel()

	input := []any{"not", "an", "object"}
	got := normalizeToolSchema(input)
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("normalizeToolSchema(array) = %#v, want %#v", got, input)
	}
}

func TestConvertToolResultOutputPreservesUnsupportedRawBlocks(t *testing.T) {
	t.Parallel()

	raw := []any{
		map[string]any{
			"type":    "custom",
			"content": map[string]any{"ok": true},
		},
	}

	got := convertToolResultOutput(raw, true)
	if !reflect.DeepEqual(got, raw) {
		t.Fatalf("convertToolResultOutput preserved = %#v, want %#v", got, raw)
	}
}

func TestConvertToolResultOutputExtractsStructuredTextFromJSONString(t *testing.T) {
	t.Parallel()

	raw := `{"result":[{"type":"text","text":"hello"},{"content":[{"text":"world"}]}]}`
	got := convertToolResultOutput(raw, true)
	text, ok := got.(string)
	if !ok {
		t.Fatalf("convertToolResultOutput type = %T, want string", got)
	}
	if text != "hello\nworld" {
		t.Fatalf("convertToolResultOutput text = %q, want %q", text, "hello\nworld")
	}
}

func TestConvertToolResultOutputConvertsStructuredBlocks(t *testing.T) {
	t.Parallel()

	raw := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "json", "json": map[string]any{"ok": true}},
		map[string]any{"type": "tool_reference", "tool_name": "grep"},
	}

	got := convertToolResultOutput(raw, true)
	content, ok := got.([]OpenAIContentItem)
	if !ok {
		t.Fatalf("convertToolResultOutput type = %T, want []OpenAIContentItem", got)
	}

	want := []OpenAIContentItem{
		{Type: "input_text", Text: "hello"},
		{Type: "input_text", Text: `{"ok":true}`},
		{Type: "input_text", Text: "Tool grep loaded"},
	}
	if !reflect.DeepEqual(content, want) {
		t.Fatalf("convertToolResultOutput content = %#v, want %#v", content, want)
	}
}

func TestConvertToolResultOutputFallsBackToSummariesForImagesAndDocuments(t *testing.T) {
	t.Parallel()

	raw := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "url",
				"url":  "https://example.com/cat.png",
			},
		},
		map[string]any{
			"type":  "document",
			"title": "notes",
			"source": map[string]any{
				"type":    "file",
				"file_id": "file_123",
			},
		},
	}

	got := convertToolResultOutput(raw, true)
	text, ok := got.(string)
	if !ok {
		t.Fatalf("convertToolResultOutput type = %T, want string summary fallback", got)
	}
	for _, snippet := range []string{
		"hello",
		"[image url=https://example.com/cat.png]",
		"[document title=notes file_id=file_123]",
	} {
		if !strings.Contains(text, snippet) {
			t.Fatalf("summary fallback missing %q in %q", snippet, text)
		}
	}
}

func TestFlattenStructuredValueFlattensNestedTextCarriers(t *testing.T) {
	t.Parallel()

	value := []any{
		map[string]any{"text": " top "},
		map[string]any{
			"result": []any{
				map[string]any{"type": "text", "text": "nested"},
				map[string]any{
					"content": []any{
						map[string]any{"text": "deeper"},
						map[string]any{"text": "   "},
					},
				},
			},
		},
		map[string]any{"content": []any{map[string]any{"text": "tail"}}},
	}

	if got := flattenStructuredValue(value); got != "top\nnested\ndeeper\ntail" {
		t.Fatalf("flattenStructuredValue = %q", got)
	}
}

func TestAggregateBackendStreamMergesInterleavedOutputEvents(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: response.created",
		`data: {"response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress"}}`,
		"",
		"event: response.output_item.added",
		`data: {"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"item_id":"msg_1","content_index":0,"delta":"hel"}`,
		"",
		"event: response.output_text.done",
		`data: {"item_id":"msg_1","content_index":0,"text":"hello"}`,
		"",
		"event: response.output_item.added",
		`data: {"item":{"id":"tool_1","type":"function_call","call_id":"call_1","name":"bash"}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"tool_1","delta":"{\"command\":\"pw"}`,
		"",
		"event: response.function_call_arguments.done",
		`data: {"item_id":"tool_1","arguments":"{\"command\":\"pwd\"}"}`,
		"",
		"event: response.output_item.added",
		`data: {"item":{"id":"rs_1","type":"reasoning"}}`,
		"",
		"event: response.reasoning_summary_text.delta",
		`data: {"item_id":"rs_1","delta":"think"}`,
		"",
		"event: response.reasoning_summary_text.done",
		`data: {"item_id":"rs_1","text":"thinking done"}`,
		"",
		"event: response.completed",
		`data: {"response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":3},"output":[{"id":"tool_1","type":"function_call","call_id":"call_1","name":"bash"}]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	got, err := aggregateBackendStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregateBackendStream error = %v", err)
	}

	if got.ID != "resp_1" || got.Model != "gpt-5.4" || got.Status != "completed" {
		t.Fatalf("aggregateBackendStream response metadata = %#v", got)
	}
	if got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %#v", got.Usage)
	}
	if len(got.Output) != 3 {
		t.Fatalf("len(output) = %d, want 3; output=%#v", len(got.Output), got.Output)
	}

	if got.Output[0].ID != "msg_1" || len(got.Output[0].Content) != 1 || got.Output[0].Content[0].Text != "hello" {
		t.Fatalf("message output = %#v", got.Output[0])
	}
	if got.Output[1].ID != "tool_1" || got.Output[1].Arguments != `{"command":"pwd"}` {
		t.Fatalf("tool output = %#v", got.Output[1])
	}
	if got.Output[2].ID != "rs_1" || len(got.Output[2].Content) != 1 || got.Output[2].Content[0].Text != "thinking done" {
		t.Fatalf("reasoning output = %#v", got.Output[2])
	}
}

func TestAggregateBackendStreamReturnsBackendErrors(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: error",
		`data: {"error":{"message":"backend exploded","type":"server_error"}}`,
		"",
	}, "\n")

	_, err := aggregateBackendStream(strings.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "backend exploded") {
		t.Fatalf("aggregateBackendStream error = %v, want backend message", err)
	}
}

func TestConvertToolResultInputItemPreservesStructuredProjectionAndStatus(t *testing.T) {
	t.Parallel()

	item := convertToolResultInputItem(AnthropicContentBlock{
		ToolUseID: "toolu_1",
		IsError:   true,
		Content: []any{
			map[string]any{"type": "text", "text": "stdout"},
			map[string]any{"type": "json", "json": map[string]any{"ok": true}},
		},
	}, backendRequestOptions{PreserveStructuredOutput: true})

	if item.Type != "function_call_output" || item.CallID != "toolu_1" {
		t.Fatalf("tool_result item mapping incorrect: %#v", item)
	}
	if item.Status != "incomplete" {
		t.Fatalf("tool_result status = %q, want incomplete", item.Status)
	}
	content, ok := item.Output.([]OpenAIContentItem)
	if !ok {
		t.Fatalf("tool_result output type = %T, want []OpenAIContentItem", item.Output)
	}
	want := []OpenAIContentItem{
		{Type: "input_text", Text: "stdout"},
		{Type: "input_text", Text: `{"ok":true}`},
	}
	if !reflect.DeepEqual(content, want) {
		t.Fatalf("tool_result output = %#v, want %#v", content, want)
	}
}

func TestConvertReasoningOrCompactionInputItemUsesUnifiedCarrierBoundary(t *testing.T) {
	t.Parallel()

	reasoningCarrier := encodeReasoningCarrier(OpenAIOutputItem{
		ID:               "rs_1",
		Type:             "reasoning",
		EncryptedContent: "opaque-reasoning",
		Summary:          []OpenAIReasoningPart{{Type: "summary_text", Text: "summary"}},
	})
	compactionCarrier := encodeCompactionCarrier("cmp_1", "opaque-compaction")

	tests := []struct {
		name              string
		block             AnthropicContentBlock
		preserveReasoning bool
		wantOK            bool
		wantType          string
		wantID            string
		wantEncrypted     string
	}{
		{
			name:              "preserve reasoning from thinking signature",
			block:             AnthropicContentBlock{Type: "thinking", Signature: reasoningCarrier},
			preserveReasoning: true,
			wantOK:            true,
			wantType:          "reasoning",
			wantID:            "rs_1",
			wantEncrypted:     "opaque-reasoning",
		},
		{
			name:              "drop reasoning when reasoning preservation disabled",
			block:             AnthropicContentBlock{Type: "redacted_thinking", Data: reasoningCarrier},
			preserveReasoning: false,
			wantOK:            false,
		},
		{
			name:              "keep compaction even when reasoning preservation disabled",
			block:             AnthropicContentBlock{Type: "redacted_thinking", Data: compactionCarrier},
			preserveReasoning: false,
			wantOK:            true,
			wantType:          "compaction",
			wantID:            "cmp_1",
			wantEncrypted:     "opaque-compaction",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := convertReasoningOrCompactionInputItem(tc.block, tc.preserveReasoning)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.Type != tc.wantType || got.ID != tc.wantID || got.EncryptedContent != tc.wantEncrypted {
				t.Fatalf("item = %#v, want type=%q id=%q encrypted=%q", got, tc.wantType, tc.wantID, tc.wantEncrypted)
			}
		})
	}
}
