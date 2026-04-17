package claudecodexproxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultListenAddr            = "127.0.0.1:8787"
	defaultBackendPath           = "/v1/responses"
	defaultRequestTimeout        = 120 * time.Second
	defaultCapabilityReprobeTTL  = 30 * time.Minute
	defaultAnthropicBase         = "https://api.anthropic.com"
	defaultClaudeTokenMultiplier = 1.15
)

type Config struct {
	ListenAddr                    string
	BackendBaseURL                string
	BackendPath                   string
	BackendAPIKey                 string
	ClientAPIKey                  string
	BackendModel                  string
	BackendWarmupModel            string
	AnthropicModelAlias           string
	RequestTimeout                time.Duration
	BackendReasoningEffort        string
	AnthropicAPIBaseURL           string
	AnthropicAPIKey               string
	ClaudeTokenMultiplier         float64
	CapabilityReprobeTTL          time.Duration
	EnableBackendMetadata         bool
	AnonymousMode                 bool
	ForwardUserMetadata           *bool
	UserMetadataAllowlist         []string
	DisableContinuityMetadata     bool
	DisableUserMetadataForwarding bool
	DisablePromptCacheKey         bool
	EnableModelCapabilityInit     bool
	EnablePhaseCommentary         bool
	DisableStreamingBackend       bool
	Debug                         bool
}

func LoadConfigFromEnv() (Config, error) {
	discovered := discoverFallbackConfig()
	cfg := Config{
		ListenAddr:          getEnv("CLAUDE_CODE_PROXY_LISTEN_ADDR", defaultListenAddr),
		BackendBaseURL:      firstNonEmpty(normalizeBackendBaseURL(os.Getenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL")), discovered.BackendBaseURL),
		BackendPath:         normalizeBackendPath(getEnv("CLAUDE_CODE_PROXY_BACKEND_PATH", defaultBackendPath)),
		BackendAPIKey:       firstNonEmpty(strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY")), discovered.BackendAPIKey),
		ClientAPIKey:        strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_CLIENT_API_KEY")),
		BackendModel:        firstNonEmpty(strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_BACKEND_MODEL")), discovered.BackendModel),
		BackendWarmupModel:  strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_WARMUP_MODEL")),
		RequestTimeout:      defaultRequestTimeout,
		AnthropicAPIBaseURL: normalizeExternalBaseURL(getEnv("CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL", defaultAnthropicBase)),
		AnthropicAPIKey: strings.TrimSpace(firstNonEmpty(
			os.Getenv("CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY"),
			os.Getenv("ANTHROPIC_API_KEY"),
		)),
		ClaudeTokenMultiplier: defaultClaudeTokenMultiplier,
		CapabilityReprobeTTL:  defaultCapabilityReprobeTTL,
	}

	if cfg.BackendBaseURL == "" {
		return Config{}, fmt.Errorf("missing CLAUDE_CODE_PROXY_BACKEND_BASE_URL")
	}
	if cfg.BackendAPIKey == "" {
		return Config{}, fmt.Errorf("missing CLAUDE_CODE_PROXY_BACKEND_API_KEY")
	}

	if timeoutRaw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_REQUEST_TIMEOUT")); timeoutRaw != "" {
		timeout, err := time.ParseDuration(timeoutRaw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = timeout
	}

	cfg.AnthropicModelAlias = strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS"))
	cfg.BackendReasoningEffort = strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT"))

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA: %w", err)
		}
		cfg.EnableBackendMetadata = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_ANONYMOUS_MODE")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_ANONYMOUS_MODE: %w", err)
		}
		cfg.AnonymousMode = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_FORWARD_USER_METADATA: %w", err)
		}
		cfg.ForwardUserMetadata = &parsed
	}

	if cfg.ForwardUserMetadata == nil {
		if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING")); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING: %w", err)
			}
			cfg.DisableUserMetadataForwarding = parsed
		}
	}

	cfg.UserMetadataAllowlist = parseCSVEnvAllowlist(os.Getenv("CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST"))

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA: %w", err)
		}
		cfg.DisableContinuityMetadata = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY: %w", err)
		}
		cfg.DisablePromptCacheKey = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT: %w", err)
		}
		cfg.EnableModelCapabilityInit = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY: %w", err)
		}
		cfg.EnablePhaseCommentary = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING: %w", err)
		}
		cfg.DisableStreamingBackend = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_DEBUG")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_DEBUG: %w", err)
		}
		cfg.Debug = parsed
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER: %w", err)
		}
		if parsed > 0 {
			cfg.ClaudeTokenMultiplier = parsed
		}
	}

	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL: %w", err)
		}
		cfg.CapabilityReprobeTTL = parsed
	}
	if !isLoopbackListenAddr(cfg.ListenAddr) && cfg.ClientAPIKey == "" {
		return Config{}, fmt.Errorf("missing CLAUDE_CODE_PROXY_CLIENT_API_KEY for non-loopback CLAUDE_CODE_PROXY_LISTEN_ADDR")
	}

	return cfg, nil
}

func (c Config) BackendURL() string {
	return c.BackendBaseURL + c.BackendPath
}

func (c Config) BackendModelsURL() string {
	return c.BackendBaseURL + "/v1/models"
}

func (c Config) AdvertisedModel(requestModel string) string {
	if c.AnthropicModelAlias != "" {
		return c.AnthropicModelAlias
	}
	if requestModel != "" {
		return requestModel
	}
	if c.BackendModel != "" {
		return c.BackendModel
	}
	return "claude-code-proxy"
}

func (c Config) EffectiveBackendModel(requestModel string) string {
	if c.BackendModel != "" {
		return c.BackendModel
	}
	return requestModel
}

func (c Config) EffectiveForwardUserMetadata() bool {
	if c.AnonymousMode {
		return false
	}
	if c.ForwardUserMetadata != nil {
		return *c.ForwardUserMetadata
	}
	return !c.DisableUserMetadataForwarding
}

func parseCSVEnvAllowlist(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := map[string]struct{}{}
	allowlist := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		key := strings.TrimSpace(item)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		allowlist = append(allowlist, key)
	}
	if len(allowlist) == 0 {
		return nil
	}
	return allowlist
}

func getEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func normalizeBackendPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return defaultBackendPath
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

type fallbackConfig struct {
	BackendBaseURL string
	BackendAPIKey  string
	BackendModel   string
}

type codexConfigFile struct {
	ModelProvider  string                         `toml:"model_provider"`
	Model          string                         `toml:"model"`
	ModelProviders map[string]codexProviderConfig `toml:"model_providers"`
}

type codexProviderConfig struct {
	BaseURL string `toml:"base_url"`
	WireAPI string `toml:"wire_api"`
}

type codexAuthFile struct {
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
}

type claudeSettingsFile struct {
	Env map[string]string `json:"env"`
}

func discoverFallbackConfig() fallbackConfig {
	codex := discoverCodexFallbackConfig()
	claude := discoverClaudeFallbackConfig()

	return fallbackConfig{
		BackendBaseURL: firstNonEmpty(codex.BackendBaseURL, claude.BackendBaseURL),
		BackendAPIKey:  firstNonEmpty(codex.BackendAPIKey, claude.BackendAPIKey),
		BackendModel:   firstNonEmpty(codex.BackendModel, claude.BackendModel),
	}
}

func discoverCodexFallbackConfig() fallbackConfig {
	root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return fallbackConfig{}
		}
		root = filepath.Join(home, ".codex")
	}

	configPath := filepath.Join(root, "config.toml")
	authPath := filepath.Join(root, "auth.json")

	var discovered fallbackConfig

	var cfg codexConfigFile
	if _, err := toml.DecodeFile(configPath, &cfg); err == nil {
		provider := strings.TrimSpace(cfg.ModelProvider)
		if provider == "" {
			provider = "codex"
		}
		if providerCfg, ok := cfg.ModelProviders[provider]; ok {
			if isResponsesWireAPI(providerCfg.WireAPI) {
				discovered.BackendBaseURL = normalizeBackendBaseURL(providerCfg.BaseURL)
				discovered.BackendModel = strings.TrimSpace(cfg.Model)
			}
		}
		if discovered.BackendBaseURL == "" {
			if providerCfg, ok := cfg.ModelProviders["codex"]; ok {
				if isResponsesWireAPI(providerCfg.WireAPI) {
					discovered.BackendBaseURL = normalizeBackendBaseURL(providerCfg.BaseURL)
					discovered.BackendModel = strings.TrimSpace(cfg.Model)
				}
			}
		}
	}

	var auth codexAuthFile
	if err := readJSONFile(authPath, &auth); err == nil {
		discovered.BackendAPIKey = strings.TrimSpace(auth.OpenAIAPIKey)
	}

	return discovered
}

func discoverClaudeFallbackConfig() fallbackConfig {
	var discovered fallbackConfig

	for _, path := range candidateClaudeSettingsPaths() {
		var settings claudeSettingsFile
		if err := readJSONFile(path, &settings); err != nil {
			continue
		}
		baseURL := normalizeBackendBaseURL(settings.Env["ANTHROPIC_BASE_URL"])
		if isLoopbackBaseURL(baseURL) {
			continue
		}
		discovered.BackendBaseURL = firstNonEmpty(discovered.BackendBaseURL, baseURL)
		discovered.BackendAPIKey = firstNonEmpty(discovered.BackendAPIKey, strings.TrimSpace(settings.Env["ANTHROPIC_AUTH_TOKEN"]))
		discovered.BackendModel = firstNonEmpty(discovered.BackendModel, strings.TrimSpace(settings.Env["ANTHROPIC_MODEL"]))
	}

	return discovered
}

func candidateClaudeSettingsPaths() []string {
	var paths []string
	seen := map[string]struct{}{}
	addPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		addPath(filepath.Join(home, ".claude", "settings.local.json"))
		addPath(filepath.Join(home, ".claude", "settings.json"))
	}

	return paths
}

func readJSONFile(path string, v any) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, v)
}

func normalizeBackendBaseURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return ""
	}

	for _, suffix := range []string{
		"/v1/responses",
		"/v1/messages/count_tokens",
		"/v1/messages",
		"/messages/count_tokens",
		"/messages",
		"/anthropic",
		"/v1",
	} {
		base = trimSuffixInsensitive(base, suffix)
	}

	return strings.TrimRight(base, "/")
}

func normalizeExternalBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func trimSuffixInsensitive(value, suffix string) string {
	lowerValue := strings.ToLower(value)
	lowerSuffix := strings.ToLower(suffix)
	if !strings.HasSuffix(lowerValue, lowerSuffix) {
		return value
	}
	return value[:len(value)-len(suffix)]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isResponsesWireAPI(raw string) bool {
	wireAPI := strings.ToLower(strings.TrimSpace(raw))
	return wireAPI == "" || wireAPI == "responses"
}

func isLoopbackBaseURL(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func isLoopbackListenAddr(raw string) bool {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return false
	}

	host := addr
	if parsedHost, _, err := net.SplitHostPort(addr); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}

	switch strings.ToLower(host) {
	case "localhost":
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
