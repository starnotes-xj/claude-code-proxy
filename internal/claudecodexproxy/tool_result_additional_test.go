package claudecodexproxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type flushFunc func()

func (f flushFunc) Flush() { f() }

var _ http.Flusher = flushFunc(nil)

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

func TestSanitizeClaudeCodeToolInputRemovesEmptyReadPages(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"file_path": "/tmp/example.txt",
		"pages":     "",
	}
	sanitizeClaudeCodeToolInput("Read", input)

	if _, ok := input["pages"]; ok {
		t.Fatalf("empty Read pages was not removed: %#v", input)
	}
	if got := input["file_path"]; got != "/tmp/example.txt" {
		t.Fatalf("file_path = %#v, want preserved path", got)
	}
}

func TestSanitizeClaudeCodeToolInputPreservesNonEmptyReadPages(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"file_path": "/tmp/example.pdf",
		"pages":     "1-2",
	}
	sanitizeClaudeCodeToolInput("Read", input)

	if got := input["pages"]; got != "1-2" {
		t.Fatalf("pages = %#v, want preserved range", got)
	}
}

func TestSanitizeClaudeCodeToolInputOnlyAppliesToReadAndWrite(t *testing.T) {
	t.Parallel()

	input := map[string]any{"pages": ""}
	sanitizeClaudeCodeToolInput("OtherTool", input)

	if got, ok := input["pages"]; !ok || got != "" {
		t.Fatalf("non-Read/Write pages changed: %#v", input)
	}
}

func TestSanitizeClaudeCodeToolInputRemovesEmptyWritePages(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"file_path": "/tmp/example.txt",
		"content":   "hello",
		"pages":     "",
	}
	sanitizeClaudeCodeToolInput("Write", input)

	if _, ok := input["pages"]; ok {
		t.Fatalf("empty Write pages was not removed: %#v", input)
	}
	if got := input["file_path"]; got != "/tmp/example.txt" {
		t.Fatalf("file_path = %#v, want preserved path", got)
	}
}

func TestSanitizeClaudeCodeToolArgumentsRemovesEmptyWritePages(t *testing.T) {
	t.Parallel()

	got := sanitizeClaudeCodeToolArguments("Write", `{"file_path":"/tmp/example.txt","content":"hello","pages":""}`)
	var input map[string]any
	if err := json.Unmarshal([]byte(got), &input); err != nil {
		t.Fatalf("sanitized arguments are not JSON: %v", err)
	}
	if _, ok := input["pages"]; ok {
		t.Fatalf("empty Write pages was not removed from arguments: %#v", input)
	}
	if got := input["file_path"]; got != "/tmp/example.txt" {
		t.Fatalf("file_path = %#v, want preserved path", got)
	}
}

func TestSSETranslatorBuffersWriteDeltasAndRemovesEmptyPages(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"id":"fc_write","type":"function_call","name":"Write","call_id":"toolu_write"}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_write","delta":"{\"file_path\":\"/tmp/example.txt\",\"content\":\"hello\",\"pages\":\"\"}"}`,
		"",
		"event: response.function_call_arguments.done",
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_write"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_write","output":[{"id":"fc_write","type":"function_call","call_id":"toolu_write","name":"Write","arguments":"{\"file_path\":\"/tmp/example.txt\",\"content\":\"hello\",\"pages\":\"\"}"}],"usage":{"input_tokens":7,"output_tokens":3}}}`,
		"",
	}, "\n")

	var out strings.Builder
	translator := newSSETranslator(&out, flushFunc(func() {}), "claude-sonnet-4-5", "msg_write", nil)
	if err := translator.consume(strings.NewReader(stream)); err != nil {
		t.Fatalf("consume stream: %v", err)
	}

	body := out.String()
	if strings.Contains(body, `\"pages\":\"\"`) || strings.Contains(body, `"pages":""`) {
		t.Fatalf("stream leaked empty Write pages:\n%s", body)
	}
	if !strings.Contains(body, `\"file_path\":\"/tmp/example.txt\"`) && !strings.Contains(body, `"file_path":"/tmp/example.txt"`) {
		t.Fatalf("stream missing sanitized Write file_path:\n%s", body)
	}
}

func TestSanitizeClaudeCodeToolArgumentsRemovesEmptyReadPages(t *testing.T) {
	t.Parallel()

	got := sanitizeClaudeCodeToolArguments("Read", `{"file_path":"/tmp/example.txt","pages":""}`)
	var input map[string]any
	if err := json.Unmarshal([]byte(got), &input); err != nil {
		t.Fatalf("sanitized arguments are not JSON: %v", err)
	}
	if _, ok := input["pages"]; ok {
		t.Fatalf("empty Read pages was not removed from arguments: %#v", input)
	}
	if got := input["file_path"]; got != "/tmp/example.txt" {
		t.Fatalf("file_path = %#v, want preserved path", got)
	}
}

func TestSSETranslatorBuffersReadDeltasAndRemovesEmptyPages(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"id":"fc_read","type":"function_call","name":"Read","call_id":"toolu_read"}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_read","delta":"{\"file_path\":\"/tmp/example.txt\",\"pages\":\"\"}"}`,
		"",
		"event: response.function_call_arguments.done",
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_read"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_read","output":[{"id":"fc_read","type":"function_call","call_id":"toolu_read","name":"Read","arguments":"{\"file_path\":\"/tmp/example.txt\",\"pages\":\"\"}"}],"usage":{"input_tokens":7,"output_tokens":3}}}`,
		"",
	}, "\n")

	var out strings.Builder
	translator := newSSETranslator(&out, flushFunc(func() {}), "claude-sonnet-4-5", "msg_read", nil)
	if err := translator.consume(strings.NewReader(stream)); err != nil {
		t.Fatalf("consume stream: %v", err)
	}

	body := out.String()
	if strings.Contains(body, `\"pages\":\"\"`) || strings.Contains(body, `"pages":""`) {
		t.Fatalf("stream leaked empty Read pages:\n%s", body)
	}
	if !strings.Contains(body, `\"file_path\":\"/tmp/example.txt\"`) && !strings.Contains(body, `"file_path":"/tmp/example.txt"`) {
		t.Fatalf("stream missing sanitized Read file_path:\n%s", body)
	}
}

func TestConvertToolUseInputItemRemovesEmptyReadPages(t *testing.T) {
	t.Parallel()

	item := convertToolUseInputItem(AnthropicContentBlock{
		Type:  "tool_use",
		Name:  "Read",
		ID:    "toolu_read_1",
		Input: json.RawMessage(`{"file_path":"/tmp/example.txt","pages":""}`),
	})

	if item.Type != "function_call" || item.Name != "Read" {
		t.Fatalf("item = %#v, want Read function_call", item)
	}
	if strings.Contains(item.Arguments, `"pages"`) {
		t.Fatalf("empty Read pages was not removed from input arguments: %s", item.Arguments)
	}
	if !strings.Contains(item.Arguments, `"file_path"`) {
		t.Fatalf("file_path missing from arguments: %s", item.Arguments)
	}
}

func TestConvertToolUseInputItemPreservesNonEmptyReadPages(t *testing.T) {
	t.Parallel()

	item := convertToolUseInputItem(AnthropicContentBlock{
		Type:  "tool_use",
		Name:  "Read",
		ID:    "toolu_read_2",
		Input: json.RawMessage(`{"file_path":"/tmp/example.pdf","pages":"1-2"}`),
	})

	if !strings.Contains(item.Arguments, `"pages":"1-2"`) {
		t.Fatalf("non-empty Read pages was removed: %s", item.Arguments)
	}
}

func TestConvertToolUseInputItemOnlySanitizesReadAndWrite(t *testing.T) {
	t.Parallel()

	item := convertToolUseInputItem(AnthropicContentBlock{
		Type:  "tool_use",
		Name:  "Bash",
		ID:    "toolu_bash_1",
		Input: json.RawMessage(`{"command":"ls","pages":""}`),
	})

	if !strings.Contains(item.Arguments, `"pages":""`) {
		t.Fatalf("non-Read/Write tool pages was incorrectly removed: %s", item.Arguments)
	}
}

func TestConvertToolUseInputItemRemovesEmptyWritePages(t *testing.T) {
	t.Parallel()

	item := convertToolUseInputItem(AnthropicContentBlock{
		Type:  "tool_use",
		Name:  "Write",
		ID:    "toolu_write_1",
		Input: json.RawMessage(`{"file_path":"/tmp/example.txt","content":"hello","pages":""}`),
	})

	if strings.Contains(item.Arguments, `"pages"`) {
		t.Fatalf("empty Write pages was not removed from input arguments: %s", item.Arguments)
	}
	if !strings.Contains(item.Arguments, `"file_path"`) {
		t.Fatalf("file_path missing from arguments: %s", item.Arguments)
	}
}

func TestConvertToolUseInputItemHandlesEmptyInput(t *testing.T) {
	t.Parallel()

	item := convertToolUseInputItem(AnthropicContentBlock{
		Type:  "tool_use",
		Name:  "Read",
		ID:    "toolu_read_3",
		Input: nil,
	})

	if item.Arguments != "{}" {
		t.Fatalf("empty input should become {}, got: %s", item.Arguments)
	}
}
