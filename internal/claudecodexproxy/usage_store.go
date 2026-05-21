package claudecodexproxy

import "time"

type usageEvent struct {
	CreatedAtMs  int64
	Endpoint     string
	Model        string
	InputTokens  int
	OutputTokens int
}

type modelUsage struct {
	Model        string `json:"model"`
	Events       int    `json:"events"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type usageSummary struct {
	Period      string       `json:"period"`
	GeneratedAt time.Time    `json:"generated_at"`
	TotalEvents int          `json:"total_events"`
	TotalInput  int          `json:"total_input_tokens"`
	TotalOutput int          `json:"total_output_tokens"`
	ByModel     []modelUsage `json:"by_model"`
}

type usageStore interface {
	RecordUsage(event usageEvent) error
	QuerySummary(period string) (usageSummary, error)
	Close() error
}

type noopUsageStore struct{}

func (*noopUsageStore) RecordUsage(_ usageEvent) error           { return nil }
func (*noopUsageStore) QuerySummary(_ string) (usageSummary, error) { return usageSummary{}, nil }
func (*noopUsageStore) Close() error                              { return nil }

func (p *Proxy) recordUsage(model, endpoint string, usage AnthropicUsage) {
	if p.usageStore == nil {
		return
	}
	event := usageEvent{
		CreatedAtMs:  p.now().UnixMilli(),
		Endpoint:     endpoint,
		Model:        model,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}
	go func() {
		if err := p.usageStore.RecordUsage(event); err != nil {
			p.debugf("usage record failed: %v", err)
		}
	}()
}
