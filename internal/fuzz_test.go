package claudecodexproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzNormalizeToolSchema(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"type":"object"}`),
		[]byte(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		[]byte(`{"type":"array","items":{"type":"string"}}`),
		[]byte(`["not","an","object"]`),
		[]byte(`not-json`),
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, data []byte, preferRawMap bool) {
		input := fuzzJSONLikeValue(data, preferRawMap)
		got := normalizeToolSchema(input)

		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("normalizeToolSchema(%T) returned non-marshalable value %#v: %v", input, got, err)
		}
		if normalized, ok := got.(map[string]any); ok && strings.EqualFold(asString(normalized["type"]), "object") {
			if _, ok := normalized["properties"]; !ok {
				t.Fatalf("object schema missing properties: %#v", normalized)
			}
		}
	})
}

func FuzzConvertToolResultOutput(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"result":[{"type":"text","text":"hello"}]}`),
		[]byte(`[{"type":"text","text":"hi"},{"type":"tool_reference","tool_name":"grep"}]`),
		[]byte(`[{"type":"json","content":{"ok":true}}]`),
		[]byte(`plain text`),
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, data []byte, allowStructured bool) {
		input := fuzzJSONLikeValue(data, false)
		got := convertToolResultOutput(input, allowStructured)

		if !allowStructured {
			if _, ok := got.(string); !ok {
				t.Fatalf("allowStructured=false returned %T, want string", got)
			}
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("convertToolResultOutput returned non-marshalable value %#v: %v", got, err)
		}
	})
}

func FuzzAggregateBackendStream(f *testing.F) {
	for _, seed := range []string{
		"data: [DONE]\n\n",
		"event: response.created\ndata: {\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"item_id\":\"msg_1\",\"delta\":\"hi\"}\n\n",
		"event: error\ndata: {\"error\":{\"message\":\"boom\"}}\n\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		resp, err := aggregateBackendStream(strings.NewReader(body))
		if err == nil {
			if _, marshalErr := json.Marshal(resp); marshalErr != nil {
				t.Fatalf("aggregateBackendStream returned non-marshalable response %#v: %v", resp, marshalErr)
			}
		}
	})
}

func fuzzJSONLikeValue(data []byte, preferRawMap bool) any {
	if preferRawMap {
		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(data, &rawMap); err == nil {
			return rawMap
		}
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err == nil {
		return decoded
	}
	return string(data)
}
