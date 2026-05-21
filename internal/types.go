package claudecodexproxy

import (
	"encoding/json"
	"strings"
)

type AnthropicMessagesRequest struct {
	Model         string                 `json:"model"`
	System        any                    `json:"system,omitempty"`
	Messages      []AnthropicMessage     `json:"messages"`
	Tools         []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice   `json:"tool_choice,omitempty"`
	MaxTokens     int                    `json:"max_tokens,omitempty"`
	Metadata      map[string]any         `json:"metadata,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Thinking      *AnthropicThinking     `json:"thinking,omitempty"`
	ServiceTier   string                 `json:"service_tier,omitempty"`
	OutputConfig  *AnthropicOutputConfig `json:"output_config,omitempty"`
}

type AnthropicCountTokensRequest struct {
	Model    string             `json:"model,omitempty"`
	System   any                `json:"system,omitempty"`
	Messages []AnthropicMessage `json:"messages"`
	Tools    []AnthropicTool    `json:"tools,omitempty"`
}

type AnthropicThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicContentBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	CacheControl map[string]any   `json:"cache_control,omitempty"`
	ToolName     string           `json:"tool_name,omitempty"`
	ID           string           `json:"id,omitempty"`
	Name         string           `json:"name,omitempty"`
	Input        json.RawMessage  `json:"input,omitempty"`
	ToolUseID    string           `json:"tool_use_id,omitempty"`
	IsError      bool             `json:"is_error,omitempty"`
	JSON         any              `json:"json,omitempty"`
	Content      any              `json:"content,omitempty"`
	Source       *AnthropicSource `json:"source,omitempty"`
	Title        string           `json:"title,omitempty"`
	Context      string           `json:"context,omitempty"`
	Thinking     string           `json:"thinking,omitempty"`
	Signature    string           `json:"signature,omitempty"`
	Data         string           `json:"data,omitempty"`
}

type AnthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

type AnthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type AnthropicMessageResponse struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Role         string                   `json:"role"`
	Model        string                   `json:"model"`
	Content      []AnthropicResponseBlock `json:"content"`
	StopReason   string                   `json:"stop_reason"`
	StopSequence *string                  `json:"stop_sequence"`
	Usage        AnthropicUsage           `json:"usage"`
}

type AnthropicResponseBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type AnthropicCountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

type AnthropicErrorEnvelope struct {
	Type  string            `json:"type"`
	Error AnthropicAPIError `json:"error"`
}

type AnthropicAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type OpenAIResponsesRequest struct {
	Model             string                        `json:"model"`
	Instructions      string                        `json:"instructions,omitempty"`
	Input             []OpenAIInputItem             `json:"input"`
	Include           []string                      `json:"include,omitempty"`
	ContextManagement []OpenAIContextManagementItem `json:"context_management,omitempty"`
	Tools             []OpenAITool                  `json:"tools,omitempty"`
	ToolChoice        any                           `json:"tool_choice,omitempty"`
	MaxOutputTokens   int                           `json:"max_output_tokens,omitempty"`
	Temperature       *float64                      `json:"temperature,omitempty"`
	TopP              *float64                      `json:"top_p,omitempty"`
	Metadata          map[string]string             `json:"metadata,omitempty"`
	PromptCacheKey    string                        `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls *bool                         `json:"parallel_tool_calls,omitempty"`
	Store             bool                          `json:"store"`
	Stream            bool                          `json:"stream,omitempty"`
	Reasoning         *OpenAIReasoning              `json:"reasoning,omitempty"`
}

type OpenAIContextManagementItem struct {
	Type             string `json:"type"`
	CompactThreshold int    `json:"compact_threshold,omitempty"`
}

type OpenAIReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type OpenAIInputItem struct {
	ID               string                `json:"id,omitempty"`
	Type             string                `json:"type,omitempty"`
	Role             string                `json:"role,omitempty"`
	Content          []OpenAIContentItem   `json:"content,omitempty"`
	Phase            string                `json:"phase,omitempty"`
	Name             string                `json:"name,omitempty"`
	Arguments        string                `json:"arguments,omitempty"`
	CallID           string                `json:"call_id,omitempty"`
	Output           any                   `json:"output,omitempty"`
	EncryptedContent string                `json:"encrypted_content,omitempty"`
	Summary          []OpenAIReasoningPart `json:"summary,omitempty"`
	Status           string                `json:"status,omitempty"`
}

func (i OpenAIInputItem) MarshalJSON() ([]byte, error) {
	type alias OpenAIInputItem
	if strings.EqualFold(strings.TrimSpace(i.Type), "reasoning") {
		type reasoningAlias struct {
			ID               string                `json:"id,omitempty"`
			Type             string                `json:"type,omitempty"`
			Role             string                `json:"role,omitempty"`
			Content          []OpenAIContentItem   `json:"content,omitempty"`
			Phase            string                `json:"phase,omitempty"`
			Name             string                `json:"name,omitempty"`
			Arguments        string                `json:"arguments,omitempty"`
			CallID           string                `json:"call_id,omitempty"`
			Output           any                   `json:"output,omitempty"`
			EncryptedContent string                `json:"encrypted_content,omitempty"`
			Summary          []OpenAIReasoningPart `json:"summary"`
			Status           string                `json:"status,omitempty"`
		}
		aux := reasoningAlias{
			ID:               i.ID,
			Type:             i.Type,
			Role:             i.Role,
			Content:          i.Content,
			Phase:            i.Phase,
			Name:             i.Name,
			Arguments:        i.Arguments,
			CallID:           i.CallID,
			Output:           i.Output,
			EncryptedContent: i.EncryptedContent,
			Summary:          i.Summary,
			Status:           i.Status,
		}
		if aux.Summary == nil {
			aux.Summary = []OpenAIReasoningPart{}
		}
		return json.Marshal(aux)
	}
	return json.Marshal(alias(i))
}

type OpenAIContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type OpenAIReasoningPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type OpenAITool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
}

type OpenAIResponsesResponse struct {
	ID                string             `json:"id"`
	Model             string             `json:"model"`
	Status            string             `json:"status,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Output            []OpenAIOutputItem `json:"output"`
	Usage             OpenAIUsage        `json:"usage"`
	Error             *OpenAIError       `json:"error,omitempty"`
}

type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type OpenAIOutputItem struct {
	ID               string                `json:"id,omitempty"`
	Type             string                `json:"type"`
	Role             string                `json:"role,omitempty"`
	Phase            string                `json:"phase,omitempty"`
	Name             string                `json:"name,omitempty"`
	CallID           string                `json:"call_id,omitempty"`
	Arguments        string                `json:"arguments,omitempty"`
	Content          []OpenAIOutputContent `json:"content,omitempty"`
	EncryptedContent string                `json:"encrypted_content,omitempty"`
	Summary          []OpenAIReasoningPart `json:"summary,omitempty"`
	Status           string                `json:"status,omitempty"`
}

type OpenAIOutputContent struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Phase string `json:"phase,omitempty"`
}

type OpenAIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    any    `json:"code,omitempty"`
}

type OpenAIStreamEnvelope struct {
	Type  string           `json:"type"`
	Error *OpenAIError     `json:"error,omitempty"`
	Item  *OpenAIEventItem `json:"item,omitempty"`
	Part  *OpenAIEventPart `json:"part,omitempty"`

	OutputIndex  int    `json:"output_index,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	Delta        string `json:"delta,omitempty"`
	Arguments    string `json:"arguments,omitempty"`
	Text         string `json:"text,omitempty"`

	Response *OpenAIResponsesResponse `json:"response,omitempty"`
}

type OpenAIEventItem struct {
	ID               string                `json:"id,omitempty"`
	Type             string                `json:"type,omitempty"`
	Role             string                `json:"role,omitempty"`
	Phase            string                `json:"phase,omitempty"`
	Name             string                `json:"name,omitempty"`
	CallID           string                `json:"call_id,omitempty"`
	Arguments        string                `json:"arguments,omitempty"`
	Content          []OpenAIOutputContent `json:"content,omitempty"`
	EncryptedContent string                `json:"encrypted_content,omitempty"`
	Summary          []OpenAIReasoningPart `json:"summary,omitempty"`
	Status           string                `json:"status,omitempty"`
}

type OpenAIEventPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}
