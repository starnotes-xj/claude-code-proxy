package claudecodexproxy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFromEnvFallsBackToCodexConfigAndAuth(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "config.toml"), `
model_provider = "codex"
model = "gpt-5.4"

[model_providers.codex]
base_url = "https://rawchat.cn/codex/"
`)
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://should-not-win.example/anthropic","ANTHROPIC_AUTH_TOKEN":"from-claude","ANTHROPIC_MODEL":"claude-model"}}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_MODEL", "")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if cfg.BackendBaseURL != "https://rawchat.cn/codex" {
		t.Fatalf("BackendBaseURL = %q, want https://rawchat.cn/codex", cfg.BackendBaseURL)
	}
	if cfg.BackendAPIKey != "from-codex-auth" {
		t.Fatalf("BackendAPIKey = %q, want from-codex-auth", cfg.BackendAPIKey)
	}
	if cfg.BackendModel != "gpt-5.4" {
		t.Fatalf("BackendModel = %q, want gpt-5.4", cfg.BackendModel)
	}
}

func TestLoadConfigFromEnvFallsBackToClaudeSettings(t *testing.T) {
	home := t.TempDir()
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://example.com/codex/anthropic","ANTHROPIC_AUTH_TOKEN":"from-claude","ANTHROPIC_MODEL":"gpt-5.3-codex"}}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".missing-codex"))
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_MODEL", "")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if cfg.BackendBaseURL != "https://example.com/codex" {
		t.Fatalf("BackendBaseURL = %q, want https://example.com/codex", cfg.BackendBaseURL)
	}
	if cfg.BackendAPIKey != "from-claude" {
		t.Fatalf("BackendAPIKey = %q, want from-claude", cfg.BackendAPIKey)
	}
	if cfg.BackendModel != "gpt-5.3-codex" {
		t.Fatalf("BackendModel = %q, want gpt-5.3-codex", cfg.BackendModel)
	}
}

func TestLoadConfigFromEnvExplicitEnvOverridesFallbacks(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "config.toml"), `
model_provider = "codex"
model = "gpt-5.4"

[model_providers.codex]
base_url = "https://rawchat.cn/codex"
`)
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", "https://override.example/v1/responses")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY", "explicit-key")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_MODEL", "explicit-model")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if cfg.BackendBaseURL != "https://override.example" {
		t.Fatalf("BackendBaseURL = %q, want https://override.example", cfg.BackendBaseURL)
	}
	if cfg.BackendAPIKey != "explicit-key" {
		t.Fatalf("BackendAPIKey = %q, want explicit-key", cfg.BackendAPIKey)
	}
	if cfg.BackendModel != "explicit-model" {
		t.Fatalf("BackendModel = %q, want explicit-model", cfg.BackendModel)
	}
}

func TestLoadConfigFromEnvIgnoresNonResponsesCodexProvider(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "config.toml"), `
model_provider = "codex"
model = "gpt-5.4"

[model_providers.codex]
base_url = "https://chat.example.com/v1/chat/completions"
wire_api = "chat_completions"
`)
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_MODEL", "")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if cfg.BackendBaseURL != defaultBackendBase {
		t.Fatalf("BackendBaseURL = %q, want %q", cfg.BackendBaseURL, defaultBackendBase)
	}
	if cfg.BackendModel != "" {
		t.Fatalf("BackendModel = %q, want empty", cfg.BackendModel)
	}
	if cfg.BackendAPIKey != "from-codex-auth" {
		t.Fatalf("BackendAPIKey = %q, want from-codex-auth", cfg.BackendAPIKey)
	}
}

func TestLoadConfigFromEnvIgnoresLoopbackClaudeSettings(t *testing.T) {
	home := t.TempDir()
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8787","ANTHROPIC_AUTH_TOKEN":"loop-key","ANTHROPIC_MODEL":"loop-model"}}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".missing-codex"))
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_API_KEY", "")
	t.Setenv("CLAUDE_CODE_PROXY_BACKEND_MODEL", "")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want missing API key error")
	}
	if !strings.Contains(err.Error(), "missing CLAUDE_CODE_PROXY_BACKEND_API_KEY") {
		t.Fatalf("error = %q, want missing API key", err)
	}
}

func TestLoadConfigFromEnvParsesOptionalFlags(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA", "true")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "true")
	t.Setenv("CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT", "true")
	t.Setenv("CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY", "true")
	t.Setenv("CLAUDE_CODE_PROXY_DEBUG", "true")
	t.Setenv("CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL", "https://example.anthropic.test/")
	t.Setenv("CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY", "ant-key")
	t.Setenv("CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER", "1.25")
	t.Setenv("CLAUDE_CODE_PROXY_WARMUP_MODEL", "gpt-5.4-mini")
	t.Setenv("CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL", "45m")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA", "true")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY", "true")
	t.Setenv("CLAUDE_CODE_PROXY_ANONYMOUS_MODE", "true")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if !cfg.EnableBackendMetadata {
		t.Fatalf("EnableBackendMetadata = false, want true")
	}
	if !cfg.DisableUserMetadataForwarding {
		t.Fatalf("DisableUserMetadataForwarding = false, want true")
	}
	if !cfg.Debug {
		t.Fatalf("Debug = false, want true")
	}
	if !cfg.EnableModelCapabilityInit {
		t.Fatalf("EnableModelCapabilityInit = false, want true")
	}
	if !cfg.EnablePhaseCommentary {
		t.Fatalf("EnablePhaseCommentary = false, want true")
	}
	if cfg.AnthropicAPIBaseURL != "https://example.anthropic.test" {
		t.Fatalf("AnthropicAPIBaseURL = %q, want https://example.anthropic.test", cfg.AnthropicAPIBaseURL)
	}
	if cfg.AnthropicAPIKey != "ant-key" {
		t.Fatalf("AnthropicAPIKey = %q, want ant-key", cfg.AnthropicAPIKey)
	}
	if cfg.ClaudeTokenMultiplier != 1.25 {
		t.Fatalf("ClaudeTokenMultiplier = %v, want 1.25", cfg.ClaudeTokenMultiplier)
	}
	if cfg.BackendWarmupModel != "gpt-5.4-mini" {
		t.Fatalf("BackendWarmupModel = %q, want gpt-5.4-mini", cfg.BackendWarmupModel)
	}
	if cfg.CapabilityReprobeTTL != 45*time.Minute {
		t.Fatalf("CapabilityReprobeTTL = %v, want 45m", cfg.CapabilityReprobeTTL)
	}
	if !cfg.DisableContinuityMetadata {
		t.Fatalf("DisableContinuityMetadata = false, want true")
	}
	if !cfg.DisablePromptCacheKey {
		t.Fatalf("DisablePromptCacheKey = false, want true")
	}
	if !cfg.AnonymousMode {
		t.Fatalf("AnonymousMode = false, want true")
	}
}

func TestLoadConfigFromEnvParsesDisableUserMetadataForwardingFalse(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "false")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.DisableUserMetadataForwarding {
		t.Fatalf("DisableUserMetadataForwarding = true, want false")
	}
}

func TestLoadConfigFromEnvParsesForwardUserMetadataFalse(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA", "false")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.ForwardUserMetadata == nil || *cfg.ForwardUserMetadata {
		t.Fatalf("ForwardUserMetadata = %#v, want explicit false", cfg.ForwardUserMetadata)
	}
	if cfg.EffectiveForwardUserMetadata() {
		t.Fatalf("EffectiveForwardUserMetadata() = true, want false")
	}
}

func TestConfigEffectiveForwardUserMetadataAnonymousModeOverridesAll(t *testing.T) {
	t.Run("explicit forward true still becomes false", func(t *testing.T) {
		forward := true
		cfg := Config{
			AnonymousMode:       true,
			ForwardUserMetadata: &forward,
		}
		if cfg.EffectiveForwardUserMetadata() {
			t.Fatalf("EffectiveForwardUserMetadata() = true, want false")
		}
	})

	t.Run("legacy disable false still becomes false", func(t *testing.T) {
		cfg := Config{
			AnonymousMode:                 true,
			DisableUserMetadataForwarding: false,
		}
		if cfg.EffectiveForwardUserMetadata() {
			t.Fatalf("EffectiveForwardUserMetadata() = true, want false")
		}
	})
}

func TestLoadConfigFromEnvForwardUserMetadataOverridesDisableFlag(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA", "true")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "true")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.ForwardUserMetadata == nil || !*cfg.ForwardUserMetadata {
		t.Fatalf("ForwardUserMetadata = %#v, want explicit true", cfg.ForwardUserMetadata)
	}
	if !cfg.EffectiveForwardUserMetadata() {
		t.Fatalf("EffectiveForwardUserMetadata() = false, want true")
	}
}

func TestLoadConfigFromEnvForwardUserMetadataIgnoresInvalidLegacyFlag(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA", "true")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "maybe")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.ForwardUserMetadata == nil || !*cfg.ForwardUserMetadata {
		t.Fatalf("ForwardUserMetadata = %#v, want explicit true", cfg.ForwardUserMetadata)
	}
	if !cfg.EffectiveForwardUserMetadata() {
		t.Fatalf("EffectiveForwardUserMetadata() = false, want true")
	}
}

func TestLoadConfigFromEnvForwardUserMetadataFalseIgnoresInvalidLegacyFlag(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA", "false")
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "maybe")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.ForwardUserMetadata == nil || *cfg.ForwardUserMetadata {
		t.Fatalf("ForwardUserMetadata = %#v, want explicit false", cfg.ForwardUserMetadata)
	}
	if cfg.EffectiveForwardUserMetadata() {
		t.Fatalf("EffectiveForwardUserMetadata() = true, want false")
	}
}

func TestLoadConfigFromEnvRejectsInvalidDisableUserMetadataForwarding(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING") {
		t.Fatalf("error = %q, want disable user metadata forwarding env name", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidForwardUserMetadata(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_FORWARD_USER_METADATA", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_FORWARD_USER_METADATA") {
		t.Fatalf("error = %q, want forward user metadata env name", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidCapabilityReprobeTTL(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL") {
		t.Fatalf("error = %q, want capability reprobe ttl env name", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidDisableContinuityMetadata(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA") {
		t.Fatalf("error = %q, want disable continuity metadata env name", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidDisablePromptCacheKey(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY") {
		t.Fatalf("error = %q, want disable prompt cache key env name", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidAnonymousMode(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_ANONYMOUS_MODE", "maybe")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_PROXY_ANONYMOUS_MODE") {
		t.Fatalf("error = %q, want anonymous mode env name", err)
	}
}

func TestLoadConfigFromEnvParsesUserMetadataAllowlist(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")

	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"OPENAI_API_KEY":"from-codex-auth"}`)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST", "trace, tenant ,trace,,request_class")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	want := []string{"trace", "tenant", "request_class"}
	if !reflect.DeepEqual(cfg.UserMetadataAllowlist, want) {
		t.Fatalf("UserMetadataAllowlist = %#v, want %#v", cfg.UserMetadataAllowlist, want)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
