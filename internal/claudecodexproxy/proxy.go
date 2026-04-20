package claudecodexproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	opaqueReasoningPrefix     = "ccp-reasoning-v1:"
	compactionCarrierPrefix   = "cm1#"
	compactionCarrierSep      = "@"
	defaultThinkingText       = "Thinking..."
	maxToolArgumentBytes      = 256 * 1024
	maxToolEmptyDeltaCount    = 8
	capabilityReprobeLeaseTTL = 15 * time.Second
)

const (
	compactNone = iota
	compactRequest
	compactAutoContinue
)

const (
	compactSystemPromptStart          = "You are a helpful AI assistant tasked with summarizing conversations"
	compactTextOnlyGuard              = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."
	compactSummaryPromptStart         = "Your task is to create a detailed summary of the conversation so far"
	compactAutoContinueClaudePrompt   = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation."
	compactAutoContinueOpenCodePrompt = "Continue if you have next steps, or stop and ask for clarification if you are unsure how to proceed."
	ideExecuteCodeTool                = "mcp__ide__executeCode"
	ideGetDiagnosticsTool             = "mcp__ide__getDiagnostics"
	ideGetDiagnosticsDescription      = "Get language diagnostics from the IDE. Returns errors, warnings, information, and hints for files in the workspace."
)

var (
	compactAutoContinuePromptStarts = []string{
		compactAutoContinueClaudePrompt,
		compactAutoContinueOpenCodePrompt,
	}
	compactMessageSections = []string{"Pending Tasks:", "Current Work:"}
)

type Proxy struct {
	cfg              Config
	httpClient       *http.Client
	idCounter        uint64
	capsMu           sync.RWMutex
	caps             runtimeCapabilities
	scopedCaps       map[string]scopedRuntimeCapabilities
	unsupportedUntil map[string]time.Time
	reprobeUntil     map[string]time.Time
	now              func() time.Time
}

type capabilityState uint8

const (
	capabilityUnknown capabilityState = iota
	capabilitySupported
	capabilityUnsupported
)

type runtimeCapabilities struct {
	Metadata        capabilityState
	SupportedModels map[string]struct{}
	ModelProfiles   map[string]backendModelProfile
	PreferredModel  string
	WarmupModel     string
	ModelsFetched   bool
}

type backendModelProfile struct {
	Vendor                    string
	Family                    string
	EndpointFamily            string
	HasExplicitFamily         bool
	SupportsAdaptiveThinking  *bool
	SupportsStreaming         *bool
	SupportsStructuredOutput  *bool
	SupportsToolCalls         *bool
	SupportsParallelToolCalls *bool
	SupportsPhase             *bool
	MinThinkingBudget         int
	MaxThinkingBudget         int
	MaxPromptTokens           int
	MaxContextWindowTokens    int
	MaxOutputTokens           int
	SupportedEndpoints        map[string]struct{}
}

type backendCapabilityPreset struct {
	DisableReasoningInclude  bool
	DisablePhase             bool
	DisablePromptCacheKey    bool
	DisableContextManagement bool
	DisableCompactionInput   bool
}

type scopedRuntimeCapabilities struct {
	StructuredToolOutput capabilityState
	BackendStreaming     capabilityState
	ContextManagement    capabilityState
	CompactionInput      capabilityState
	Reasoning            capabilityState
	ReasoningInclude     capabilityState
	Phase                capabilityState
	ModelPassthrough     capabilityState
	PromptCacheKey       capabilityState
	InputItemPersistence capabilityState
}

type subagentMarker struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

type backendRequestOptions struct {
	RequestScopeKey                   string
	BackendCapabilityScopeKey         string
	Model                             string
	MaxOutputTokens                   int
	Reasoning                         *OpenAIReasoning
	ContextManagementCompactThreshold int
	EnableMetadata                    bool
	EnableReasoning                   bool
	EnableReasoningInclude            bool
	EnablePhaseCommentary             bool
	EnablePromptCacheKey              bool
	EnableContextManagement           bool
	PreserveCompactionInput           bool
	PreserveStructuredOutput          bool
	PreserveReasoningItems            bool
	EnableBackendStreaming            bool
	EnableParallelToolCalls           bool
	StripRoundTripItemIDs             bool
	UsesRequestModelPassthrough       bool
}

type continuityContext struct {
	RootSessionID    string
	RequestID        string
	PromptCacheKey   string
	SessionAffinity  string
	ParentSessionID  string
	InboundRequestID string
	TraceID          string
	InteractionType  string
	InteractionID    string
	Subagent         *subagentMarker
}

type requestFeatures struct {
	hasVisionInput        bool
	hasToolResultHistory  bool
	hasCompactionCarrier  bool
	hasRoundTripHistory   bool
	hasSystemInstructions bool
	warmupCandidateText   string
}

func New(cfg Config) *Proxy {
	return &Proxy{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		caps: runtimeCapabilities{
			SupportedModels: map[string]struct{}{},
			ModelProfiles:   map[string]backendModelProfile{},
		},
		scopedCaps:       map[string]scopedRuntimeCapabilities{},
		unsupportedUntil: map[string]time.Time{},
		reprobeUntil:     map[string]time.Time{},
		now:              time.Now,
	}
}

func NewBackendRequestForTest(ctx context.Context, cfg Config, req AnthropicMessagesRequest, headers http.Header) (*http.Request, error) {
	return New(cfg).buildBackendRequest(ctx, req, headers)
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", p.handleHealthz)
	mux.HandleFunc("/v1/messages", p.requireClientAuth(p.handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", p.requireClientAuth(p.handleCountTokens))
	mux.HandleFunc("/v1/models", p.requireClientAuth(p.handleModels))
	return mux
}

func (p *Proxy) requireClientAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.isAuthorizedClient(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="claude-codex-proxy"`)
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing client API key")
			return
		}
		next(w, r)
	}
}

func (p *Proxy) isAuthorizedClient(r *http.Request) bool {
	expected := strings.TrimSpace(p.cfg.ClientAPIKey)
	if expected == "" {
		return true
	}

	if provided := strings.TrimSpace(r.Header.Get("x-api-key")); secureSecretCompare(provided, expected) {
		return true
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	return ok && secureSecretCompare(token, expected)
}

func bearerToken(raw string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", false
	}
	return fields[1], true
}

func secureSecretCompare(provided, expected string) bool {
	if len(provided) == 0 || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (p *Proxy) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"backend_url": p.cfg.BackendURL(),
	})
}

func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	if payload, ok := p.fetchBackendModels(); ok {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeJSON(w, http.StatusOK, p.syntheticModelsPayload())
}

func (p *Proxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req AnthropicCountTokensRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	preprocessed := AnthropicMessagesRequest{
		System:   req.System,
		Messages: req.Messages,
		Tools:    req.Tools,
	}
	preprocessMessagesForClaude(&preprocessed, preprocessOptions{compactType: detectCompactType(AnthropicMessagesRequest{
		System:   req.System,
		Messages: req.Messages,
	})})

	req.System = preprocessed.System
	req.Messages = preprocessed.Messages
	if exact, ok := p.countTokensViaAnthropic(r.Context(), req); ok {
		writeJSON(w, http.StatusOK, AnthropicCountTokensResponse{InputTokens: exact})
		return
	}

	inputTokens := estimateInputTokens(req.System, req.Messages, req.Tools)
	toolOverhead := 0
	multiplier := 1.0
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.Model)), "claude") {
		if shouldAddClaudeToolOverhead(req.Tools, strings.TrimSpace(r.Header.Get("anthropic-beta"))) {
			toolOverhead = 346
			inputTokens += toolOverhead
		}
		multiplier = p.cfg.ClaudeTokenMultiplier
		inputTokens = int(math.Round(float64(inputTokens) * multiplier))
	}
	p.debugf("count_tokens fallback model=%q estimated_tokens=%d tool_overhead=%d multiplier=%.2f", req.Model, inputTokens, toolOverhead, multiplier)

	writeJSON(w, http.StatusOK, AnthropicCountTokensResponse{
		InputTokens: inputTokens,
	})
}

func (p *Proxy) countTokensViaAnthropic(ctx context.Context, req AnthropicCountTokensRequest) (int, bool) {
	model := strings.TrimSpace(req.Model)
	if !strings.HasPrefix(strings.ToLower(model), "claude") {
		return 0, false
	}
	if strings.TrimSpace(p.cfg.AnthropicAPIKey) == "" || strings.TrimSpace(p.cfg.AnthropicAPIBaseURL) == "" {
		return 0, false
	}

	body, err := json.Marshal(AnthropicCountTokensRequest{
		Model:    strings.ReplaceAll(model, ".", "-"),
		System:   req.System,
		Messages: req.Messages,
		Tools:    req.Tools,
	})
	if err != nil {
		p.debugf("anthropic count_tokens marshal failed: %v", err)
		return 0, false
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.AnthropicAPIBaseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		p.debugf("anthropic count_tokens request build failed: %v", err)
		return 0, false
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.cfg.AnthropicAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "token-counting-2024-11-01")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		p.debugf("anthropic count_tokens request failed: %v", err)
		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		p.debugf("anthropic count_tokens response status=%d body=%s", resp.StatusCode, sanitizeLogValue(string(body)))
		return 0, false
	}

	var result AnthropicCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		p.debugf("anthropic count_tokens decode failed: %v", err)
		return 0, false
	}
	p.debugf("anthropic count_tokens success model=%q input_tokens=%d", model, result.InputTokens)
	return result.InputTokens, true
}

func (p *Proxy) fetchBackendModels() (map[string]any, bool) {
	req, err := http.NewRequest(http.MethodGet, p.cfg.BackendModelsURL(), nil)
	if err != nil {
		p.debugf("backend models request build failed: %v", err)
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.BackendAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.debugf("backend models request failed: %v", err)
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		p.debugf("backend models response status=%d body=%s", resp.StatusCode, sanitizeLogValue(string(body)))
		return nil, false
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		p.debugf("backend models decode failed: %v", err)
		return nil, false
	}

	data, ok := payload["data"].([]any)
	if !ok || len(data) == 0 {
		p.debugf("backend models payload missing data array")
		return nil, false
	}

	normalized := make([]map[string]any, 0, len(data))
	for _, item := range data {
		model, ok := item.(map[string]any)
		if !ok {
			continue
		}
		normalized = append(normalized, normalizeModelDescriptor(model))
	}
	if len(normalized) == 0 {
		return nil, false
	}
	p.seedCapabilitiesFromModels(normalized)

	return map[string]any{
		"object":   firstNonEmpty(asString(payload["object"]), "list"),
		"has_more": false,
		"data":     normalized,
	}, true
}

func (p *Proxy) seedCapabilitiesFromModels(models []map[string]any) {
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	p.caps.ModelsFetched = true
	p.caps.SupportedModels = map[string]struct{}{}
	p.caps.ModelProfiles = map[string]backendModelProfile{}
	p.caps.PreferredModel = ""
	p.caps.WarmupModel = ""
	for _, model := range models {
		id := strings.TrimSpace(asString(model["id"]))
		if id == "" {
			continue
		}
		if p.caps.PreferredModel == "" {
			p.caps.PreferredModel = id
		}
		p.caps.SupportedModels[id] = struct{}{}
		profile := extractBackendModelProfile(model)
		p.caps.ModelProfiles[id] = profile
		if p.caps.WarmupModel == "" && isWarmupCandidate(id) && profileSupportsResponses(profile) {
			p.caps.WarmupModel = id
		}
	}
}

func extractBackendModelProfile(model map[string]any) backendModelProfile {
	id := strings.TrimSpace(asString(model["id"]))
	profile := backendModelProfile{
		Vendor:             firstNonEmpty(strings.TrimSpace(asString(model["vendor"])), strings.TrimSpace(asString(model["owned_by"])), inferModelVendor(id)),
		SupportedEndpoints: map[string]struct{}{},
	}
	switch endpoints := model["supported_endpoints"].(type) {
	case []any:
		for _, endpoint := range endpoints {
			if value := strings.TrimSpace(asString(endpoint)); value != "" {
				profile.SupportedEndpoints[value] = struct{}{}
			}
		}
	case []string:
		for _, endpoint := range endpoints {
			if value := strings.TrimSpace(endpoint); value != "" {
				profile.SupportedEndpoints[value] = struct{}{}
			}
		}
	}
	capabilities, _ := model["capabilities"].(map[string]any)
	explicitFamily := strings.TrimSpace(asString(capabilities["family"]))
	profile.Family = firstNonEmpty(explicitFamily, inferModelFamily(id))
	profile.HasExplicitFamily = explicitFamily != ""
	profile.EndpointFamily = inferEndpointFamily(profile.SupportedEndpoints)
	supports, _ := capabilities["supports"].(map[string]any)
	profile.SupportsAdaptiveThinking = asOptionalBool(supports["adaptive_thinking"])
	profile.SupportsStreaming = asOptionalBool(supports["streaming"])
	profile.SupportsStructuredOutput = asOptionalBool(supports["structured_outputs"])
	profile.SupportsToolCalls = asOptionalBool(supports["tool_calls"])
	profile.SupportsParallelToolCalls = asOptionalBool(supports["parallel_tool_calls"])
	profile.SupportsPhase = asOptionalBool(supports["phase"])
	profile.MinThinkingBudget = asPositiveInt(supports["min_thinking_budget"])
	profile.MaxThinkingBudget = asPositiveInt(supports["max_thinking_budget"])
	if limits, ok := capabilities["limits"].(map[string]any); ok {
		profile.MaxPromptTokens = asPositiveInt(limits["max_prompt_tokens"])
		profile.MaxContextWindowTokens = asPositiveInt(limits["max_context_window_tokens"])
		profile.MaxOutputTokens = asPositiveInt(limits["max_output_tokens"])
	}
	return profile
}

func inferEndpointFamily(endpoints map[string]struct{}) string {
	if len(endpoints) == 0 {
		return "unknown"
	}
	families := map[string]struct{}{}
	for endpoint := range endpoints {
		if family := normalizeEndpointFamily(endpoint); family != "" {
			families[family] = struct{}{}
		}
	}
	if len(families) == 0 {
		return "unknown"
	}
	if len(families) > 1 {
		return "mixed"
	}
	for family := range families {
		return family
	}
	return "unknown"
}

func normalizeEndpointFamily(endpoint string) string {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "/responses", "/v1/responses":
		return "responses"
	case "/messages", "/v1/messages":
		return "messages"
	case "/chat/completions", "/v1/chat/completions":
		return "chat_completions"
	default:
		return ""
	}
}

func profileCapabilityPreset(profile backendModelProfile) backendCapabilityPreset {
	if profile.EndpointFamily != "responses" || !profile.HasExplicitFamily {
		return backendCapabilityPreset{}
	}
	if strings.ToLower(strings.TrimSpace(profile.Vendor)) != "openai" {
		return backendCapabilityPreset{}
	}
	return backendCapabilityPreset{
		DisableReasoningInclude:  true,
		DisablePhase:             true,
		DisablePromptCacheKey:    true,
		DisableContextManagement: true,
		DisableCompactionInput:   true,
	}
}

func shouldPresetDisableFeature(p *Proxy, scopeKey, feature string, state capabilityState, disabled bool) bool {
	if !disabled || state != capabilityUnknown {
		return false
	}
	if p == nil || p.cfg.CapabilityReprobeTTL <= 0 || strings.TrimSpace(scopeKey) == "" {
		return true
	}
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	now := p.now()
	leaseKey := capabilityReprobeLeaseKey(scopeKey, "preset:"+feature)
	until, ok := p.reprobeUntil[leaseKey]
	if !ok {
		p.reprobeUntil[leaseKey] = now.Add(p.cfg.CapabilityReprobeTTL)
		return true
	}
	if now.Before(until) {
		return true
	}
	p.reprobeUntil[leaseKey] = now.Add(p.cfg.CapabilityReprobeTTL)
	return false
}

func isWarmupCandidate(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "mini"),
		strings.Contains(lower, "nano"),
		strings.Contains(lower, "haiku"),
		strings.Contains(lower, "small"):
		return true
	default:
		return false
	}
}

func profileSupportsResponses(profile backendModelProfile) bool {
	if len(profile.SupportedEndpoints) == 0 {
		return true
	}
	return profile.EndpointFamily == "responses" || profile.EndpointFamily == "mixed"
}

func (p *Proxy) syntheticModelsPayload() map[string]any {
	model := p.cfg.AdvertisedModel("")
	return map[string]any{
		"object":   "list",
		"has_more": false,
		"data": []map[string]any{
			normalizeModelDescriptor(map[string]any{"id": model}),
		},
	}
}

func normalizeModelDescriptor(model map[string]any) map[string]any {
	id := firstNonEmpty(asString(model["id"]), asString(model["name"]), "claude-code-proxy")
	model["id"] = id
	model["object"] = firstNonEmpty(asString(model["object"]), "model")
	model["type"] = firstNonEmpty(asString(model["type"]), "model")
	model["name"] = firstNonEmpty(asString(model["name"]), id)
	model["display_name"] = firstNonEmpty(asString(model["display_name"]), asString(model["name"]), id)
	if _, ok := model["created"]; !ok {
		model["created"] = 0
	}
	if _, ok := model["created_at"]; !ok {
		model["created_at"] = time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	model["owned_by"] = firstNonEmpty(asString(model["owned_by"]), asString(model["vendor"]), inferModelVendor(id))
	model["vendor"] = firstNonEmpty(asString(model["vendor"]), inferModelVendor(id))
	model["version"] = firstNonEmpty(asString(model["version"]), "proxy")
	if _, ok := model["preview"]; !ok {
		model["preview"] = false
	}
	if _, ok := model["model_picker_enabled"]; !ok {
		model["model_picker_enabled"] = true
	}
	if _, ok := model["supported_endpoints"]; !ok {
		model["supported_endpoints"] = []string{"/v1/messages"}
	}
	if _, ok := model["capabilities"]; !ok {
		model["capabilities"] = map[string]any{
			"family":    inferModelFamily(id),
			"object":    "capabilities",
			"type":      "capabilities",
			"tokenizer": "proxy",
			"limits": map[string]any{
				"max_context_window_tokens": 272000,
				"max_prompt_tokens":         272000,
				"max_output_tokens":         128000,
			},
			"supports": map[string]any{
				"tool_calls":          true,
				"parallel_tool_calls": true,
				"streaming":           true,
				"structured_outputs":  true,
				"vision":              true,
				"adaptive_thinking":   true,
			},
		}
	}
	return model
}

func shouldAddClaudeToolOverhead(tools []AnthropicTool, anthropicBetaHeader string) bool {
	if len(tools) == 0 || strings.TrimSpace(anthropicBetaHeader) == "" {
		return false
	}
	if len(tools) == 1 && tools[0].Name == "Skill" {
		return false
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "mcp__") {
			return false
		}
	}
	return true
}

func (p *Proxy) resolveBackendReasoning(req AnthropicMessagesRequest) *OpenAIReasoning {
	if req.ToolChoice != nil {
		switch strings.ToLower(strings.TrimSpace(req.ToolChoice.Type)) {
		case "any", "tool":
			return nil
		}
	}
	if effort := mapBackendReasoningEffort(p.cfg.BackendReasoningEffort); effort != "" {
		return &OpenAIReasoning{Effort: effort}
	}
	if req.OutputConfig != nil {
		if effort := mapBackendReasoningEffort(req.OutputConfig.Effort); effort != "" {
			return &OpenAIReasoning{Effort: effort}
		}
	}
	if req.Thinking != nil {
		if effort := inferReasoningEffortFromThinking(*req.Thinking); effort != "" {
			return &OpenAIReasoning{Effort: effort}
		}
	}
	return nil
}

func mapBackendReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "max":
		return "high"
	default:
		return ""
	}
}

func inferReasoningEffortFromThinking(thinking AnthropicThinking) string {
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "adaptive":
		return "medium"
	case "enabled":
		if thinking.BudgetTokens >= 8000 {
			return "high"
		}
		return "medium"
	default:
		if thinking.BudgetTokens >= 8000 {
			return "high"
		}
		if thinking.BudgetTokens > 0 {
			return "medium"
		}
		return ""
	}
}

func (p *Proxy) optionsForRequest(req AnthropicMessagesRequest, headers http.Header) backendRequestOptions {
	features := scanRequestFeatures(req)
	if p.shouldPrimeModelProfiles(req, headers, features) {
		p.fetchBackendModels()
	}
	caps := p.snapshotCaps(p.cfg.AnonymousMode)
	requestScopeKey := requestScopeKey(p.cfg, req)
	compactType := detectCompactType(req)
	model := p.selectBackendModel(req, headers, caps, compactType, requestScopeKey, features)
	usesRequestModelPassthrough := strings.TrimSpace(p.cfg.BackendModel) == "" && strings.TrimSpace(model) == strings.TrimSpace(req.Model)
	if model != strings.TrimSpace(req.Model) {
		usesRequestModelPassthrough = false
	}
	backendScopeKey := backendCapabilityScopeKey(model)
	hasRoundTripHistoryItems := features.hasRoundTripHistory
	scoped := p.snapshotScopedCaps(backendScopeKey, p.cfg.AnonymousMode, !hasRoundTripHistoryItems)
	stripRoundTripItemIDs := hasRoundTripHistoryItems && scoped.InputItemPersistence == capabilityUnsupported
	profile := backendModelProfile{}
	preset := backendCapabilityPreset{}
	if p.cfg.EnableModelCapabilityInit {
		profile = caps.ModelProfiles[strings.TrimSpace(model)]
		preset = profileCapabilityPreset(profile)
	}

	reasoning := applyReasoningProfile(p.resolveBackendReasoning(req), profile, p.cfg.EnableModelCapabilityInit)
	enableReasoning := reasoning != nil && scoped.Reasoning != capabilityUnsupported
	enableReasoningInclude := requiresReasoningInclude(req) && scoped.ReasoningInclude != capabilityUnsupported && scoped.Reasoning != capabilityUnsupported
	enablePhaseCommentary := p.cfg.EnablePhaseCommentary && scoped.Phase != capabilityUnsupported
	enableStructuredOutput := scoped.StructuredToolOutput != capabilityUnsupported
	enableBackendStreaming := req.Stream && !p.cfg.DisableStreamingBackend && scoped.BackendStreaming != capabilityUnsupported
	preserveCompactionInput := scoped.CompactionInput != capabilityUnsupported
	if shouldPresetDisableFeature(p, backendScopeKey, "compaction_input", scoped.CompactionInput, preset.DisableCompactionInput) {
		preserveCompactionInput = false
	}
	enableContextManagement := p.cfg.EnableModelCapabilityInit && scoped.ContextManagement != capabilityUnsupported && preserveCompactionInput && features.hasCompactionCarrier
	if shouldPresetDisableFeature(p, backendScopeKey, "context_management", scoped.ContextManagement, preset.DisableContextManagement) {
		enableContextManagement = false
	}
	preserveReasoningItems := scoped.Reasoning != capabilityUnsupported
	enablePromptCacheKey := !p.cfg.AnonymousMode && !p.cfg.DisablePromptCacheKey && derivedPromptCacheKey(req, headers) != "" && scoped.PromptCacheKey != capabilityUnsupported
	if shouldPresetDisableFeature(p, backendScopeKey, "prompt_cache_key", scoped.PromptCacheKey, preset.DisablePromptCacheKey) {
		enablePromptCacheKey = false
	}
	if profile.SupportsAdaptiveThinking != nil && !*profile.SupportsAdaptiveThinking {
		enableReasoning = false
		enableReasoningInclude = false
		preserveReasoningItems = false
	}
	if profile.SupportsPhase != nil && !*profile.SupportsPhase {
		enablePhaseCommentary = false
	}
	if shouldPresetDisableFeature(p, backendScopeKey, "phase", scoped.Phase, preset.DisablePhase) {
		enablePhaseCommentary = false
	}
	if profile.SupportsStructuredOutput != nil && !*profile.SupportsStructuredOutput {
		enableStructuredOutput = false
	}
	if profile.SupportsStreaming != nil && !*profile.SupportsStreaming {
		enableBackendStreaming = false
	}
	if shouldPresetDisableFeature(p, backendScopeKey, "reasoning_include", scoped.ReasoningInclude, preset.DisableReasoningInclude) {
		enableReasoningInclude = false
	}
	enableParallelToolCalls := p.cfg.EnableModelCapabilityInit &&
		len(req.Tools) > 0 &&
		profile.SupportsParallelToolCalls != nil &&
		*profile.SupportsParallelToolCalls
	maxOutputTokens := anthropicReqMaxOutputTokens(req, profile, p.cfg.EnableModelCapabilityInit)
	contextManagementCompactThreshold := 0
	if enableContextManagement {
		contextManagementCompactThreshold = contextManagementCompactionThreshold(profile)
	}
	return backendRequestOptions{
		RequestScopeKey:                   requestScopeKey,
		BackendCapabilityScopeKey:         backendScopeKey,
		Model:                             model,
		MaxOutputTokens:                   maxOutputTokens,
		Reasoning:                         reasoning,
		ContextManagementCompactThreshold: contextManagementCompactThreshold,
		EnableMetadata:                    p.cfg.EnableBackendMetadata && !p.cfg.AnonymousMode && caps.Metadata != capabilityUnsupported,
		EnableReasoning:                   enableReasoning,
		EnableReasoningInclude:            enableReasoningInclude,
		EnablePhaseCommentary:             enablePhaseCommentary,
		EnablePromptCacheKey:              enablePromptCacheKey,
		EnableContextManagement:           enableContextManagement,
		PreserveCompactionInput:           preserveCompactionInput,
		PreserveStructuredOutput:          enableStructuredOutput,
		PreserveReasoningItems:            preserveReasoningItems,
		EnableBackendStreaming:            enableBackendStreaming,
		EnableParallelToolCalls:           enableParallelToolCalls,
		StripRoundTripItemIDs:             stripRoundTripItemIDs,
		UsesRequestModelPassthrough:       usesRequestModelPassthrough,
	}
}

func applyReasoningProfile(reasoning *OpenAIReasoning, profile backendModelProfile, enableModelCapabilityInit bool) *OpenAIReasoning {
	if reasoning == nil || !enableModelCapabilityInit || profile.MaxThinkingBudget <= 0 {
		return reasoning
	}
	adjusted := *reasoning
	switch adjusted.Effort {
	case "high":
		if profile.MaxThinkingBudget < 2000 {
			adjusted.Effort = "low"
		} else if profile.MaxThinkingBudget < 8000 {
			adjusted.Effort = "medium"
		}
	case "medium":
		if profile.MaxThinkingBudget < 2000 {
			adjusted.Effort = "low"
		}
	}
	return &adjusted
}

func (p *Proxy) selectBackendModel(req AnthropicMessagesRequest, headers http.Header, caps runtimeCapabilities, compactType int, requestScopeKey string, features requestFeatures) string {
	if warmupModel := p.resolveWarmupModel(req, headers, caps, compactType, features); warmupModel != "" {
		return warmupModel
	}
	model := strings.TrimSpace(p.cfg.EffectiveBackendModel(req.Model))
	if model == "" && strings.TrimSpace(caps.PreferredModel) != "" {
		model = strings.TrimSpace(caps.PreferredModel)
	}
	if strings.TrimSpace(p.cfg.BackendModel) == "" && p.snapshotModelPassthroughState(requestScopeKey) == capabilityUnsupported && strings.TrimSpace(caps.PreferredModel) != "" {
		return caps.PreferredModel
	}
	if !p.cfg.EnableModelCapabilityInit {
		return model
	}
	if profile, ok := caps.ModelProfiles[model]; ok && !modelProfileSupportsRequest(profile, req) {
		fallbackModel := strings.TrimSpace(caps.PreferredModel)
		if fallbackModel != "" && fallbackModel != model {
			if fallbackProfile, ok := caps.ModelProfiles[fallbackModel]; !ok || modelProfileSupportsRequest(fallbackProfile, req) {
				return fallbackModel
			}
		}
	}
	return model
}

func requestScopeKey(cfg Config, req AnthropicMessagesRequest) string {
	if backendModel := strings.TrimSpace(cfg.BackendModel); backendModel != "" {
		return "backend:" + backendModel
	}
	if requestModel := strings.TrimSpace(req.Model); requestModel != "" {
		return "request:" + requestModel
	}
	return ""
}

func backendCapabilityScopeKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return "backend-model:" + model
}

func (p *Proxy) snapshotModelPassthroughState(scopeKey string) capabilityState {
	if strings.TrimSpace(scopeKey) == "" {
		return capabilityUnknown
	}
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	if scope, ok := p.scopedCaps[scopeKey]; ok {
		return p.effectiveCapabilityStateLocked(scopeKey, "model", scope.ModelPassthrough)
	}
	return capabilityUnknown
}

func (p *Proxy) resolveWarmupModel(req AnthropicMessagesRequest, headers http.Header, caps runtimeCapabilities, compactType int, features requestFeatures) string {
	if !isWarmupRequest(req, headers, compactType, features) {
		return ""
	}
	if strings.TrimSpace(p.cfg.BackendModel) != "" {
		return ""
	}
	if warmupModel := strings.TrimSpace(p.cfg.BackendWarmupModel); warmupModel != "" {
		if p.cfg.EnableModelCapabilityInit {
			if profile, ok := caps.ModelProfiles[warmupModel]; ok && !modelProfileSupportsRequest(profile, req) {
				return ""
			}
		}
		return warmupModel
	}
	if !p.cfg.EnableModelCapabilityInit {
		return ""
	}
	warmupModel := strings.TrimSpace(caps.WarmupModel)
	if warmupModel == "" {
		return ""
	}
	if profile, ok := caps.ModelProfiles[warmupModel]; ok && !modelProfileSupportsRequest(profile, req) {
		return ""
	}
	return warmupModel
}

func isWarmupRequest(req AnthropicMessagesRequest, headers http.Header, compactType int, features requestFeatures) bool {
	if compactType != compactNone {
		return false
	}
	if headers == nil || strings.TrimSpace(headers.Get("anthropic-beta")) == "" {
		return false
	}
	if len(req.Tools) > 0 || req.ToolChoice != nil {
		return false
	}
	if len(req.Messages) != 1 {
		return false
	}
	if features.hasSystemInstructions || features.hasVisionInput || features.hasToolResultHistory || features.hasCompactionCarrier {
		return false
	}
	return features.warmupCandidateText != ""
}

func (p *Proxy) shouldPrimeModelProfiles(req AnthropicMessagesRequest, headers http.Header, features requestFeatures) bool {
	if !p.cfg.EnableModelCapabilityInit {
		return false
	}
	caps := p.snapshotCaps(p.cfg.AnonymousMode)
	if caps.ModelsFetched {
		return false
	}
	compactType := detectCompactType(req)
	return req.Stream ||
		len(req.Tools) > 0 ||
		p.resolveBackendReasoning(req) != nil ||
		isWarmupRequest(req, headers, compactType, features) ||
		compactType != compactNone ||
		features.hasCompactionCarrier
}

func modelProfileSupportsRequest(profile backendModelProfile, req AnthropicMessagesRequest) bool {
	if !profileSupportsResponses(profile) {
		return false
	}
	if !profileSupportsThinkingBudget(profile, req) {
		return false
	}
	if promptLimit := profilePromptLimit(profile); promptLimit > 0 {
		if estimateInputTokens(req.System, req.Messages, req.Tools) > promptLimit {
			return false
		}
	}
	if req.Stream && profile.SupportsStreaming != nil && !*profile.SupportsStreaming {
		return false
	}
	if len(req.Tools) > 0 && profile.SupportsToolCalls != nil && !*profile.SupportsToolCalls {
		return false
	}
	return true
}

func profileSupportsThinkingBudget(profile backendModelProfile, req AnthropicMessagesRequest) bool {
	if req.Thinking == nil || req.Thinking.BudgetTokens <= 0 {
		return true
	}
	if profile.MinThinkingBudget > 0 && req.Thinking.BudgetTokens < profile.MinThinkingBudget {
		return false
	}
	if profile.MaxThinkingBudget > 0 && req.Thinking.BudgetTokens > profile.MaxThinkingBudget {
		return false
	}
	return true
}

func profilePromptLimit(profile backendModelProfile) int {
	switch {
	case profile.MaxPromptTokens > 0 && profile.MaxContextWindowTokens > 0 && profile.MaxPromptTokens < profile.MaxContextWindowTokens:
		return profile.MaxPromptTokens
	case profile.MaxPromptTokens > 0:
		return profile.MaxPromptTokens
	default:
		return profile.MaxContextWindowTokens
	}
}

func contextManagementCompactionThreshold(profile backendModelProfile) int {
	if limit := profilePromptLimit(profile); limit > 0 {
		return int(math.Floor(float64(limit) * 0.9))
	}
	return 50000
}

func compactInputByLatestCompaction(items []OpenAIInputItem) []OpenAIInputItem {
	latestIndex := -1
	for i := len(items) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(items[i].Type), "compaction") {
			latestIndex = i
			break
		}
	}
	if latestIndex <= 0 {
		return items
	}
	return append([]OpenAIInputItem(nil), items[latestIndex:]...)
}

func downgradeCompactionInputItems(items []OpenAIInputItem) []OpenAIInputItem {
	if len(items) == 0 {
		return items
	}
	converted := make([]OpenAIInputItem, len(items))
	copy(converted, items)
	for i, item := range converted {
		if !strings.EqualFold(strings.TrimSpace(item.Type), "compaction") {
			continue
		}
		converted[i] = OpenAIInputItem{
			ID:               item.ID,
			Type:             "reasoning",
			EncryptedContent: item.EncryptedContent,
			Summary:          []OpenAIReasoningPart{},
			Status:           item.Status,
		}
	}
	return converted
}

func dropCompactionInputItems(items []OpenAIInputItem) []OpenAIInputItem {
	if len(items) == 0 {
		return items
	}
	filtered := make([]OpenAIInputItem, 0, len(items))
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Type), "compaction") {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func scanRequestFeatures(req AnthropicMessagesRequest) requestFeatures {
	features := requestFeatures{}
	if systemBlocks, err := normalizeSystemBlocks(req.System); err == nil {
		features.hasSystemInstructions = strings.TrimSpace(systemBlocksToInstructions(systemBlocks)) != ""
	}
	if len(req.Messages) == 1 {
		features.warmupCandidateText = warmupCandidateText(req.Messages[0])
	}
	for _, message := range req.Messages {
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			switch block.Type {
			case "image", "document", "file":
				features.hasVisionInput = true
			case "tool_result":
				features.hasToolResultHistory = true
				if toolResultContainsMedia(block.Content) {
					features.hasVisionInput = true
				}
			}
			carrier := anthropicThinkingCarrier(block)
			if carrier == "" {
				continue
			}
			if _, ok := decodeCompactionCarrier(carrier); ok {
				features.hasCompactionCarrier = true
				features.hasRoundTripHistory = true
			}
			if _, ok := decodeReasoningCarrier(carrier); ok {
				features.hasRoundTripHistory = true
			}
		}
	}
	return features
}

func requestHasVisionInput(req AnthropicMessagesRequest) bool {
	return scanRequestFeatures(req).hasVisionInput
}

func requestHasToolResultHistory(req AnthropicMessagesRequest) bool {
	return scanRequestFeatures(req).hasToolResultHistory
}

func requestContainsCompactionCarrier(req AnthropicMessagesRequest) bool {
	return scanRequestFeatures(req).hasCompactionCarrier
}

func requestHasRoundTripHistoryItems(req AnthropicMessagesRequest) bool {
	return scanRequestFeatures(req).hasRoundTripHistory
}

func anthropicReqMaxOutputTokens(req AnthropicMessagesRequest, profile backendModelProfile, enableModelCapabilityInit bool) int {
	maxOutputTokens := req.MaxTokens
	if !enableModelCapabilityInit || maxOutputTokens <= 0 {
		return maxOutputTokens
	}
	if profile.MaxOutputTokens > 0 && maxOutputTokens > profile.MaxOutputTokens {
		return profile.MaxOutputTokens
	}
	return maxOutputTokens
}

func toolResultContainsMedia(raw any) bool {
	blocks, err := normalizeContentBlocks(raw)
	if err != nil {
		return false
	}
	for _, block := range blocks {
		switch block.Type {
		case "image", "document", "file":
			return true
		}
	}
	return false
}

func extractLastUserText(messages []AnthropicMessage) string {
	for idx := len(messages) - 1; idx >= 0; idx-- {
		message := messages[idx]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			if text, ok := message.Content.(string); ok {
				return text
			}
			continue
		}
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "" || block.Type == "text" {
				if text := strings.TrimSpace(stripSubagentMarkerFromText(block.Text)); text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func (p *Proxy) snapshotCaps(skipMetadataReprobe bool) runtimeCapabilities {
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	copyCaps := p.caps
	if !skipMetadataReprobe {
		copyCaps.Metadata = p.effectiveCapabilityStateLocked("global", "metadata", copyCaps.Metadata)
	}
	if p.caps.SupportedModels != nil {
		copyCaps.SupportedModels = make(map[string]struct{}, len(p.caps.SupportedModels))
		for k, v := range p.caps.SupportedModels {
			copyCaps.SupportedModels[k] = v
		}
	}
	if p.caps.ModelProfiles != nil {
		copyCaps.ModelProfiles = make(map[string]backendModelProfile, len(p.caps.ModelProfiles))
		for k, v := range p.caps.ModelProfiles {
			copyCaps.ModelProfiles[k] = cloneBackendModelProfile(v)
		}
	}
	return copyCaps
}

func cloneBackendModelProfile(profile backendModelProfile) backendModelProfile {
	cloned := profile
	if profile.SupportedEndpoints != nil {
		cloned.SupportedEndpoints = make(map[string]struct{}, len(profile.SupportedEndpoints))
		for k, v := range profile.SupportedEndpoints {
			cloned.SupportedEndpoints[k] = v
		}
	}
	return cloned
}

func capabilityCooldownKey(scopeKey, feature string) string {
	scopeKey = strings.TrimSpace(scopeKey)
	feature = strings.TrimSpace(feature)
	if scopeKey == "" {
		return feature
	}
	return scopeKey + "|" + feature
}

func capabilityReprobeLeaseKey(scopeKey, feature string) string {
	return capabilityCooldownKey(scopeKey, feature)
}

func (p *Proxy) effectiveCapabilityStateLocked(scopeKey, feature string, state capabilityState) capabilityState {
	if state != capabilityUnsupported || p.cfg.CapabilityReprobeTTL <= 0 {
		return state
	}
	until, ok := p.unsupportedUntil[capabilityCooldownKey(scopeKey, feature)]
	if !ok {
		return state
	}
	if p.now().Before(until) {
		return state
	}
	leaseKey := capabilityReprobeLeaseKey(scopeKey, feature)
	if leaseUntil, ok := p.reprobeUntil[leaseKey]; ok && p.now().Before(leaseUntil) {
		return state
	}
	leaseTTL := capabilityReprobeLeaseTTL
	if p.cfg.RequestTimeout > 0 && p.cfg.RequestTimeout < leaseTTL {
		leaseTTL = p.cfg.RequestTimeout
	}
	leaseUntil := p.now().Add(leaseTTL)
	p.reprobeUntil[leaseKey] = leaseUntil
	p.debugf("capability feature=%s scope=%q cooldown expired; reprobe lease until=%s", feature, scopeKey, leaseUntil.Format(time.RFC3339))
	return capabilityUnknown
}

func (p *Proxy) setCapabilityCooldownLocked(scopeKey, feature string) {
	if p.cfg.CapabilityReprobeTTL <= 0 {
		return
	}
	until := p.now().Add(p.cfg.CapabilityReprobeTTL)
	p.unsupportedUntil[capabilityCooldownKey(scopeKey, feature)] = until
	p.debugf("capability feature=%s scope=%q marked unsupported until=%s", feature, scopeKey, until.Format(time.RFC3339))
}

func (p *Proxy) clearCapabilityCooldownLocked(scopeKey, feature string) {
	cooldownKey := capabilityCooldownKey(scopeKey, feature)
	leaseKey := capabilityReprobeLeaseKey(scopeKey, feature)
	_, hadCooldown := p.unsupportedUntil[cooldownKey]
	_, hadLease := p.reprobeUntil[leaseKey]
	delete(p.unsupportedUntil, cooldownKey)
	delete(p.reprobeUntil, leaseKey)
	if hadCooldown || hadLease {
		p.debugf("capability feature=%s scope=%q restored", feature, scopeKey)
	}
}

func (p *Proxy) snapshotScopedCaps(scopeKey string, skipPromptCacheKeyReprobe, skipInputItemPersistenceReprobe bool) scopedRuntimeCapabilities {
	if strings.TrimSpace(scopeKey) == "" {
		return scopedRuntimeCapabilities{}
	}
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	scoped := p.scopedCaps[scopeKey]
	scoped.StructuredToolOutput = p.effectiveCapabilityStateLocked(scopeKey, "structured_output", scoped.StructuredToolOutput)
	scoped.BackendStreaming = p.effectiveCapabilityStateLocked(scopeKey, "stream", scoped.BackendStreaming)
	scoped.ContextManagement = p.effectiveCapabilityStateLocked(scopeKey, "context_management", scoped.ContextManagement)
	scoped.CompactionInput = p.effectiveCapabilityStateLocked(scopeKey, "compaction_input", scoped.CompactionInput)
	scoped.Reasoning = p.effectiveCapabilityStateLocked(scopeKey, "reasoning", scoped.Reasoning)
	scoped.ReasoningInclude = p.effectiveCapabilityStateLocked(scopeKey, "reasoning_include", scoped.ReasoningInclude)
	scoped.Phase = p.effectiveCapabilityStateLocked(scopeKey, "phase", scoped.Phase)
	scoped.ModelPassthrough = p.effectiveCapabilityStateLocked(scopeKey, "model", scoped.ModelPassthrough)
	if !skipPromptCacheKeyReprobe {
		scoped.PromptCacheKey = p.effectiveCapabilityStateLocked(scopeKey, "prompt_cache_key", scoped.PromptCacheKey)
	}
	if !skipInputItemPersistenceReprobe {
		scoped.InputItemPersistence = p.effectiveCapabilityStateLocked(scopeKey, "input_item_persistence", scoped.InputItemPersistence)
	}
	return scoped
}

func (p *Proxy) setCapabilityUnsupported(feature string, opts backendRequestOptions) {
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	switch feature {
	case "metadata":
		p.caps.Metadata = capabilityUnsupported
		p.setCapabilityCooldownLocked("global", "metadata")
	case "structured_output", "stream", "reasoning", "reasoning_include", "phase", "prompt_cache_key", "context_management", "compaction_input", "input_item_persistence":
		if strings.TrimSpace(opts.BackendCapabilityScopeKey) == "" {
			return
		}
		scoped := p.scopedCaps[opts.BackendCapabilityScopeKey]
		switch feature {
		case "structured_output":
			scoped.StructuredToolOutput = capabilityUnsupported
		case "stream":
			scoped.BackendStreaming = capabilityUnsupported
		case "reasoning":
			scoped.Reasoning = capabilityUnsupported
		case "reasoning_include":
			scoped.ReasoningInclude = capabilityUnsupported
		case "phase":
			scoped.Phase = capabilityUnsupported
		case "prompt_cache_key":
			scoped.PromptCacheKey = capabilityUnsupported
		case "context_management":
			scoped.ContextManagement = capabilityUnsupported
		case "compaction_input":
			scoped.CompactionInput = capabilityUnsupported
		case "input_item_persistence":
			scoped.InputItemPersistence = capabilityUnsupported
		}
		p.scopedCaps[opts.BackendCapabilityScopeKey] = scoped
		p.setCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, feature)
	case "model":
		if strings.TrimSpace(opts.RequestScopeKey) == "" {
			return
		}
		scoped := p.scopedCaps[opts.RequestScopeKey]
		scoped.ModelPassthrough = capabilityUnsupported
		p.scopedCaps[opts.RequestScopeKey] = scoped
		p.setCapabilityCooldownLocked(opts.RequestScopeKey, "model")
	}
}

func (p *Proxy) learnCapabilitiesFromRequest(opts backendRequestOptions, payload OpenAIResponsesRequest) {
	p.capsMu.Lock()
	defer p.capsMu.Unlock()
	if payload.Metadata != nil {
		p.caps.Metadata = capabilitySupported
		p.clearCapabilityCooldownLocked("global", "metadata")
	}
	if strings.TrimSpace(opts.BackendCapabilityScopeKey) != "" {
		scoped := p.scopedCaps[opts.BackendCapabilityScopeKey]
		if requestUsesStructuredToolOutput(payload) {
			scoped.StructuredToolOutput = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "structured_output")
		}
		if payload.Stream {
			scoped.BackendStreaming = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "stream")
		}
		if payload.Reasoning != nil {
			scoped.Reasoning = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "reasoning")
		}
		if len(payload.Include) > 0 {
			scoped.ReasoningInclude = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "reasoning_include")
		}
		if requestUsesPhaseCommentary(payload) {
			scoped.Phase = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "phase")
		}
		if strings.TrimSpace(payload.PromptCacheKey) != "" {
			scoped.PromptCacheKey = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "prompt_cache_key")
		}
		if len(payload.ContextManagement) > 0 {
			scoped.ContextManagement = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "context_management")
		}
		if requestUsesCompactionInput(payload) {
			scoped.CompactionInput = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "compaction_input")
		}
		if requestUsesRoundTripHistoryItems(payload) {
			scoped.InputItemPersistence = capabilitySupported
			p.clearCapabilityCooldownLocked(opts.BackendCapabilityScopeKey, "input_item_persistence")
		}
		p.scopedCaps[opts.BackendCapabilityScopeKey] = scoped
	}
	if strings.TrimSpace(opts.RequestScopeKey) == "" {
		return
	}

	scoped := p.scopedCaps[opts.RequestScopeKey]
	if opts.UsesRequestModelPassthrough {
		scoped.ModelPassthrough = capabilitySupported
		p.clearCapabilityCooldownLocked(opts.RequestScopeKey, "model")
	}
	p.scopedCaps[opts.RequestScopeKey] = scoped
}

func requiresReasoningInclude(req AnthropicMessagesRequest) bool {
	for _, message := range req.Messages {
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "thinking" && strings.TrimSpace(block.Signature) != "" {
				return true
			}
			if block.Type == "redacted_thinking" && strings.TrimSpace(block.Data) != "" {
				return true
			}
		}
	}
	return false
}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req AnthropicMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	p.debugf("incoming request stream=%t model=%q messages=%d has_system=%t has_metadata=%t tools=%d", req.Stream, req.Model, len(req.Messages), req.System != nil, len(req.Metadata) > 0, len(req.Tools))
	if marker, ok := parseSubagentMarker(req); ok {
		p.debugf("detected subagent marker session_id=%q agent_id=%q agent_type=%q", marker.SessionID, marker.AgentID, marker.AgentType)
	}

	if req.Stream {
		p.handleStream(w, r.Context(), req, r.Header)
		return
	}

	p.handleNonStream(w, r.Context(), req, r.Header)
}

func (p *Proxy) handleNonStream(w http.ResponseWriter, ctx context.Context, anthropicReq AnthropicMessagesRequest, headers http.Header) {
	resp, payload, err := p.doBackendWithAdaptiveRetry(ctx, anthropicReq, headers)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		p.forwardBackendError(w, resp)
		return
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		backendResp, err := aggregateBackendStream(resp.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid backend stream")
			return
		}
		anthropicResp, err := translateBackendResponse(backendResp, p.cfg.AdvertisedModel(payload.Model))
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, anthropicResp)
		return
	}

	var backendResp OpenAIResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&backendResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid backend response")
		return
	}

	anthropicResp, err := translateBackendResponse(backendResp, p.cfg.AdvertisedModel(payload.Model))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, anthropicResp)
}

func (p *Proxy) handleStream(w http.ResponseWriter, ctx context.Context, anthropicReq AnthropicMessagesRequest, headers http.Header) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	resp, payload, err := p.doBackendWithAdaptiveRetry(ctx, anthropicReq, headers)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		p.forwardBackendError(w, resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	advertisedModel := p.cfg.AdvertisedModel(payload.Model)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		translator := newSSETranslator(w, flusher, advertisedModel, p.nextID("msg"), p.debugf)
		if err := translator.consume(resp.Body); err != nil {
			translator.writeAnthropicStreamError(err.Error())
		}
		return
	}

	var backendResp OpenAIResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&backendResp); err != nil {
		newSSETranslator(w, flusher, advertisedModel, p.nextID("msg"), p.debugf).writeAnthropicStreamError("invalid backend response")
		return
	}

	anthropicResp, err := translateBackendResponse(backendResp, advertisedModel)
	if err != nil {
		newSSETranslator(w, flusher, advertisedModel, fallback(backendResp.ID, p.nextID("msg")), p.debugf).writeAnthropicStreamError(err.Error())
		return
	}

	newSSETranslator(w, flusher, advertisedModel, fallback(anthropicResp.ID, p.nextID("msg")), p.debugf).emitResponse(anthropicResp)
}

func (p *Proxy) buildBackendRequest(ctx context.Context, anthropicReq AnthropicMessagesRequest, headers http.Header) (*http.Request, error) {
	opts := p.optionsForRequest(anthropicReq, headers)
	_, req, err := p.buildBackendRequestWithOptions(ctx, anthropicReq, headers, opts)
	return req, err
}

func (p *Proxy) buildBackendRequestWithOptions(ctx context.Context, anthropicReq AnthropicMessagesRequest, headers http.Header, opts backendRequestOptions) (OpenAIResponsesRequest, *http.Request, error) {
	continuity := deriveContinuityContext(anthropicReq, headers)
	compactType := detectCompactType(anthropicReq)
	preprocessMessagesForClaude(&anthropicReq, preprocessOptions{compactType: compactType})

	systemBlocks, err := normalizeSystemBlocks(anthropicReq.System)
	if err != nil {
		return OpenAIResponsesRequest{}, nil, err
	}

	input, err := convertAnthropicInput(anthropicReq, opts)
	if err != nil {
		return OpenAIResponsesRequest{}, nil, err
	}
	input = compactInputByLatestCompaction(input)
	if !opts.PreserveCompactionInput {
		if opts.PreserveReasoningItems {
			input = downgradeCompactionInputItems(input)
		} else {
			input = dropCompactionInputItems(input)
		}
	}
	input = stripReasoningItemIDsWithoutEncryptedContent(input)
	if opts.StripRoundTripItemIDs {
		input = stripRoundTripHistoryItemIDs(input)
	}

	backendReq := OpenAIResponsesRequest{
		Model:           opts.Model,
		Instructions:    systemBlocksToInstructions(systemBlocks),
		Input:           input,
		Tools:           convertTools(anthropicReq.Tools),
		ToolChoice:      convertToolChoice(anthropicReq.ToolChoice),
		MaxOutputTokens: opts.MaxOutputTokens,
		Temperature:     anthropicReq.Temperature,
		TopP:            anthropicReq.TopP,
		Store:           true,
		Stream:          opts.EnableBackendStreaming,
	}
	if opts.EnableContextManagement {
		backendReq.ContextManagement = []OpenAIContextManagementItem{{
			Type:             "compaction",
			CompactThreshold: opts.ContextManagementCompactThreshold,
		}}
	}
	if opts.EnableParallelToolCalls {
		enabled := true
		backendReq.ParallelToolCalls = &enabled
	}
	if opts.EnableReasoningInclude {
		backendReq.Include = []string{"reasoning.encrypted_content"}
	}
	if opts.EnableMetadata {
		backendReq.Metadata = buildMetadata(
			headers,
			anthropicReq.Metadata,
			continuity,
			p.cfg.EffectiveForwardUserMetadata(),
			p.cfg.UserMetadataAllowlist,
			!p.cfg.AnonymousMode && !p.cfg.DisableContinuityMetadata,
			!p.cfg.AnonymousMode,
		)
	}
	if opts.EnablePromptCacheKey {
		backendReq.PromptCacheKey = continuity.PromptCacheKey
	}

	if opts.EnableReasoning {
		backendReq.Reasoning = opts.Reasoning
	}
	p.debugf("backend request model=%q stream=%t instructions=%t metadata_keys=%d input_items=%d prompt_cache_key=%t", backendReq.Model, backendReq.Stream, strings.TrimSpace(backendReq.Instructions) != "", len(backendReq.Metadata), len(backendReq.Input), strings.TrimSpace(backendReq.PromptCacheKey) != "")
	p.debugf("backend input summary: %s", summarizeInputItems(backendReq.Input))
	p.debugf("backend tool summary: %s", summarizeTools(backendReq.Tools))
	p.debugf("backend tool outputs: %s", summarizeFunctionCallOutputs(backendReq.Input))

	body, err := json.Marshal(backendReq)
	if err != nil {
		return OpenAIResponsesRequest{}, nil, fmt.Errorf("marshal backend request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BackendURL(), bytes.NewReader(body))
	if err != nil {
		return OpenAIResponsesRequest{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.BackendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if backendReq.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return backendReq, req, nil
}

func (p *Proxy) doBackendWithAdaptiveRetry(ctx context.Context, anthropicReq AnthropicMessagesRequest, headers http.Header) (*http.Response, OpenAIResponsesRequest, error) {
	opts := p.optionsForRequest(anthropicReq, headers)
	tried := map[string]bool{}

	for attempts := 0; attempts < 8; attempts++ {
		payload, req, err := p.buildBackendRequestWithOptions(ctx, anthropicReq, headers, opts)
		if err != nil {
			return nil, OpenAIResponsesRequest{}, err
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, payload, err
		}
		if resp.StatusCode < 400 {
			p.learnCapabilitiesFromRequest(opts, payload)
			return resp, payload, nil
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, payload, readErr
		}
		feature := p.classifyCapabilityFailure(resp.StatusCode, body, opts, payload)
		if feature == "" || tried[feature] {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, nil
		}

		p.debugf("adaptive retry disabling feature=%s status=%d body=%s", feature, resp.StatusCode, sanitizeLogValue(string(body)))
		tried[feature] = true
		if feature == "input_item_persistence" {
			p.setCapabilityUnsupported(feature, opts)
			nextOpts := p.optionsForRequest(anthropicReq, headers)
			nextOpts.StripRoundTripItemIDs = true
			opts = nextOpts
			continue
		}
		if feature == "model" && strings.TrimSpace(p.cfg.BackendModel) == "" {
			p.fetchBackendModels()
		}
		p.setCapabilityUnsupported(feature, opts)
		nextOpts := p.optionsForRequest(anthropicReq, headers)
		nextOpts.StripRoundTripItemIDs = opts.StripRoundTripItemIDs
		opts = nextOpts
	}
	return nil, OpenAIResponsesRequest{}, fmt.Errorf("adaptive retry exhausted")
}

func (p *Proxy) classifyCapabilityFailure(status int, body []byte, opts backendRequestOptions, payload OpenAIResponsesRequest) string {
	if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusUnprocessableEntity && status != http.StatusNotImplemented {
		return ""
	}
	hint := extractBackendErrorHint(body)
	msg := hint.Message
	switch {
	case payload.Metadata != nil && matchesParameterFailure(msg, "metadata", hint.Param):
		return "metadata"
	case payload.Reasoning != nil && matchesParameterFailure(msg, "reasoning", hint.Param):
		return "reasoning"
	case len(payload.Include) > 0 && strings.Contains(msg, "reasoning.encrypted_content"):
		return "reasoning_include"
	case requestUsesPhaseCommentary(payload) && (matchesParameterFailure(msg, "phase", hint.Param) || strings.Contains(msg, ".phase")):
		return "phase"
	case payload.Stream && matchesParameterFailure(msg, "stream", hint.Param):
		return "stream"
	case requestUsesStructuredToolOutput(payload) && (strings.Contains(msg, "function_call_output.output") || strings.Contains(msg, "\"param\":\"input") && strings.Contains(msg, ".output")):
		return "structured_output"
	case strings.TrimSpace(payload.PromptCacheKey) != "" && matchesParameterFailure(msg, "prompt_cache_key", hint.Param):
		return "prompt_cache_key"
	case len(payload.ContextManagement) > 0 && matchesParameterFailure(msg, "context_management", hint.Param):
		return "context_management"
	case requestUsesCompactionInput(payload) && matchesCompactionInputFailure(msg, hint.Param, payload):
		return "compaction_input"
	case requestUsesRoundTripHistoryItems(payload) && matchesInputItemPersistenceFailure(msg):
		return "input_item_persistence"
	case opts.UsesRequestModelPassthrough && matchesModelFailure(msg, hint.Param, hint.Code):
		return "model"
	default:
		return ""
	}
}

type backendErrorHint struct {
	Message string
	Param   string
	Code    string
}

func extractBackendErrorHint(body []byte) backendErrorHint {
	hint := backendErrorHint{
		Message: strings.ToLower(string(body)),
	}

	var resp OpenAIResponsesResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
		return hint
	}

	if trimmed := strings.ToLower(strings.TrimSpace(resp.Error.Message)); trimmed != "" {
		hint.Message = trimmed
	}
	hint.Param = strings.ToLower(strings.TrimSpace(resp.Error.Param))
	hint.Code = strings.ToLower(strings.TrimSpace(stringifyAny(resp.Error.Code)))
	return hint
}

func requestUsesPhaseCommentary(payload OpenAIResponsesRequest) bool {
	for _, item := range payload.Input {
		if strings.TrimSpace(item.Phase) != "" {
			return true
		}
	}
	return false
}

func requestUsesCompactionInput(payload OpenAIResponsesRequest) bool {
	for _, item := range payload.Input {
		if strings.EqualFold(strings.TrimSpace(item.Type), "compaction") {
			return true
		}
	}
	return false
}

func requestUsesRoundTripHistoryItems(payload OpenAIResponsesRequest) bool {
	for _, item := range payload.Input {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(item.Type)) {
		case "reasoning", "compaction":
			return true
		}
	}
	return false
}

func requestUsesStructuredToolOutput(payload OpenAIResponsesRequest) bool {
	for _, item := range payload.Input {
		if item.Type != "function_call_output" || item.Output == nil {
			continue
		}
		if _, ok := item.Output.(string); ok {
			continue
		}
		return true
	}
	return false
}

func convertAnthropicInput(req AnthropicMessagesRequest, opts backendRequestOptions) ([]OpenAIInputItem, error) {
	var items []OpenAIInputItem

	for _, message := range req.Messages {
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			return nil, fmt.Errorf("normalize %s content: %w", message.Role, err)
		}
		converted, err := convertMessageBlocks(message.Role, blocks, opts)
		if err != nil {
			return nil, err
		}
		items = append(items, converted...)
	}

	return items, nil
}

func systemBlocksToInstructions(blocks []AnthropicContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}

	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" || strings.TrimSpace(block.Text) == "" {
			continue
		}
		parts = append(parts, block.Text)
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func contentTextTypeForRole(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "assistant") {
		return "output_text"
	}
	return "input_text"
}

func resolveAssistantPhase(blocks []AnthropicContentBlock) string {
	hasText := false
	hasToolUse := false
	for _, block := range blocks {
		switch block.Type {
		case "", "text":
			if strings.TrimSpace(block.Text) != "" {
				hasText = true
			}
		case "tool_use":
			hasToolUse = true
		}
	}
	if !hasText {
		return ""
	}
	if hasToolUse {
		return "commentary"
	}
	return "final_answer"
}

func normalizeSystemBlocks(system any) ([]AnthropicContentBlock, error) {
	switch typed := system.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, nil
		}
		return []AnthropicContentBlock{{Type: "text", Text: typed}}, nil
	default:
		return normalizeContentBlocks(system)
	}
}

func parseSubagentMarker(req AnthropicMessagesRequest) (subagentMarker, bool) {
	for _, message := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			return subagentMarker{}, false
		}
		for _, block := range blocks {
			if block.Type != "text" {
				continue
			}
			if marker, ok := parseSubagentMarkerFromText(block.Text); ok {
				return marker, true
			}
		}
		break
	}
	return subagentMarker{}, false
}

func parseSubagentMarkerFromText(text string) (subagentMarker, bool) {
	const (
		startTag     = "<system-reminder>"
		endTag       = "</system-reminder>"
		markerPrefix = "__SUBAGENT_MARKER__"
	)

	searchFrom := 0
	for {
		reminderStart := strings.Index(text[searchFrom:], startTag)
		if reminderStart == -1 {
			return subagentMarker{}, false
		}
		reminderStart += searchFrom
		contentStart := reminderStart + len(startTag)
		reminderEndRel := strings.Index(text[contentStart:], endTag)
		if reminderEndRel == -1 {
			return subagentMarker{}, false
		}
		reminderEnd := contentStart + reminderEndRel
		reminderContent := text[contentStart:reminderEnd]
		markerIndex := strings.Index(reminderContent, markerPrefix)
		if markerIndex == -1 {
			searchFrom = reminderEnd + len(endTag)
			continue
		}

		markerJSON := strings.TrimSpace(reminderContent[markerIndex+len(markerPrefix):])
		var marker subagentMarker
		if err := json.Unmarshal([]byte(markerJSON), &marker); err != nil {
			return subagentMarker{}, false
		}
		if marker.SessionID == "" || marker.AgentID == "" || marker.AgentType == "" {
			return subagentMarker{}, false
		}
		return marker, true
	}
}

func stripSubagentMarkerFromText(text string) string {
	const (
		startTag     = "<system-reminder>"
		endTag       = "</system-reminder>"
		markerPrefix = "__SUBAGENT_MARKER__"
	)

	var cleaned strings.Builder
	searchFrom := 0
	for {
		reminderStartRel := strings.Index(text[searchFrom:], startTag)
		if reminderStartRel == -1 {
			cleaned.WriteString(text[searchFrom:])
			break
		}
		reminderStart := searchFrom + reminderStartRel
		cleaned.WriteString(text[searchFrom:reminderStart])
		contentStart := reminderStart + len(startTag)
		reminderEndRel := strings.Index(text[contentStart:], endTag)
		if reminderEndRel == -1 {
			cleaned.WriteString(text[reminderStart:])
			break
		}
		reminderEnd := contentStart + reminderEndRel
		reminderContent := text[contentStart:reminderEnd]
		if !strings.Contains(reminderContent, markerPrefix) {
			cleaned.WriteString(text[reminderStart : reminderEnd+len(endTag)])
		}
		searchFrom = reminderEnd + len(endTag)
	}

	return strings.TrimSpace(cleaned.String())
}

func deriveContinuityContext(req AnthropicMessagesRequest, headers http.Header) continuityContext {
	promptCacheKey := derivedPromptCacheKey(req, headers)
	rootSessionID := deriveRootSessionID(req, headers, promptCacheKey)
	var markerPtr *subagentMarker
	if marker, ok := parseSubagentMarker(req); ok {
		markerCopy := marker
		markerPtr = &markerCopy
	}
	requestReq := AnthropicMessagesRequest{
		Model:    req.Model,
		System:   req.System,
		Messages: append([]AnthropicMessage(nil), req.Messages...),
	}
	stripSubagentMarkers(&requestReq)
	requestID := deriveRequestID(requestReq, rootSessionID)
	interactionType := deriveInteractionType(req, markerPtr, detectCompactType(req))
	interactionID := deriveInteractionID(rootSessionID, requestID, interactionType)

	return continuityContext{
		RootSessionID:    rootSessionID,
		RequestID:        requestID,
		PromptCacheKey:   promptCacheKey,
		SessionAffinity:  deriveSessionAffinity(promptCacheKey, rootSessionID, headers),
		ParentSessionID:  deriveParentSessionID(headers, markerPtr),
		InboundRequestID: sanitizedMetadataValue(firstNonEmpty(headerValue(headers, "x-request-id"), headerValue(headers, "x-correlation-id"), headerValue(headers, "x-claude-code-request-id"))),
		TraceID:          deriveTraceID(headers),
		InteractionType:  interactionType,
		InteractionID:    interactionID,
		Subagent:         markerPtr,
	}
}

func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get(name))
}

func deriveSessionAffinity(promptCacheKey, rootSessionID string, headers http.Header) string {
	source := firstNonEmpty(
		headerValue(headers, "x-session-affinity"),
		promptCacheKey,
		rootSessionID,
		headerValue(headers, "x-claude-code-session-id"),
		headerValue(headers, "x-session-id"),
	)
	if source == "" {
		return ""
	}
	return stableUUID(source)
}

func deriveParentSessionID(headers http.Header, marker *subagentMarker) string {
	source := firstNonEmpty(
		headerValue(headers, "x-claude-code-parent-session-id"),
		headerValue(headers, "x-parent-session-id"),
	)
	if source == "" && marker != nil {
		source = strings.TrimSpace(marker.SessionID)
	}
	if source == "" {
		return ""
	}
	return stableUUID(source)
}

func deriveTraceID(headers http.Header) string {
	traceID := strings.TrimSpace(headerValue(headers, "x-trace-id"))
	if isHexTraceID(traceID) {
		return strings.ToLower(traceID)
	}
	traceparent := strings.TrimSpace(headerValue(headers, "traceparent"))
	if traceparent != "" {
		parts := strings.Split(traceparent, "-")
		if len(parts) >= 4 && isHexTraceID(parts[1]) {
			return strings.ToLower(parts[1])
		}
	}
	return ""
}

func matchesParameterFailure(msg, param, observedParam string) bool {
	param = strings.ToLower(strings.TrimSpace(param))
	if param == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(observedParam), param) {
		return true
	}
	if matched, _ := regexp.MatchString(`"param"\s*:\s*"`+regexp.QuoteMeta(param)+`"`, msg); matched {
		return true
	}
	patterns := []string{
		"unsupported parameter: " + param,
		"unknown parameter: " + param,
		"unrecognized request argument supplied: " + param,
		`"` + param + `" was unexpected`,
		`'` + param + `' was unexpected`,
	}
	for _, pattern := range patterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

func matchesCompactionInputFailure(msg, observedParam string, payload OpenAIResponsesRequest) bool {
	messageHintsTypeFailure := strings.Contains(msg, "unsupported") ||
		strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "unknown") ||
		strings.Contains(msg, "unrecognized")
	if strings.Contains(msg, "compaction") && messageHintsTypeFailure &&
		(strings.Contains(msg, "input") || strings.Contains(msg, "item") || strings.Contains(msg, "type")) {
		return true
	}
	if !observedParamTargetsCompactionInput(payload, observedParam) {
		return false
	}
	return messageHintsTypeFailure ||
		strings.Contains(msg, "expected") ||
		strings.Contains(msg, "allowed") ||
		strings.Contains(msg, "valid") ||
		strings.Contains(msg, "enum") ||
		strings.Contains(msg, "one of")
}

func observedParamTargetsCompactionInput(payload OpenAIResponsesRequest, observedParam string) bool {
	observedParam = strings.ToLower(strings.TrimSpace(observedParam))
	if observedParam == "" {
		return false
	}
	matches := regexp.MustCompile(`^input\[(\d+)\](?:\.type)?$`).FindStringSubmatch(observedParam)
	if len(matches) != 2 {
		return false
	}
	idx, err := strconv.Atoi(matches[1])
	if err != nil || idx < 0 || idx >= len(payload.Input) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.Input[idx].Type), "compaction")
}

func matchesInputItemPersistenceFailure(msg string) bool {
	return strings.Contains(msg, "items are not persisted when `store` is set to false") ||
		(strings.Contains(msg, "item with id") && strings.Contains(msg, "not found") && strings.Contains(msg, "input")) ||
		(strings.Contains(msg, "not persisted") && strings.Contains(msg, "store") && strings.Contains(msg, "item")) ||
		(strings.Contains(msg, "remove this item from your input") && strings.Contains(msg, "store")) ||
		(strings.Contains(msg, "stateless") && strings.Contains(msg, "item") && strings.Contains(msg, "id"))
}

func matchesModelFailure(msg, observedParam, observedCode string) bool {
	if strings.EqualFold(strings.TrimSpace(observedParam), "model") {
		return true
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(observedCode)), "model_not_found") {
		return true
	}
	if !strings.Contains(msg, "model") {
		return false
	}
	return strings.Contains(msg, "unsupported") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "not_found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "invalid model")
}

func stringifyAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(value)
	}
}

func isHexTraceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func deriveInteractionType(req AnthropicMessagesRequest, marker *subagentMarker, compactType int) string {
	switch {
	case compactType == compactRequest:
		return "compact_request"
	case compactType == compactAutoContinue:
		return "compact_auto_continue"
	case marker != nil:
		return "subagent"
	case requestHasToolResultHistory(req):
		return "tool_followup"
	default:
		return "conversation"
	}
}

func deriveInteractionID(rootSessionID, requestID, interactionType string) string {
	base := firstNonEmpty(rootSessionID, requestID)
	if base == "" {
		return ""
	}
	if interactionType == "" {
		return stableUUID(base)
	}
	return stableUUID(base + ":" + requestID + ":" + interactionType)
}

func sanitizedMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		default:
			return r
		}
	}, value)
	const maxMetadataValueLen = 256
	if len(value) > maxMetadataValueLen {
		return value[:maxMetadataValueLen]
	}
	return value
}

func derivedPromptCacheKey(req AnthropicMessagesRequest, headers http.Header) string {
	if sessionID := parseMetadataUserSessionID(req.Metadata); sessionID != "" {
		return sessionID
	}
	for _, header := range []string{"x-session-id", "x-claude-code-session-id"} {
		if headers != nil {
			if value := strings.TrimSpace(headers.Get(header)); value != "" {
				return value
			}
		}
	}
	return ""
}

func deriveRootSessionID(req AnthropicMessagesRequest, headers http.Header, promptCacheKey string) string {
	sessionSource := strings.TrimSpace(promptCacheKey)
	if sessionSource == "" && headers != nil {
		for _, header := range []string{"x-session-id", "x-claude-code-session-id"} {
			if value := strings.TrimSpace(headers.Get(header)); value != "" {
				sessionSource = value
				break
			}
		}
	}
	if sessionSource == "" {
		return ""
	}
	return stableUUID(sessionSource)
}

func parseMetadataUserSessionID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	rawUserID, ok := metadata["user_id"]
	if !ok {
		return ""
	}
	userID, ok := rawUserID.(string)
	if !ok {
		return ""
	}
	return parseUserIDMetadata(userID).SessionID
}

type parsedUserIDMetadata struct {
	SafetyIdentifier string
	SessionID        string
}

func parseUserIDMetadata(userID string) parsedUserIDMetadata {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return parsedUserIDMetadata{}
	}

	legacySafetyIdentifier := ""
	if match := regexpStringSubmatch(`user_([^_]+)_account`, userID); len(match) == 2 {
		legacySafetyIdentifier = match[1]
	}
	legacySessionID := ""
	if match := regexpStringSubmatch(`_session_(.+)$`, userID); len(match) == 2 {
		legacySessionID = match[1]
	}
	if legacySafetyIdentifier != "" || legacySessionID != "" {
		return parsedUserIDMetadata{
			SafetyIdentifier: legacySafetyIdentifier,
			SessionID:        legacySessionID,
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(userID), &parsed); err != nil {
		return parsedUserIDMetadata{}
	}
	return parsedUserIDMetadata{
		SafetyIdentifier: firstNonEmpty(asString(parsed["device_id"]), asString(parsed["account_uuid"])),
		SessionID:        strings.TrimSpace(asString(parsed["session_id"])),
	}
}

func regexpStringSubmatch(pattern, text string) []string {
	re := regexpCache(pattern)
	return re.FindStringSubmatch(text)
}

var regexCache sync.Map

func regexpCache(pattern string) *regexp.Regexp {
	if compiled, ok := regexCache.Load(pattern); ok {
		return compiled.(*regexp.Regexp)
	}
	re := regexp.MustCompile(pattern)
	actual, _ := regexCache.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp)
}

func deriveRequestID(req AnthropicMessagesRequest, rootSessionID string) string {
	lastUserContent := findLastUserContent(req.Messages)
	switch {
	case lastUserContent != "" && rootSessionID != "":
		return stableUUID(rootSessionID + lastUserContent)
	case lastUserContent != "":
		return stableUUID(lastUserContent)
	case rootSessionID != "":
		return stableUUID(rootSessionID + req.Model)
	default:
		return ""
	}
}

func findLastUserContent(messages []AnthropicMessage) string {
	for idx := len(messages) - 1; idx >= 0; idx-- {
		message := messages[idx]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		switch typed := message.Content.(type) {
		case string:
			return typed
		default:
			blocks, err := normalizeContentBlocks(typed)
			if err != nil || len(blocks) == 0 {
				continue
			}
			filtered := make([]AnthropicContentBlock, 0, len(blocks))
			for _, block := range blocks {
				if block.Type == "tool_result" {
					continue
				}
				block.CacheControl = nil
				filtered = append(filtered, block)
			}
			if len(filtered) == 0 {
				continue
			}
			blob, err := json.Marshal(filtered)
			if err != nil {
				continue
			}
			return string(blob)
		}
	}
	return ""
}

func stableUUID(input string) string {
	sum := sha256.Sum256([]byte(input))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		bytes[0:4],
		bytes[4:6],
		bytes[6:8],
		bytes[8:10],
		bytes[10:16],
	)
}

const toolReferenceTurnBoundary = "Tool loaded."

type preprocessOptions struct {
	compactType int
}

func detectCompactType(req AnthropicMessagesRequest) int {
	lastMessage := lastMessage(req.Messages)
	if isCompactMessage(lastMessage) {
		return compactRequest
	}
	if isCompactAutoContinueMessage(lastMessage) {
		return compactAutoContinue
	}
	systemBlocks, err := normalizeSystemBlocks(req.System)
	if err != nil {
		return compactNone
	}
	for _, block := range systemBlocks {
		if strings.HasPrefix(strings.TrimSpace(block.Text), compactSystemPromptStart) {
			return compactRequest
		}
	}
	return compactNone
}

func lastMessage(messages []AnthropicMessage) AnthropicMessage {
	if len(messages) == 0 {
		return AnthropicMessage{}
	}
	return messages[len(messages)-1]
}

func isCompactMessage(message AnthropicMessage) bool {
	text := compactCandidateText(message)
	if text == "" {
		return false
	}
	if !strings.Contains(text, compactTextOnlyGuard) || !strings.Contains(text, compactSummaryPromptStart) {
		return false
	}
	for _, section := range compactMessageSections {
		if strings.Contains(text, section) {
			return true
		}
	}
	return false
}

func isCompactAutoContinueMessage(message AnthropicMessage) bool {
	text := compactCandidateText(message)
	if text == "" {
		return false
	}
	for _, promptStart := range compactAutoContinuePromptStarts {
		if strings.HasPrefix(text, promptStart) {
			return true
		}
	}
	return false
}

func compactCandidateText(message AnthropicMessage) string {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
		return ""
	}
	switch typed := message.Content.(type) {
	case string:
		return typed
	default:
		blocks, err := normalizeContentBlocks(typed)
		if err != nil {
			return ""
		}
		texts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "text" {
				continue
			}
			text := block.Text
			if strings.HasPrefix(text, "<system-reminder>") {
				continue
			}
			if strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
}

func warmupCandidateText(message AnthropicMessage) string {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
		return ""
	}
	switch typed := message.Content.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		blocks, err := normalizeContentBlocks(typed)
		if err != nil || len(blocks) == 0 {
			return ""
		}
		texts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "text" {
				return ""
			}
			text := strings.TrimSpace(stripSubagentMarkerFromText(block.Text))
			if text == "" || strings.Contains(text, "<system-reminder>") {
				return ""
			}
			texts = append(texts, text)
		}
		return strings.Join(texts, "\n\n")
	}
}

func preprocessMessagesForClaude(req *AnthropicMessagesRequest, opts preprocessOptions) {
	sanitizeIDETools(req)
	stripSystemCacheControl(req)
	stripSubagentMarkers(req)
	stripToolReferenceTurnBoundary(req)
	mergeToolResultForClaude(req, opts)
	filterAssistantThinkingBlocks(req)
}

func sanitizeIDETools(req *AnthropicMessagesRequest) {
	if len(req.Tools) == 0 {
		return
	}
	filtered := make([]AnthropicTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		switch tool.Name {
		case ideExecuteCodeTool:
			continue
		case ideGetDiagnosticsTool:
			tool.Description = ideGetDiagnosticsDescription
		}
		filtered = append(filtered, tool)
	}
	req.Tools = filtered
}

func stripSubagentMarkers(req *AnthropicMessagesRequest) {
	for idx, message := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil || len(blocks) == 0 {
			continue
		}
		filtered := make([]AnthropicContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "text" {
				filtered = append(filtered, block)
				continue
			}
			cleaned := stripSubagentMarkerFromText(block.Text)
			if strings.TrimSpace(cleaned) == "" {
				continue
			}
			block.Text = cleaned
			filtered = append(filtered, block)
		}
		req.Messages[idx].Content = filtered
	}
}

func stripSystemCacheControl(req *AnthropicMessagesRequest) {
	systemBlocks, err := normalizeSystemBlocks(req.System)
	if err != nil || len(systemBlocks) == 0 {
		return
	}
	for idx := range systemBlocks {
		if systemBlocks[idx].CacheControl != nil {
			delete(systemBlocks[idx].CacheControl, "scope")
			if len(systemBlocks[idx].CacheControl) == 0 {
				systemBlocks[idx].CacheControl = nil
			}
		}
	}
	req.System = systemBlocks
}

func stripToolReferenceTurnBoundary(req *AnthropicMessagesRequest) {
	for idx, message := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}

		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil || len(blocks) == 0 {
			continue
		}

		hasToolResult := false
		for _, block := range blocks {
			if block.Type == "tool_result" {
				hasToolResult = true
				break
			}
		}
		if !hasToolResult {
			continue
		}

		filtered := make([]AnthropicContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "text" && strings.TrimSpace(block.Text) == toolReferenceTurnBoundary {
				continue
			}
			filtered = append(filtered, block)
		}
		req.Messages[idx].Content = filtered
	}
}

func mergeToolResultForClaude(req *AnthropicMessagesRequest, opts preprocessOptions) {
	for idx, message := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		if opts.compactType == compactRequest && idx == len(req.Messages)-1 {
			continue
		}

		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil || len(blocks) == 0 {
			continue
		}

		toolResults := make([]AnthropicContentBlock, 0)
		textBlocks := make([]AnthropicContentBlock, 0)
		valid := true
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				toolResults = append(toolResults, block)
			case "text":
				textBlocks = append(textBlocks, block)
			default:
				valid = false
			}
			if !valid {
				break
			}
		}

		if !valid || len(toolResults) == 0 || len(textBlocks) == 0 {
			continue
		}

		merged := mergeToolResultBlocks(toolResults, textBlocks)
		req.Messages[idx].Content = merged
	}
}

func filterAssistantThinkingBlocks(req *AnthropicMessagesRequest) {
	for idx, message := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			continue
		}
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil || len(blocks) == 0 {
			continue
		}
		filtered := make([]AnthropicContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "thinking" {
				filtered = append(filtered, block)
				continue
			}
			if _, ok := decodeCompactionCarrier(block.Signature); ok {
				filtered = append(filtered, block)
				continue
			}
			if strings.TrimSpace(block.Thinking) == "" || block.Thinking == "Thinking..." {
				continue
			}
			if strings.TrimSpace(block.Signature) == "" {
				continue
			}
			if strings.Contains(block.Signature, "@") {
				continue
			}
			filtered = append(filtered, block)
		}
		req.Messages[idx].Content = filtered
	}
}

func mergeToolResultBlocks(toolResults, textBlocks []AnthropicContentBlock) []AnthropicContentBlock {
	if len(toolResults) == len(textBlocks) {
		merged := make([]AnthropicContentBlock, 0, len(toolResults))
		for i, toolResult := range toolResults {
			merged = append(merged, mergeToolResultBlockWithTexts(toolResult, []AnthropicContentBlock{textBlocks[i]}))
		}
		return merged
	}

	merged := append([]AnthropicContentBlock(nil), toolResults...)
	last := len(merged) - 1
	merged[last] = mergeToolResultBlockWithTexts(merged[last], textBlocks)
	return merged
}

func mergeToolResultBlockWithTexts(toolResult AnthropicContentBlock, textBlocks []AnthropicContentBlock) AnthropicContentBlock {
	if len(textBlocks) == 0 {
		return toolResult
	}

	texts := make([]string, 0, len(textBlocks))
	for _, block := range textBlocks {
		if strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	if len(texts) == 0 {
		return toolResult
	}

	if contentText, ok := toolResult.Content.(string); ok {
		toolResult.Content = contentText + "\n\n" + strings.Join(texts, "\n\n")
		return toolResult
	}

	contentBlocks, err := normalizeContentBlocks(toolResult.Content)
	if err != nil {
		return toolResult
	}
	for _, block := range contentBlocks {
		if block.Type == "tool_reference" {
			return toolResult
		}
	}

	mergedContent := append([]AnthropicContentBlock(nil), contentBlocks...)
	mergedContent = append(mergedContent, textBlocks...)
	toolResult.Content = mergedContent
	return toolResult
}

func toolResultHasToolReference(toolResult AnthropicContentBlock) bool {
	contentBlocks, err := normalizeContentBlocks(toolResult.Content)
	if err != nil {
		return false
	}
	for _, block := range contentBlocks {
		if block.Type == "tool_reference" {
			return true
		}
	}
	return false
}

func normalizeContentBlocks(raw any) ([]AnthropicContentBlock, error) {
	switch typed := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []AnthropicContentBlock{{Type: "text", Text: typed}}, nil
	case AnthropicContentBlock:
		return []AnthropicContentBlock{typed}, nil
	case map[string]any:
		blob, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		var block AnthropicContentBlock
		if err := json.Unmarshal(blob, &block); err != nil {
			return nil, err
		}
		return []AnthropicContentBlock{block}, nil
	case []AnthropicContentBlock:
		return typed, nil
	case []any:
		blob, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(blob, &blocks); err != nil {
			return nil, err
		}
		return blocks, nil
	case json.RawMessage:
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(typed, &blocks); err == nil {
			return blocks, nil
		}
		var block AnthropicContentBlock
		if err := json.Unmarshal(typed, &block); err == nil {
			return []AnthropicContentBlock{block}, nil
		}
		var text string
		if err := json.Unmarshal(typed, &text); err == nil {
			return []AnthropicContentBlock{{Type: "text", Text: text}}, nil
		}
		return nil, fmt.Errorf("unsupported content payload")
	default:
		blob, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(blob, &blocks); err == nil {
			return blocks, nil
		}
		var block AnthropicContentBlock
		if err := json.Unmarshal(blob, &block); err == nil {
			return []AnthropicContentBlock{block}, nil
		}
		var text string
		if err := json.Unmarshal(blob, &text); err == nil {
			return []AnthropicContentBlock{{Type: "text", Text: text}}, nil
		}
		return nil, fmt.Errorf("unsupported content payload")
	}
}

func convertMessageBlocks(role string, blocks []AnthropicContentBlock, opts backendRequestOptions) ([]OpenAIInputItem, error) {
	var items []OpenAIInputItem
	var pending []OpenAIContentItem
	assistantPhase := ""
	if opts.EnablePhaseCommentary && strings.EqualFold(strings.TrimSpace(role), "assistant") {
		assistantPhase = resolveAssistantPhase(blocks)
	}

	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		item := OpenAIInputItem{
			Type:    "message",
			Role:    role,
			Content: append([]OpenAIContentItem(nil), pending...),
		}
		if assistantPhase != "" {
			item.Phase = assistantPhase
		}
		items = append(items, item)
		pending = nil
	}

	for _, block := range blocks {
		switch block.Type {
		case "", "text":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			pending = append(pending, OpenAIContentItem{
				Type: contentTextTypeForRole(role),
				Text: block.Text,
			})
		case "image":
			item, err := convertImageBlock(block)
			if err != nil {
				return nil, err
			}
			pending = append(pending, item)
		case "document", "file":
			if strings.TrimSpace(block.Context) != "" {
				pending = append(pending, OpenAIContentItem{
					Type: "input_text",
					Text: block.Context,
				})
			}
			item, err := convertDocumentBlock(block)
			if err != nil {
				return nil, err
			}
			pending = append(pending, item)
		case "tool_use", "tool_result", "thinking", "redacted_thinking":
			flushPending()
			converted, err := convertSemanticAnthropicBlock(block, opts)
			if err != nil {
				return nil, err
			}
			items = append(items, converted...)
		default:
			return nil, fmt.Errorf("unsupported anthropic block type %q", block.Type)
		}
	}

	flushPending()
	return items, nil
}

func convertSemanticAnthropicBlock(block AnthropicContentBlock, opts backendRequestOptions) ([]OpenAIInputItem, error) {
	switch block.Type {
	case "tool_use":
		return []OpenAIInputItem{convertToolUseInputItem(block)}, nil
	case "tool_result":
		return []OpenAIInputItem{convertToolResultInputItem(block, opts)}, nil
	case "thinking", "redacted_thinking":
		if item, ok := convertReasoningOrCompactionInputItem(block, opts.PreserveReasoningItems); ok {
			return []OpenAIInputItem{item}, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported semantic anthropic block type %q", block.Type)
	}
}

func convertToolUseInputItem(block AnthropicContentBlock) OpenAIInputItem {
	arguments := strings.TrimSpace(string(block.Input))
	if arguments == "" {
		arguments = "{}"
	}
	callID := block.ID
	if callID == "" {
		callID = "call_missing_id"
	}
	return OpenAIInputItem{
		Type:      "function_call",
		Name:      block.Name,
		CallID:    callID,
		Arguments: arguments,
	}
}

func convertToolResultInputItem(block AnthropicContentBlock, opts backendRequestOptions) OpenAIInputItem {
	return OpenAIInputItem{
		Type:   "function_call_output",
		CallID: block.ToolUseID,
		Output: convertToolResultOutput(block.Content, opts.PreserveStructuredOutput),
		Status: toolResultStatus(block),
	}
}

func toolResultStatus(block AnthropicContentBlock) string {
	if block.IsError {
		return "incomplete"
	}
	return "completed"
}

func convertReasoningOrCompactionInputItem(block AnthropicContentBlock, preserveReasoning bool) (OpenAIInputItem, bool) {
	carrier := anthropicThinkingCarrier(block)
	if carrier == "" {
		return OpenAIInputItem{}, false
	}
	if item, ok := decodeCompactionCarrier(carrier); ok {
		return item, true
	}
	if !preserveReasoning {
		return OpenAIInputItem{}, false
	}
	return decodeReasoningCarrier(carrier)
}

func anthropicThinkingCarrier(block AnthropicContentBlock) string {
	switch block.Type {
	case "thinking":
		return strings.TrimSpace(block.Signature)
	case "redacted_thinking":
		return strings.TrimSpace(block.Data)
	default:
		return ""
	}
}

func convertImageBlock(block AnthropicContentBlock) (OpenAIContentItem, error) {
	if block.Source == nil {
		return OpenAIContentItem{}, fmt.Errorf("image block missing source")
	}

	switch block.Source.Type {
	case "base64":
		if block.Source.MediaType == "" || block.Source.Data == "" {
			return OpenAIContentItem{}, fmt.Errorf("image base64 source requires media_type and data")
		}
		return OpenAIContentItem{
			Type:     "input_image",
			ImageURL: dataURL(block.Source.MediaType, block.Source.Data),
		}, nil
	case "url":
		if block.Source.URL == "" {
			return OpenAIContentItem{}, fmt.Errorf("image url source requires url")
		}
		return OpenAIContentItem{
			Type:     "input_image",
			ImageURL: block.Source.URL,
		}, nil
	case "file":
		if block.Source.FileID == "" {
			return OpenAIContentItem{}, fmt.Errorf("image file source requires file_id")
		}
		return OpenAIContentItem{
			Type:   "input_image",
			FileID: block.Source.FileID,
		}, nil
	default:
		return OpenAIContentItem{}, fmt.Errorf("unsupported image source type %q", block.Source.Type)
	}
}

func convertDocumentBlock(block AnthropicContentBlock) (OpenAIContentItem, error) {
	if block.Source == nil {
		return OpenAIContentItem{}, fmt.Errorf("%s block missing source", block.Type)
	}

	filename := block.Title
	if strings.TrimSpace(filename) == "" {
		filename = defaultDocumentFilename(block.Source.MediaType)
	}

	switch block.Source.Type {
	case "base64":
		if block.Source.Data == "" {
			return OpenAIContentItem{}, fmt.Errorf("%s base64 source requires data", block.Type)
		}
		fileData := block.Source.Data
		if shouldWrapFileDataAsDataURL(block.Source.MediaType) {
			fileData = dataURL(block.Source.MediaType, block.Source.Data)
		}
		return OpenAIContentItem{
			Type:     "input_file",
			Filename: filename,
			FileData: fileData,
		}, nil
	case "url":
		if block.Source.URL == "" {
			return OpenAIContentItem{}, fmt.Errorf("%s url source requires url", block.Type)
		}
		return OpenAIContentItem{
			Type:     "input_file",
			Filename: filename,
			FileURL:  block.Source.URL,
		}, nil
	case "file":
		if block.Source.FileID == "" {
			return OpenAIContentItem{}, fmt.Errorf("%s file source requires file_id", block.Type)
		}
		return OpenAIContentItem{
			Type:   "input_file",
			FileID: block.Source.FileID,
		}, nil
	case "text":
		if block.Source.Data == "" {
			return OpenAIContentItem{}, fmt.Errorf("%s text source requires data", block.Type)
		}
		return OpenAIContentItem{
			Type: "input_text",
			Text: block.Source.Data,
		}, nil
	default:
		return OpenAIContentItem{}, fmt.Errorf("unsupported %s source type %q", block.Type, block.Source.Type)
	}
}

func dataURL(mediaType, data string) string {
	return "data:" + fallback(mediaType, "application/octet-stream") + ";base64," + data
}

func shouldWrapFileDataAsDataURL(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "" || strings.HasPrefix(mediaType, "text/")
}

func defaultDocumentFilename(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/pdf":
		return "document.pdf"
	case "text/plain":
		return "document.txt"
	case "text/markdown":
		return "document.md"
	case "text/csv":
		return "document.csv"
	default:
		return "document"
	}
}

func encodeCompactionCarrier(id, encryptedContent string) string {
	id = strings.TrimSpace(id)
	encryptedContent = strings.TrimSpace(encryptedContent)
	if id == "" || encryptedContent == "" {
		return ""
	}
	return compactionCarrierPrefix + encryptedContent + compactionCarrierSep + id
}

func decodeCompactionCarrier(raw string) (OpenAIInputItem, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, compactionCarrierPrefix) {
		return OpenAIInputItem{}, false
	}
	rest := strings.TrimPrefix(raw, compactionCarrierPrefix)
	separatorIndex := strings.Index(rest, compactionCarrierSep)
	if separatorIndex <= 0 || separatorIndex == len(rest)-1 {
		return OpenAIInputItem{}, false
	}
	encryptedContent := rest[:separatorIndex]
	id := rest[separatorIndex+1:]
	if strings.TrimSpace(encryptedContent) == "" || strings.TrimSpace(id) == "" {
		return OpenAIInputItem{}, false
	}
	return OpenAIInputItem{
		ID:               id,
		Type:             "compaction",
		EncryptedContent: encryptedContent,
	}, true
}

func encodeReasoningCarrier(item OpenAIOutputItem) string {
	if item.Type != "reasoning" {
		return ""
	}
	sanitized := item
	if strings.TrimSpace(sanitized.EncryptedContent) == "" {
		sanitized.ID = ""
	}
	payload, err := json.Marshal(sanitized)
	if err != nil {
		return ""
	}
	return opaqueReasoningPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeReasoningCarrier(raw string) (OpenAIInputItem, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, opaqueReasoningPrefix) {
		return OpenAIInputItem{}, false
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, opaqueReasoningPrefix))
	if err != nil {
		return OpenAIInputItem{}, false
	}
	var item OpenAIOutputItem
	if err := json.Unmarshal(blob, &item); err != nil {
		return OpenAIInputItem{}, false
	}
	if item.Type != "reasoning" {
		return OpenAIInputItem{}, false
	}
	return OpenAIInputItem{
		ID:               item.ID,
		Type:             "reasoning",
		Content:          outputContentToInputContent(item.Content),
		EncryptedContent: item.EncryptedContent,
		Summary:          item.Summary,
		Status:           item.Status,
	}, true
}

func stripReasoningItemIDsWithoutEncryptedContent(items []OpenAIInputItem) []OpenAIInputItem {
	var sanitized []OpenAIInputItem
	for idx, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Type), "reasoning") {
			continue
		}
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.EncryptedContent) != "" {
			continue
		}
		if sanitized == nil {
			sanitized = append([]OpenAIInputItem(nil), items...)
		}
		sanitized[idx].ID = ""
	}
	if sanitized == nil {
		return items
	}
	return sanitized
}

func stripRoundTripHistoryItemIDs(items []OpenAIInputItem) []OpenAIInputItem {
	var sanitized []OpenAIInputItem
	for idx, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(item.Type)) {
		case "reasoning", "compaction":
			if sanitized == nil {
				sanitized = append([]OpenAIInputItem(nil), items...)
			}
			sanitized[idx].ID = ""
		}
	}
	if sanitized == nil {
		return items
	}
	return sanitized
}

func outputContentToInputContent(content []OpenAIOutputContent) []OpenAIContentItem {
	if len(content) == 0 {
		return nil
	}
	converted := make([]OpenAIContentItem, 0, len(content))
	for _, part := range content {
		converted = append(converted, OpenAIContentItem{
			Type: part.Type,
			Text: part.Text,
		})
	}
	return converted
}

func flattenToolResult(raw any) string {
	switch typed := raw.(type) {
	case nil:
		return ""
	case string:
		if extracted, ok := extractStructuredToolResultText(typed); ok {
			return extracted
		}
		return typed
	case []AnthropicContentBlock:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			switch block.Type {
			case "", "text":
				if strings.TrimSpace(block.Text) != "" {
					parts = append(parts, block.Text)
				}
			case "tool_reference":
				if strings.TrimSpace(block.ToolName) != "" {
					parts = append(parts, "Tool "+strings.TrimSpace(block.ToolName)+" loaded")
				} else {
					parts = append(parts, "[tool reference loaded]")
				}
			case "json":
				if text := stableJSONText(firstNonNil(block.JSON, block.Content)); text != "" {
					parts = append(parts, text)
				}
			case "image":
				parts = append(parts, summarizeToolResultImage(block))
			case "document", "file":
				parts = append(parts, summarizeToolResultDocument(block))
			default:
				blob, err := json.Marshal(block)
				if err != nil {
					parts = append(parts, fmt.Sprintf("[%s]", block.Type))
					continue
				}
				parts = append(parts, string(blob))
			}
		}
		return strings.Join(parts, "\n")
	case []any:
		blocks, err := normalizeContentBlocks(typed)
		if err == nil {
			return flattenToolResult(blocks)
		}
		blob, _ := json.Marshal(typed)
		return string(blob)
	default:
		if extracted, ok := extractStructuredToolResultText(typed); ok {
			return extracted
		}
		blob, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", raw)
		}
		return string(blob)
	}
}

type toolResultOutputProjection struct {
	raw                 any
	rawBlockList        bool
	blocks              []AnthropicContentBlock
	preservedStructured any
}

func newToolResultOutputProjection(raw any) toolResultOutputProjection {
	projection := toolResultOutputProjection{
		raw:          raw,
		rawBlockList: isRawToolResultBlockList(raw),
	}
	if preserved, ok := preserveUnsupportedStructuredToolResult(raw); ok {
		projection.preservedStructured = preserved
	}
	blocks, err := normalizeContentBlocks(raw)
	if err == nil && len(blocks) > 0 {
		projection.blocks = blocks
	}
	return projection
}

func (p toolResultOutputProjection) flatten() string {
	if len(p.blocks) > 0 {
		return flattenToolResult(p.blocks)
	}
	return flattenToolResult(p.raw)
}

func (p toolResultOutputProjection) extractStructuredText() (string, bool) {
	if p.rawBlockList {
		return "", false
	}
	return extractStructuredToolResultText(p.raw)
}

func (p toolResultOutputProjection) shouldPreferText() bool {
	if len(p.blocks) == 0 {
		return true
	}
	return hasTextOnlyToolResultBlocks(p.blocks) || hasSummaryOnlyToolResultBlock(p.blocks)
}

func (p toolResultOutputProjection) structured() any {
	if p.preservedStructured != nil {
		return p.preservedStructured
	}
	if len(p.blocks) == 0 {
		return nil
	}
	if hasUnsupportedStructuredToolResultBlock(p.blocks) {
		return preserveToolResultContentAsJSON(p.blocks)
	}

	content := make([]OpenAIContentItem, 0, len(p.blocks))
	for _, block := range p.blocks {
		switch block.Type {
		case "", "text":
			if strings.TrimSpace(block.Text) != "" {
				content = append(content, OpenAIContentItem{Type: "input_text", Text: block.Text})
			}
		case "tool_reference":
			name := strings.TrimSpace(block.ToolName)
			if name == "" {
				name = "unknown"
			}
			content = append(content, OpenAIContentItem{Type: "input_text", Text: "Tool " + name + " loaded"})
		case "json":
			if text := stableJSONText(firstNonNil(block.JSON, block.Content)); text != "" {
				content = append(content, OpenAIContentItem{Type: "input_text", Text: text})
			}
		case "image":
			if item, err := convertImageBlock(block); err == nil {
				content = append(content, item)
			} else {
				content = append(content, OpenAIContentItem{Type: "input_text", Text: summarizeToolResultImage(block)})
			}
		case "document", "file":
			content = append(content, OpenAIContentItem{Type: "input_text", Text: summarizeToolResultDocument(block)})
		default:
			blob, err := json.Marshal(block)
			if err != nil {
				content = append(content, OpenAIContentItem{Type: "input_text", Text: fmt.Sprintf("[%s]", block.Type)})
			} else {
				content = append(content, OpenAIContentItem{Type: "input_text", Text: string(blob)})
			}
		}
	}
	if len(content) == 0 {
		return nil
	}
	return content
}

func convertToolResultOutput(raw any, allowStructured bool) any {
	projection := newToolResultOutputProjection(raw)
	if !allowStructured {
		return projection.flatten()
	}
	if text, ok := projection.extractStructuredText(); ok {
		return text
	}
	if projection.shouldPreferText() {
		return projection.flatten()
	}
	if structured := projection.structured(); structured != nil {
		return structured
	}
	return projection.flatten()
}

func isRawToolResultBlockList(raw any) bool {
	switch raw.(type) {
	case []any, []AnthropicContentBlock:
		return true
	default:
		return false
	}
}

func preserveUnsupportedStructuredToolResult(raw any) (any, bool) {
	switch typed := raw.(type) {
	case []any:
		if rawBlocksHaveUnsupportedToolResultType(typed) {
			return typed, true
		}
	case []AnthropicContentBlock:
		if hasUnsupportedStructuredToolResultBlock(typed) {
			return typed, true
		}
	}
	return nil, false
}

func rawBlocksHaveUnsupportedToolResultType(blocks []any) bool {
	for _, block := range blocks {
		record, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType := strings.TrimSpace(asString(record["type"]))
		if blockType == "" {
			blockType = "text"
		}
		if !isStructuredToolResultTypeSupported(blockType) {
			return true
		}
	}
	return false
}

func hasUnsupportedStructuredToolResultBlock(blocks []AnthropicContentBlock) bool {
	for _, block := range blocks {
		blockType := block.Type
		if blockType == "" {
			blockType = "text"
		}
		if !isStructuredToolResultTypeSupported(blockType) {
			return true
		}
	}
	return false
}

func isStructuredToolResultTypeSupported(blockType string) bool {
	switch blockType {
	case "text", "tool_reference", "json", "image", "document", "file":
		return true
	default:
		return false
	}
}

func hasSummaryOnlyToolResultBlock(blocks []AnthropicContentBlock) bool {
	for _, block := range blocks {
		switch block.Type {
		case "image", "document", "file":
			return true
		}
	}
	return false
}

func hasTextOnlyToolResultBlocks(blocks []AnthropicContentBlock) bool {
	for _, block := range blocks {
		if block.Type != "" && block.Type != "text" {
			return false
		}
	}
	return true
}

func preserveToolResultContentAsJSON(blocks []AnthropicContentBlock) string {
	blob, err := json.Marshal(map[string]any{"content": blocks})
	if err != nil {
		return flattenToolResult(blocks)
	}
	return string(blob)
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stableJSONText(value any) string {
	if value == nil {
		return ""
	}
	blob, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(blob)
}

func extractStructuredToolResultText(raw any) (string, bool) {
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || !(strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
			return "", false
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return "", false
		}
		return extractStructuredToolResultText(decoded)
	case map[string]any:
		if value, ok := typed["result"]; ok {
			if text := flattenStructuredValue(value); strings.TrimSpace(text) != "" {
				return text, true
			}
		}
		if value, ok := typed["content"]; ok {
			if text := flattenStructuredValue(value); strings.TrimSpace(text) != "" {
				return text, true
			}
		}
		if value, ok := typed["text"]; ok {
			if text := flattenStructuredValue(value); strings.TrimSpace(text) != "" {
				return text, true
			}
		}
	case []any:
		if text := flattenStructuredValue(typed); strings.TrimSpace(text) != "" {
			return text, true
		}
	}
	return "", false
}

func flattenStructuredValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(flattenStructuredValue(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if blockType, _ := typed["type"].(string); blockType == "text" {
			if text, _ := typed["text"].(string); strings.TrimSpace(text) != "" {
				return text
			}
		}
		if text, _ := typed["text"].(string); strings.TrimSpace(text) != "" {
			return text
		}
		if result, ok := typed["result"]; ok {
			return flattenStructuredValue(result)
		}
		if content, ok := typed["content"]; ok {
			return flattenStructuredValue(content)
		}
	}
	return ""
}

func summarizeToolResultImage(block AnthropicContentBlock) string {
	if block.Source == nil {
		return "[image]"
	}
	if url := strings.TrimSpace(block.Source.URL); url != "" {
		return fmt.Sprintf("[image url=%s]", url)
	}
	if fileID := strings.TrimSpace(block.Source.FileID); fileID != "" {
		return fmt.Sprintf("[image file_id=%s]", fileID)
	}
	if dataLen := len(block.Source.Data); dataLen > 0 {
		mediaType := strings.TrimSpace(block.Source.MediaType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return fmt.Sprintf("[image base64 media_type=%s data_len=%d]", mediaType, dataLen)
	}
	return "[image]"
}

func summarizeToolResultDocument(block AnthropicContentBlock) string {
	display := block.Type
	if strings.TrimSpace(block.Title) != "" {
		display += " title=" + strings.TrimSpace(block.Title)
	}
	if block.Source == nil {
		return "[" + display + "]"
	}
	if url := strings.TrimSpace(block.Source.URL); url != "" {
		return fmt.Sprintf("[%s url=%s]", display, url)
	}
	if fileID := strings.TrimSpace(block.Source.FileID); fileID != "" {
		return fmt.Sprintf("[%s file_id=%s]", display, fileID)
	}
	if dataLen := len(block.Source.Data); dataLen > 0 {
		mediaType := strings.TrimSpace(block.Source.MediaType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return fmt.Sprintf("[%s base64 media_type=%s data_len=%d]", display, mediaType, dataLen)
	}
	return "[" + display + "]"
}

func toOpenAIContent(blocks []AnthropicContentBlock) []OpenAIContentItem {
	content := make([]OpenAIContentItem, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" || strings.TrimSpace(block.Text) == "" {
			continue
		}
		content = append(content, OpenAIContentItem{
			Type: "input_text",
			Text: block.Text,
		})
	}
	return content
}

func summarizeInputItems(items []OpenAIInputItem) string {
	if len(items) == 0 {
		return "none"
	}

	summaries := make([]string, 0, len(items))
	for index, item := range items {
		switch item.Type {
		case "message":
			contentTypes := make([]string, 0, len(item.Content))
			for _, content := range item.Content {
				contentTypes = append(contentTypes, content.Type)
			}
			summaries = append(summaries, fmt.Sprintf("%d:message(%s)[%s]", index, item.Role, strings.Join(contentTypes, ",")))
		case "function_call":
			summaries = append(summaries, fmt.Sprintf("%d:function_call(%s)", index, item.Name))
		case "function_call_output":
			summaries = append(summaries, fmt.Sprintf("%d:function_call_output(%s)", index, item.CallID))
		case "reasoning":
			summaries = append(summaries, fmt.Sprintf("%d:reasoning", index))
		default:
			summaries = append(summaries, fmt.Sprintf("%d:%s", index, item.Type))
		}
	}

	return strings.Join(summaries, " | ")
}

func summarizeTools(tools []OpenAITool) string {
	if len(tools) == 0 {
		return "none"
	}

	summaries := make([]string, 0, len(tools))
	for _, tool := range tools {
		summaries = append(summaries, tool.Name)
	}
	return strings.Join(summaries, ", ")
}

func summarizeFunctionCallOutputs(items []OpenAIInputItem) string {
	summaries := make([]string, 0)
	for _, item := range items {
		if item.Type != "function_call_output" {
			continue
		}
		summaries = append(summaries, fmt.Sprintf("%s=%s", item.CallID, sanitizeLogValue(outputLogString(item.Output))))
	}
	if len(summaries) == 0 {
		return "none"
	}
	return strings.Join(summaries, " | ")
}

func outputLogString(output any) string {
	switch typed := output.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		blob, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(blob)
	}
}

func convertTools(tools []AnthropicTool) []OpenAITool {
	if len(tools) == 0 {
		return nil
	}
	converted := make([]OpenAITool, 0, len(tools))
	for _, tool := range tools {
		converted = append(converted, OpenAITool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  normalizeToolSchema(tool.InputSchema),
		})
	}
	return converted
}

func normalizeToolSchema(schema any) any {
	switch typed := schema.(type) {
	case nil:
		return map[string]any{"type": "object", "properties": map[string]any{}}
	case map[string]any:
		return normalizeToolSchemaMap(typed)
	case map[string]json.RawMessage:
		blob, err := json.Marshal(typed)
		if err != nil {
			return schema
		}
		var decoded map[string]any
		if err := json.Unmarshal(blob, &decoded); err != nil {
			return schema
		}
		return normalizeToolSchemaMap(decoded)
	default:
		blob, err := json.Marshal(typed)
		if err != nil {
			return schema
		}
		var decoded map[string]any
		if err := json.Unmarshal(blob, &decoded); err != nil {
			return schema
		}
		return normalizeToolSchemaMap(decoded)
	}
}

func normalizeToolSchemaMap(schema map[string]any) map[string]any {
	normalized := make(map[string]any, len(schema)+1)
	for key, value := range schema {
		normalized[key] = normalizeToolSchemaField(key, value)
	}
	if schemaTypeIncludesObject(normalized["type"]) {
		if _, ok := normalized["properties"]; !ok {
			normalized["properties"] = map[string]any{}
		}
	}
	return normalized
}

func normalizeToolSchemaField(key string, value any) any {
	switch key {
	case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
		return normalizeSchemaMapValues(value)
	case "items", "additionalProperties", "not", "if", "then", "else":
		return normalizeToolSchemaValue(value)
	case "anyOf", "oneOf", "allOf":
		return normalizeSchemaList(value)
	default:
		return value
	}
}

func normalizeSchemaMapValues(value any) any {
	decoded := decodeJSONMapValue(value)
	if decoded == nil {
		return value
	}
	normalized := make(map[string]any, len(decoded))
	for key, nested := range decoded {
		normalized[key] = normalizeToolSchemaValue(nested)
	}
	return normalized
}

func normalizeSchemaList(value any) any {
	items, ok := value.([]any)
	if !ok {
		return value
	}
	normalized := make([]any, 0, len(items))
	for _, item := range items {
		normalized = append(normalized, normalizeToolSchemaValue(item))
	}
	return normalized
}

func normalizeToolSchemaValue(value any) any {
	if decoded := decodeJSONMapValue(value); decoded != nil {
		return normalizeToolSchemaMap(decoded)
	}
	return value
}

func decodeJSONMapValue(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case map[string]json.RawMessage:
		blob, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		var decoded map[string]any
		if err := json.Unmarshal(blob, &decoded); err != nil {
			return nil
		}
		return decoded
	default:
		return nil
	}
}

func schemaTypeIncludesObject(raw any) bool {
	switch typed := raw.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "object")
	case []any:
		for _, item := range typed {
			if schemaTypeIncludesObject(item) {
				return true
			}
		}
	case []string:
		for _, item := range typed {
			if strings.EqualFold(strings.TrimSpace(item), "object") {
				return true
			}
		}
	}
	return false
}

func convertToolChoice(choice *AnthropicToolChoice) any {
	if choice == nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]string{
			"type": "function",
			"name": choice.Name,
		}
	case "none":
		return "none"
	default:
		return nil
	}
}

func buildMetadata(headers http.Header, input map[string]any, continuity continuityContext, forwardUserMetadata bool, allowlist []string, includeContinuity bool, includeBridgeHeaders bool) map[string]string {
	metadata := map[string]string{}

	if forwardUserMetadata {
		for key, value := range input {
			if metadataKeyBlocked(key) {
				continue
			}
			if !metadataKeyAllowed(key, allowlist) {
				continue
			}
			switch typed := value.(type) {
			case string:
				if trimmed := sanitizedMetadataValue(typed); trimmed != "" {
					metadata[key] = trimmed
				}
			case fmt.Stringer:
				if trimmed := sanitizedMetadataValue(typed.String()); trimmed != "" {
					metadata[key] = trimmed
				}
			}
		}
	}

	if includeBridgeHeaders {
		for _, header := range []string{
			"x-claude-code-model",
			"x-claude-code-config-hash",
		} {
			if value := sanitizedMetadataValue(headers.Get(header)); value != "" {
				metadata[header] = value
			}
		}
	}

	if includeContinuity {
		if value := strings.TrimSpace(continuity.RootSessionID); value != "" {
			metadata["claude_code_root_session_id"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.RequestID); value != "" {
			metadata["claude_code_request_id"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.SessionAffinity); value != "" {
			metadata["claude_code_session_affinity"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.ParentSessionID); value != "" {
			metadata["claude_code_parent_session_id"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.InboundRequestID); value != "" {
			metadata["claude_code_inbound_request_id"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.TraceID); value != "" {
			metadata["claude_code_trace_id"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.InteractionType); value != "" {
			metadata["claude_code_interaction_type"] = sanitizedMetadataValue(value)
		}
		if value := strings.TrimSpace(continuity.InteractionID); value != "" {
			metadata["claude_code_interaction_id"] = sanitizedMetadataValue(value)
		}
		if continuity.Subagent != nil {
			if value := strings.TrimSpace(continuity.Subagent.AgentID); value != "" {
				metadata["claude_code_subagent_id"] = sanitizedMetadataValue(value)
			}
			if value := strings.TrimSpace(continuity.Subagent.AgentType); value != "" {
				metadata["claude_code_subagent_type"] = sanitizedMetadataValue(value)
			}
		}
	}

	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func metadataKeyBlocked(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return true
	}
	if key == "user_id" {
		return true
	}
	return strings.HasPrefix(key, "claude_code_")
}

func metadataKeyAllowed(key string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	key = strings.TrimSpace(key)
	for _, allowed := range allowlist {
		if key == allowed {
			return true
		}
	}
	return false
}

func translateBackendResponse(resp OpenAIResponsesResponse, advertisedModel string) (AnthropicMessageResponse, error) {
	content := make([]AnthropicResponseBlock, 0, len(resp.Output))
	stopReason := inferStopReason(resp)
	hasFinalAnswer := responseHasFinalAnswerPhase(resp)

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type != "output_text" {
					continue
				}
				if hasFinalAnswer && !isFinalAnswerPhase(item.Phase, part.Phase) {
					continue
				}
				content = append(content, AnthropicResponseBlock{
					Type: "text",
					Text: part.Text,
				})
			}
		case "function_call":
			if len(item.Arguments) > maxToolArgumentBytes {
				return AnthropicMessageResponse{}, fmt.Errorf("oversized function_call arguments in non-stream response")
			}
			input := map[string]any{}
			if strings.TrimSpace(item.Arguments) != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
					return AnthropicMessageResponse{}, fmt.Errorf("decode function call arguments: %w", err)
				}
			}
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			content = append(content, AnthropicResponseBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  item.Name,
				Input: input,
			})
		case "reasoning":
			if block, ok := translateReasoningOutputItem(item); ok {
				content = append(content, block)
			}
		case "compaction":
			if block, ok := translateCompactionOutputItem(item); ok {
				content = append(content, block)
			}
		}
	}

	return AnthropicMessageResponse{
		ID:           fallback(resp.ID, "msg_proxy"),
		Type:         "message",
		Role:         "assistant",
		Model:        advertisedModel,
		Content:      content,
		StopReason:   stopReason,
		StopSequence: nil,
		Usage: AnthropicUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

func responseHasFinalAnswerPhase(resp OpenAIResponsesResponse) bool {
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" && isFinalAnswerPhase(item.Phase, part.Phase) {
				return true
			}
		}
	}
	return false
}

func isFinalAnswerPhase(phases ...string) bool {
	for _, phase := range phases {
		if strings.EqualFold(strings.TrimSpace(phase), "final_answer") {
			return true
		}
	}
	return false
}

func inferStopReason(resp OpenAIResponsesResponse) string {
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			return "tool_use"
		}
	}

	if resp.IncompleteDetails != nil {
		switch resp.IncompleteDetails.Reason {
		case "max_output_tokens", "max_tokens":
			return "max_tokens"
		}
	}

	return "end_turn"
}

func translateReasoningOutputItem(item OpenAIOutputItem) (AnthropicResponseBlock, bool) {
	text := joinReasoningText(item)
	carrier := encodeReasoningCarrier(item)

	if text != "" {
		return AnthropicResponseBlock{
			Type:      "thinking",
			Thinking:  text,
			Signature: carrier,
		}, true
	}

	if carrier != "" {
		return AnthropicResponseBlock{
			Type: "redacted_thinking",
			Data: carrier,
		}, true
	}

	return AnthropicResponseBlock{}, false
}

func translateCompactionOutputItem(item OpenAIOutputItem) (AnthropicResponseBlock, bool) {
	carrier := encodeCompactionCarrier(item.ID, item.EncryptedContent)
	if carrier == "" {
		return AnthropicResponseBlock{}, false
	}
	return AnthropicResponseBlock{
		Type:      "thinking",
		Thinking:  defaultThinkingText,
		Signature: carrier,
	}, true
}

func joinReasoningText(item OpenAIOutputItem) string {
	var parts []string
	for _, summary := range item.Summary {
		if strings.TrimSpace(summary.Text) != "" {
			parts = append(parts, summary.Text)
		}
	}
	for _, content := range item.Content {
		switch content.Type {
		case "reasoning_text", "summary_text", "output_text":
			if strings.TrimSpace(content.Text) != "" {
				parts = append(parts, content.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

type sseTranslator struct {
	w                 io.Writer
	flusher           http.Flusher
	debugf            func(string, ...any)
	advertisedModel   string
	messageID         string
	started           bool
	nextBlockIndex    int
	seenToolUse       bool
	textBlocks        map[string]int
	closedTextBlocks  map[string]bool
	textBlockHasDelta map[string]bool
	textBlockBuffers  map[string]*strings.Builder
	toolBlocks        map[string]int
	closedToolBlocks  map[string]bool
	toolArguments     map[string]string
	toolWhitespaceRun map[string]int
	toolEmptyDeltas   map[string]int
	toolArgumentBytes map[string]int
	reasoningBlocks   map[string]int
	closedReasoning   map[string]bool
	reasoningBuffers  map[string]*strings.Builder
	toolNames         map[string]string
}

func newSSETranslator(w io.Writer, flusher http.Flusher, advertisedModel, messageID string, debugf func(string, ...any)) *sseTranslator {
	return &sseTranslator{
		w:                 w,
		flusher:           flusher,
		debugf:            debugf,
		advertisedModel:   advertisedModel,
		messageID:         messageID,
		textBlocks:        map[string]int{},
		closedTextBlocks:  map[string]bool{},
		textBlockHasDelta: map[string]bool{},
		textBlockBuffers:  map[string]*strings.Builder{},
		toolBlocks:        map[string]int{},
		closedToolBlocks:  map[string]bool{},
		toolArguments:     map[string]string{},
		toolWhitespaceRun: map[string]int{},
		toolEmptyDeltas:   map[string]int{},
		toolArgumentBytes: map[string]int{},
		reasoningBlocks:   map[string]int{},
		closedReasoning:   map[string]bool{},
		reasoningBuffers:  map[string]*strings.Builder{},
		toolNames:         map[string]string{},
	}
}

func (t *sseTranslator) consume(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var eventName string
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		return t.handleEvent(eventName, payload)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return dispatch()
}

func (t *sseTranslator) handleEvent(eventName, payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}

	var event OpenAIStreamEnvelope
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.Type == "" {
		event.Type = eventName
	}

	t.ensureMessageStart()

	switch event.Type {
	case "response.content_part.added":
		if event.Part != nil {
			switch event.Part.Type {
			case "output_text":
				blockKey := textBlockKey(event.ItemID, event.ContentIndex)
				t.emitTextSnapshot(blockKey, event.Part.Text)
			case "reasoning_text", "summary_text":
				t.emitReasoningSnapshot(event.ItemID, event.Part.Text)
			}
		}
	case "response.output_text.delta":
		blockKey := textBlockKey(event.ItemID, event.ContentIndex)
		t.appendTextDelta(blockKey, event.Delta)
	case "response.output_text.done", "response.content_part.done":
		if event.Part != nil {
			switch event.Part.Type {
			case "reasoning_text", "summary_text":
				t.emitReasoningSnapshot(event.ItemID, fallback(event.Text, event.Part.Text))
				break
			}
		}
		if event.Part != nil && (event.Part.Type == "reasoning_text" || event.Part.Type == "summary_text") {
			break
		}
		blockKey := textBlockKey(event.ItemID, event.ContentIndex)
		text := event.Text
		if event.Part != nil && strings.TrimSpace(text) == "" {
			text = event.Part.Text
		}
		if strings.TrimSpace(text) != "" {
			t.emitTextSnapshot(blockKey, text)
		}
		if index, ok := t.textBlocks[blockKey]; ok && !t.closedTextBlocks[blockKey] {
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
			t.closedTextBlocks[blockKey] = true
		}
	case "response.output_item.added":
		if event.Item != nil {
			switch event.Item.Type {
			case "function_call":
				t.toolNames[event.Item.ID] = event.Item.Name
				t.debugfTool("stream tool started item_id=%q call_id=%q name=%q", event.Item.ID, fallback(event.Item.CallID, event.Item.ID), event.Item.Name)
				if t.closedToolBlocks[event.Item.ID] {
					if err := t.acceptLateClosedToolArguments(event.Item.ID, event.Item.Arguments, "response.output_item.added"); err != nil {
						return err
					}
					break
				}
				index := t.startToolBlock(event.Item.ID, fallback(event.Item.CallID, event.Item.ID), event.Item.Name)
				if strings.TrimSpace(event.Item.Arguments) != "" {
					if err := t.validateToolArgumentsPayload(event.Item.ID, event.Item.Arguments); err != nil {
						return err
					}
					t.toolArguments[event.Item.ID] = event.Item.Arguments
					t.writeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": index,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": event.Item.Arguments,
						},
					})
				}
			case "reasoning":
				t.emitReasoningSnapshot(event.Item.ID, joinReasoningText(eventItemToOutput(*event.Item)))
			case "message":
				for contentIndex, content := range event.Item.Content {
					if content.Type != "output_text" {
						continue
					}
					t.emitTextSnapshot(textBlockKey(event.Item.ID, contentIndex), content.Text)
				}
			}
		}
	case "response.function_call_arguments.delta":
		if t.closedToolBlocks[event.ItemID] {
			return fmt.Errorf("function_call_arguments.delta after done")
		}
		callID := fallback(event.ItemID, "tool_missing")
		index := t.startToolBlock(event.ItemID, callID, t.toolNames[event.ItemID])
		if err := t.trackToolArgumentDelta(event.ItemID, event.Delta); err != nil {
			return err
		}
		t.toolArguments[event.ItemID] += event.Delta
		t.writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": event.Delta,
			},
		})
	case "response.function_call_arguments.done":
		if t.closedToolBlocks[event.ItemID] {
			return t.acceptLateClosedToolArguments(event.ItemID, fallback(event.Delta, event.Arguments), "response.function_call_arguments.done")
		}
		callID := fallback(event.ItemID, "tool_missing")
		index := t.startToolBlock(event.ItemID, callID, t.toolNames[event.ItemID])
		argumentsDelta := fallback(event.Delta, event.Arguments)
		if err := t.validateToolArgumentsPayload(event.ItemID, argumentsDelta); err != nil {
			return err
		}
		t.debugfTool("stream tool arguments item_id=%q call_id=%q name=%q arguments=%s", event.ItemID, callID, t.toolNames[event.ItemID], sanitizeLogValue(argumentsDelta))
		if remainder := t.remainingToolArguments(event.ItemID, argumentsDelta); strings.TrimSpace(remainder) != "" {
			t.writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": remainder,
				},
			})
		}
		if !t.closedToolBlocks[event.ItemID] {
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
			t.closedToolBlocks[event.ItemID] = true
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		t.appendReasoningDelta(event.ItemID, event.Delta)
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		t.emitReasoningSnapshot(event.ItemID, event.Text)
	case "response.output_item.done":
		if event.Item != nil {
			switch event.Item.Type {
			case "reasoning":
				t.emitReasoningSnapshot(event.Item.ID, joinReasoningText(eventItemToOutput(*event.Item)))
				carrier := encodeReasoningCarrier(eventItemToOutput(*event.Item))
				if strings.TrimSpace(t.currentReasoningText(event.Item.ID)) != "" {
					index := t.startThinkingBlock(event.Item.ID)
					if carrier != "" {
						t.writeEvent("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": index,
							"delta": map[string]any{
								"type":      "signature_delta",
								"signature": carrier,
							},
						})
					}
					if !t.closedReasoning[event.Item.ID] {
						t.writeEvent("content_block_stop", map[string]any{
							"type":  "content_block_stop",
							"index": index,
						})
						t.closedReasoning[event.Item.ID] = true
					}
				} else if carrier != "" && !t.closedReasoning[event.Item.ID] {
					index := t.startRedactedThinkingBlock(event.Item.ID, carrier)
					t.writeEvent("content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": index,
					})
					t.closedReasoning[event.Item.ID] = true
				}
			case "compaction":
				carrier := encodeCompactionCarrier(event.Item.ID, event.Item.EncryptedContent)
				if carrier == "" {
					break
				}
				index := t.startThinkingBlock(event.Item.ID)
				if strings.TrimSpace(t.currentReasoningText(event.Item.ID)) == "" {
					t.writeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": index,
						"delta": map[string]any{
							"type":     "thinking_delta",
							"thinking": defaultThinkingText,
						},
					})
					t.setReasoningBuffer(event.Item.ID, defaultThinkingText)
				}
				t.writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type":      "signature_delta",
						"signature": carrier,
					},
				})
				if !t.closedReasoning[event.Item.ID] {
					t.writeEvent("content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": index,
					})
					t.closedReasoning[event.Item.ID] = true
				}
			case "function_call":
				if t.closedToolBlocks[event.Item.ID] {
					if err := t.acceptLateClosedToolArguments(event.Item.ID, event.Item.Arguments, "response.output_item.done"); err != nil {
						return err
					}
					break
				}
				callID := fallback(event.Item.CallID, fallback(event.Item.ID, "tool_missing"))
				index := t.startToolBlock(event.Item.ID, callID, event.Item.Name)
				if err := t.validateToolArgumentsPayload(event.Item.ID, event.Item.Arguments); err != nil {
					return err
				}
				if remainder := t.remainingToolArguments(event.Item.ID, event.Item.Arguments); strings.TrimSpace(remainder) != "" {
					t.writeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": index,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": remainder,
						},
					})
				}
				if !t.closedToolBlocks[event.Item.ID] {
					t.writeEvent("content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": index,
					})
					t.closedToolBlocks[event.Item.ID] = true
				}
			case "message":
				for contentIndex, content := range event.Item.Content {
					if content.Type != "output_text" {
						continue
					}
					blockKey := textBlockKey(event.Item.ID, contentIndex)
					t.emitTextSnapshot(blockKey, content.Text)
					if index, ok := t.textBlocks[blockKey]; ok && !t.closedTextBlocks[blockKey] {
						t.writeEvent("content_block_stop", map[string]any{
							"type":  "content_block_stop",
							"index": index,
						})
						t.closedTextBlocks[blockKey] = true
					}
				}
			}
		}
	case "response.completed", "response.incomplete":
		t.closeOpenBlocks()
		stopReason := "end_turn"
		usage := AnthropicUsage{}
		if event.Response != nil {
			stopReason = inferStopReason(*event.Response)
			usage = AnthropicUsage{
				InputTokens:  event.Response.Usage.InputTokens,
				OutputTokens: event.Response.Usage.OutputTokens,
			}
		} else if t.seenToolUse {
			stopReason = "tool_use"
		}
		t.writeEvent("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": usage,
		})
		t.writeEvent("message_stop", map[string]any{
			"type": "message_stop",
		})
	case "response.failed", "response.error":
		t.closeOpenBlocks()
		message := "The response failed."
		if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
			message = event.Error.Message
		}
		if event.Response != nil && event.Response.Error != nil && strings.TrimSpace(event.Response.Error.Message) != "" {
			message = event.Response.Error.Message
		}
		t.writeAnthropicStreamError(message)
	case "error":
		if event.Error != nil {
			return errors.New(event.Error.Message)
		}
	}

	return nil
}

func (t *sseTranslator) currentTextBlockText(blockKey string) string {
	if builder, ok := t.textBlockBuffers[blockKey]; ok && builder != nil {
		return builder.String()
	}
	return ""
}

func (t *sseTranslator) setTextBlockBuffer(blockKey, text string) {
	builder := &strings.Builder{}
	builder.WriteString(text)
	t.textBlockBuffers[blockKey] = builder
}

func (t *sseTranslator) appendTextBuffer(blockKey, delta string) {
	builder, ok := t.textBlockBuffers[blockKey]
	if !ok || builder == nil {
		builder = &strings.Builder{}
		t.textBlockBuffers[blockKey] = builder
	}
	builder.WriteString(delta)
}

func (t *sseTranslator) currentReasoningText(itemID string) string {
	if builder, ok := t.reasoningBuffers[itemID]; ok && builder != nil {
		return builder.String()
	}
	return ""
}

func (t *sseTranslator) setReasoningBuffer(itemID, text string) {
	builder := &strings.Builder{}
	builder.WriteString(text)
	t.reasoningBuffers[itemID] = builder
}

func (t *sseTranslator) appendReasoningBuffer(itemID, delta string) {
	builder, ok := t.reasoningBuffers[itemID]
	if !ok || builder == nil {
		builder = &strings.Builder{}
		t.reasoningBuffers[itemID] = builder
	}
	builder.WriteString(delta)
}

func (t *sseTranslator) appendTextDelta(blockKey, delta string) {
	index := t.startTextBlock(blockKey)
	t.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": delta,
		},
	})
	t.textBlockHasDelta[blockKey] = true
	t.appendTextBuffer(blockKey, delta)
}

func (t *sseTranslator) emitTextSnapshot(blockKey, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	remainder := remainingSnapshotSuffix(t.currentTextBlockText(blockKey), text)
	if remainder == "" {
		if t.currentTextBlockText(blockKey) == "" {
			t.setTextBlockBuffer(blockKey, text)
		}
		return
	}
	index := t.startTextBlock(blockKey)
	t.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": remainder,
		},
	})
	t.textBlockHasDelta[blockKey] = true
	t.setTextBlockBuffer(blockKey, text)
}

func (t *sseTranslator) appendReasoningDelta(itemID, delta string) {
	index := t.startThinkingBlock(itemID)
	t.appendReasoningBuffer(itemID, delta)
	t.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": delta,
		},
	})
}

func (t *sseTranslator) emitReasoningSnapshot(itemID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	remainder := remainingSnapshotSuffix(t.currentReasoningText(itemID), text)
	if remainder == "" {
		if t.currentReasoningText(itemID) == "" {
			t.setReasoningBuffer(itemID, text)
		}
		return
	}
	index := t.startThinkingBlock(itemID)
	t.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": remainder,
		},
	})
	t.setReasoningBuffer(itemID, text)
}

func remainingSnapshotSuffix(current, full string) string {
	switch {
	case full == "":
		return ""
	case current == "":
		return full
	case full == current:
		return ""
	case strings.HasPrefix(full, current):
		return full[len(current):]
	default:
		return full
	}
}

func (t *sseTranslator) acceptLateClosedToolArguments(itemID, incoming, source string) error {
	incoming = strings.TrimSpace(incoming)
	current := strings.TrimSpace(t.toolArguments[itemID])
	if incoming == "" || incoming == current {
		return nil
	}
	if current != "" && strings.HasPrefix(incoming, current) {
		if err := t.validateToolArgumentsPayload(itemID, incoming); err != nil {
			return err
		}
		t.toolArguments[itemID] = incoming
		t.debugfTool("stream tool accepted late closed arguments item_id=%q source=%q", itemID, source)
		return nil
	}
	if source == "response.function_call_arguments.done" {
		return fmt.Errorf("duplicate function_call_arguments.done with conflicting arguments")
	}
	return fmt.Errorf("%s with conflicting arguments after done", source)
}

func (t *sseTranslator) trackToolArgumentDelta(itemID, delta string) error {
	if delta == "" {
		t.toolEmptyDeltas[itemID]++
		if t.toolEmptyDeltas[itemID] > maxToolEmptyDeltaCount {
			return fmt.Errorf("excessive empty function_call_arguments.delta")
		}
		return nil
	}
	t.toolEmptyDeltas[itemID] = 0
	if strings.TrimSpace(delta) == "" && delta != "" {
		t.toolWhitespaceRun[itemID] += len(delta)
		if t.toolWhitespaceRun[itemID] > 20 {
			return fmt.Errorf("excessive whitespace in function_call_arguments.delta")
		}
		t.toolArgumentBytes[itemID] += len(delta)
		if t.toolArgumentBytes[itemID] > maxToolArgumentBytes {
			return fmt.Errorf("oversized function_call_arguments")
		}
		return nil
	}
	t.toolWhitespaceRun[itemID] = 0
	t.toolArgumentBytes[itemID] += len(delta)
	if t.toolArgumentBytes[itemID] > maxToolArgumentBytes {
		return fmt.Errorf("oversized function_call_arguments")
	}
	return nil
}

func (t *sseTranslator) validateToolArgumentsPayload(itemID, arguments string) error {
	if len(arguments) == 0 {
		return nil
	}
	current := t.toolArguments[itemID]
	additional := len(arguments)
	switch {
	case arguments == current:
		additional = 0
	case strings.HasPrefix(arguments, current):
		additional = len(arguments) - len(current)
	}
	if len(current)+additional > maxToolArgumentBytes {
		return fmt.Errorf("oversized function_call_arguments")
	}
	return nil
}

func (t *sseTranslator) ensureMessageStart() {
	if t.started {
		return
	}
	t.started = true
	t.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.advertisedModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
}

func (t *sseTranslator) startTextBlock(blockKey string) int {
	if index, ok := t.textBlocks[blockKey]; ok {
		return index
	}
	index := t.nextBlockIndex
	t.nextBlockIndex++
	t.textBlocks[blockKey] = index
	t.writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	return index
}

func (t *sseTranslator) startToolBlock(itemID, callID, name string) int {
	if index, ok := t.toolBlocks[itemID]; ok {
		return index
	}
	index := t.nextBlockIndex
	t.nextBlockIndex++
	t.toolBlocks[itemID] = index
	t.seenToolUse = true
	t.writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    callID,
			"name":  name,
			"input": map[string]any{},
		},
	})
	return index
}

func (t *sseTranslator) startThinkingBlock(itemID string) int {
	if index, ok := t.reasoningBlocks[itemID]; ok {
		return index
	}
	index := t.nextBlockIndex
	t.nextBlockIndex++
	t.reasoningBlocks[itemID] = index
	t.writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		},
	})
	return index
}

func (t *sseTranslator) startRedactedThinkingBlock(itemID, data string) int {
	if index, ok := t.reasoningBlocks[itemID]; ok {
		return index
	}
	index := t.nextBlockIndex
	t.nextBlockIndex++
	t.reasoningBlocks[itemID] = index
	t.writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "redacted_thinking",
			"data": data,
		},
	})
	return index
}

func (t *sseTranslator) closeOpenBlocks() {
	for key, index := range t.textBlocks {
		if !t.closedTextBlocks[key] {
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
			t.closedTextBlocks[key] = true
		}
	}
	for key, index := range t.toolBlocks {
		if !t.closedToolBlocks[key] {
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
			t.closedToolBlocks[key] = true
		}
	}
	for key, index := range t.reasoningBlocks {
		if !t.closedReasoning[key] {
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
			t.closedReasoning[key] = true
		}
	}
}

func (t *sseTranslator) writeAnthropicStreamError(message string) {
	t.writeEvent("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": message,
		},
	})
}

func (t *sseTranslator) emitResponse(resp AnthropicMessageResponse) {
	t.ensureMessageStart()

	for index, block := range resp.Content {
		switch block.Type {
		case "text":
			t.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			if strings.TrimSpace(block.Text) != "" {
				t.writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type": "text_delta",
						"text": block.Text,
					},
				})
			}
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		case "tool_use":
			t.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    block.ID,
					"name":  block.Name,
					"input": map[string]any{},
				},
			})
			if payload := marshalToolUseInput(block.Input); payload != "" {
				t.writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": payload,
					},
				})
			}
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		case "thinking":
			t.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":      "thinking",
					"thinking":  "",
					"signature": "",
				},
			})
			if strings.TrimSpace(block.Thinking) != "" {
				t.writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": block.Thinking,
					},
				})
			}
			if strings.TrimSpace(block.Signature) != "" {
				t.writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type":      "signature_delta",
						"signature": block.Signature,
					},
				})
			}
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		case "redacted_thinking":
			t.writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "redacted_thinking",
					"data": block.Data,
				},
			})
			t.writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		}
	}

	t.writeEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   resp.StopReason,
			"stop_sequence": resp.StopSequence,
		},
		"usage": resp.Usage,
	})
	t.writeEvent("message_stop", map[string]any{
		"type": "message_stop",
	})
}

func marshalToolUseInput(input any) string {
	if input == nil {
		return ""
	}
	payload, err := json.Marshal(input)
	if err != nil || string(payload) == "null" {
		return ""
	}
	return string(payload)
}

func (t *sseTranslator) writeEvent(name string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(t.w, "event: %s\n", name)
	fmt.Fprintf(t.w, "data: %s\n\n", data)
	t.flusher.Flush()
}

func (t *sseTranslator) debugfTool(format string, args ...any) {
	if t.debugf == nil {
		return
	}
	t.debugf(format, args...)
}

func (t *sseTranslator) remainingToolArguments(itemID, full string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		return ""
	}

	current := t.toolArguments[itemID]
	if current == "" {
		t.toolArguments[itemID] = full
		return full
	}

	if strings.HasPrefix(full, current) {
		remainder := full[len(current):]
		t.toolArguments[itemID] = full
		return remainder
	}

	if full == current {
		return ""
	}

	t.debugfTool("tool argument mismatch item_id=%q current=%s full=%s", itemID, sanitizeLogValue(current), sanitizeLogValue(full))
	return ""
}

type streamAccumulator struct {
	response    OpenAIResponsesResponse
	outputOrder []string
	outputs     map[string]*OpenAIOutputItem
	textParts   map[string]string
	argParts    map[string]string
	reasoning   map[string]string
}

func aggregateBackendStream(body io.Reader) (OpenAIResponsesResponse, error) {
	acc := streamAccumulator{
		outputs:   map[string]*OpenAIOutputItem{},
		textParts: map[string]string{},
		argParts:  map[string]string{},
		reasoning: map[string]string{},
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var eventName string
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		return acc.handle(eventName, payload)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return OpenAIResponsesResponse{}, err
			}
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return OpenAIResponsesResponse{}, err
	}
	if err := dispatch(); err != nil {
		return OpenAIResponsesResponse{}, err
	}
	return acc.finish(), nil
}

func (a *streamAccumulator) handle(eventName, payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}

	var event OpenAIStreamEnvelope
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.Type == "" {
		event.Type = eventName
	}

	switch event.Type {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		if event.Response != nil {
			a.mergeResponse(*event.Response)
		}
	case "response.output_item.added":
		if event.Item != nil {
			a.upsertOutput(event.Item.ID, eventItemToOutput(*event.Item))
		}
	case "response.output_item.done":
		if event.Item != nil {
			a.upsertOutput(event.Item.ID, eventItemToOutput(*event.Item))
		}
	case "response.output_text.delta":
		key := textBlockKey(event.ItemID, event.ContentIndex)
		a.textParts[key] += event.Delta
		item := a.ensureMessageOutput(event.ItemID)
		item.Content = setOutputTextContent(item.Content, event.ContentIndex, a.textParts[key], "output_text")
	case "response.output_text.done":
		text := fallback(event.Text, event.Delta)
		key := textBlockKey(event.ItemID, event.ContentIndex)
		if text == "" {
			text = a.textParts[key]
		}
		item := a.ensureMessageOutput(event.ItemID)
		item.Content = setOutputTextContent(item.Content, event.ContentIndex, text, "output_text")
	case "response.content_part.added":
		if event.Part != nil && strings.TrimSpace(event.Part.Text) != "" {
			switch event.Part.Type {
			case "output_text":
				key := textBlockKey(event.ItemID, event.ContentIndex)
				if a.textParts[key] == "" {
					a.textParts[key] = event.Part.Text
				}
				item := a.ensureMessageOutput(event.ItemID)
				item.Content = setOutputTextContent(item.Content, event.ContentIndex, a.textParts[key], "output_text")
			case "reasoning_text", "summary_text":
				if a.reasoning[event.ItemID] == "" {
					a.reasoning[event.ItemID] = event.Part.Text
				}
				item := a.ensureReasoningOutput(event.ItemID)
				item.Content = setOutputTextContent(item.Content, 0, a.reasoning[event.ItemID], event.Part.Type)
			}
		}
	case "response.content_part.done":
		if event.Part != nil {
			switch event.Part.Type {
			case "output_text":
				item := a.ensureMessageOutput(event.ItemID)
				item.Content = setOutputTextContent(item.Content, event.ContentIndex, fallback(event.Text, event.Part.Text), "output_text")
			case "reasoning_text", "summary_text":
				text := fallback(event.Text, event.Part.Text)
				if text != "" {
					item := a.ensureReasoningOutput(event.ItemID)
					item.Content = setOutputTextContent(item.Content, 0, text, event.Part.Type)
				}
			}
		}
	case "response.function_call_arguments.delta":
		a.argParts[event.ItemID] += event.Delta
		item := a.ensureFunctionOutput(event.ItemID)
		item.Arguments = a.argParts[event.ItemID]
	case "response.function_call_arguments.done":
		args := fallback(event.Arguments, event.Delta)
		if args == "" {
			args = a.argParts[event.ItemID]
		}
		item := a.ensureFunctionOutput(event.ItemID)
		item.Arguments = args
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		a.reasoning[event.ItemID] += event.Delta
		item := a.ensureReasoningOutput(event.ItemID)
		item.Content = setOutputTextContent(item.Content, 0, a.reasoning[event.ItemID], "reasoning_text")
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		text := fallback(event.Text, a.reasoning[event.ItemID])
		if text != "" {
			item := a.ensureReasoningOutput(event.ItemID)
			item.Content = setOutputTextContent(item.Content, 0, text, "reasoning_text")
		}
	case "error", "response.error":
		if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
			return errors.New(event.Error.Message)
		}
		if event.Response != nil {
			a.mergeResponse(*event.Response)
			if event.Response.Error != nil && strings.TrimSpace(event.Response.Error.Message) != "" {
				return errors.New(event.Response.Error.Message)
			}
		}
	}

	return nil
}

func (a *streamAccumulator) mergeResponse(resp OpenAIResponsesResponse) {
	if resp.ID != "" {
		a.response.ID = resp.ID
	}
	if resp.Model != "" {
		a.response.Model = resp.Model
	}
	if resp.Status != "" {
		a.response.Status = resp.Status
	}
	if resp.IncompleteDetails != nil {
		a.response.IncompleteDetails = resp.IncompleteDetails
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 0 {
		a.response.Usage = resp.Usage
	}
	if len(resp.Output) > 0 {
		for _, output := range resp.Output {
			a.upsertOutput(fallback(output.ID, output.CallID), output)
		}
	}
}

func (a *streamAccumulator) upsertOutput(id string, output OpenAIOutputItem) *OpenAIOutputItem {
	id = fallback(id, fallback(output.ID, output.CallID))
	if id == "" {
		id = fmt.Sprintf("output_%d", len(a.outputOrder))
	}
	if output.ID == "" {
		output.ID = id
	}
	existing, ok := a.outputs[id]
	if !ok {
		a.outputOrder = append(a.outputOrder, id)
		a.outputs[id] = &output
		return a.outputs[id]
	}
	mergeOutputItem(existing, output)
	return existing
}

func (a *streamAccumulator) ensureMessageOutput(id string) *OpenAIOutputItem {
	return a.upsertOutput(id, OpenAIOutputItem{
		ID:   id,
		Type: "message",
		Role: "assistant",
	})
}

func (a *streamAccumulator) ensureFunctionOutput(id string) *OpenAIOutputItem {
	return a.upsertOutput(id, OpenAIOutputItem{
		ID:   id,
		Type: "function_call",
	})
}

func (a *streamAccumulator) ensureReasoningOutput(id string) *OpenAIOutputItem {
	return a.upsertOutput(id, OpenAIOutputItem{
		ID:   id,
		Type: "reasoning",
	})
}

func (a *streamAccumulator) finish() OpenAIResponsesResponse {
	if len(a.response.Output) == 0 {
		for _, id := range a.outputOrder {
			if item := a.outputs[id]; item != nil {
				a.response.Output = append(a.response.Output, *item)
			}
		}
	}
	return a.response
}

func eventItemToOutput(item OpenAIEventItem) OpenAIOutputItem {
	return OpenAIOutputItem{
		ID:               item.ID,
		Type:             item.Type,
		Role:             item.Role,
		Phase:            item.Phase,
		Name:             item.Name,
		CallID:           item.CallID,
		Arguments:        item.Arguments,
		Content:          item.Content,
		EncryptedContent: item.EncryptedContent,
		Summary:          item.Summary,
		Status:           item.Status,
	}
}

func mergeOutputItem(dst *OpenAIOutputItem, src OpenAIOutputItem) {
	if src.ID != "" {
		dst.ID = src.ID
	}
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.Role != "" {
		dst.Role = src.Role
	}
	if src.Phase != "" {
		dst.Phase = src.Phase
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.CallID != "" {
		dst.CallID = src.CallID
	}
	if src.Arguments != "" {
		dst.Arguments = src.Arguments
	}
	if len(src.Content) > 0 {
		dst.Content = src.Content
	}
	if src.EncryptedContent != "" {
		dst.EncryptedContent = src.EncryptedContent
	}
	if len(src.Summary) > 0 {
		dst.Summary = src.Summary
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
}

func setOutputTextContent(content []OpenAIOutputContent, index int, text string, contentType string) []OpenAIOutputContent {
	for len(content) <= index {
		content = append(content, OpenAIOutputContent{Type: fallback(contentType, "output_text")})
	}
	content[index] = OpenAIOutputContent{
		Type: fallback(contentType, "output_text"),
		Text: text,
	}
	return content
}

func (p *Proxy) forwardBackendError(w http.ResponseWriter, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "backend error")
		return
	}
	p.debugf("backend error status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))

	var backendErr OpenAIResponsesResponse
	if err := json.Unmarshal(body, &backendErr); err == nil && backendErr.Error != nil {
		writeAnthropicError(w, resp.StatusCode, fallback(backendErr.Error.Type, "api_error"), backendErr.Error.Message)
		return
	}

	var simpleErr OpenAIError
	if err := json.Unmarshal(body, &simpleErr); err == nil && simpleErr.Message != "" {
		writeAnthropicError(w, resp.StatusCode, fallback(simpleErr.Type, "api_error"), simpleErr.Message)
		return
	}

	writeAnthropicError(w, resp.StatusCode, "api_error", strings.TrimSpace(string(body)))
}

func estimateInputTokens(system any, messages []AnthropicMessage, tools []AnthropicTool) int {
	var totalUnits float64

	addString := func(value string) {
		totalUnits += estimateStringTokenUnits(value)
	}

	if blocks, err := normalizeSystemBlocks(system); err == nil {
		for _, block := range blocks {
			addString(block.Text)
		}
	}

	for _, message := range messages {
		blocks, err := normalizeContentBlocks(message.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			switch block.Type {
			case "", "text":
				addString(block.Text)
			case "tool_use":
				addString(block.Name)
				addString(string(block.Input))
			case "tool_result":
				addString(block.ToolUseID)
				addString(flattenToolResult(block.Content))
			case "image", "document", "file":
				if block.Source != nil {
					addString(block.Source.MediaType)
					addString(block.Source.URL)
					addString(block.Source.FileID)
					addString(block.Source.Data)
				}
				addString(block.Title)
				addString(block.Context)
			case "thinking":
				addString(block.Thinking)
			case "redacted_thinking":
				addString(block.Data)
			}
		}
	}

	for _, tool := range tools {
		addString(tool.Name)
		addString(tool.Description)
		if blob, err := json.Marshal(tool.InputSchema); err == nil {
			addString(string(blob))
		}
	}

	if totalUnits == 0 {
		return 0
	}

	return int(math.Ceil(totalUnits))
}

func estimateStringTokenUnits(value string) float64 {
	if value == "" {
		return 0
	}
	var asciiLetters int
	var digits int
	var whitespace int
	var punctuation int
	var nonASCII int
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			whitespace++
		case r > unicode.MaxASCII:
			nonASCII++
		case unicode.IsLetter(r):
			asciiLetters++
		case unicode.IsDigit(r):
			digits++
		default:
			punctuation++
		}
	}
	units := float64(asciiLetters+digits)/4.0 + float64(punctuation)/3.0 + float64(whitespace)/8.0 + float64(nonASCII)
	if units == 0 {
		return 0
	}
	return units
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSONWithStatus(w, status, AnthropicErrorEnvelope{
		Type: "error",
		Error: AnthropicAPIError{
			Type:    errorType,
			Message: message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	writeJSONWithStatus(w, status, payload)
}

func writeJSONWithStatus(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func textBlockKey(itemID string, contentIndex int) string {
	return itemID + ":" + strconv.Itoa(contentIndex)
}

func (p *Proxy) nextID(prefix string) string {
	seq := atomic.AddUint64(&p.idCounter, 1)
	return fmt.Sprintf("%s_proxy_%d_%d", prefix, time.Now().UnixNano(), seq)
}

func (p *Proxy) debugf(format string, args ...any) {
	if !p.cfg.Debug {
		return
	}
	log.Printf("[claude-codex-proxy] "+format, args...)
}

func sanitizeLogValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 500 {
		return value
	}
	return value[:500] + "...(truncated)"
}

func inferModelVendor(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(lower, "gpt"), strings.Contains(lower, "codex"), strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"):
		return "openai"
	case strings.HasPrefix(lower, "claude"):
		return "anthropic"
	default:
		return "proxy"
	}
}

func inferModelFamily(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "codex"):
		return "codex"
	case strings.HasPrefix(lower, "gpt-5"):
		return "gpt-5"
	case strings.HasPrefix(lower, "gpt-4"):
		return "gpt-4"
	case strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"):
		return "o-series"
	case strings.HasPrefix(lower, "claude"):
		return "claude"
	default:
		return "general"
	}
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	}
	return ""
}

func asOptionalBool(value any) *bool {
	switch typed := value.(type) {
	case bool:
		v := typed
		return &v
	default:
		return nil
	}
}

func asPositiveInt(value any) int {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed > 0 {
			return int(parsed)
		}
	}
	return 0
}
