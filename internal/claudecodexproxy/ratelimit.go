package claudecodexproxy

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func (p *Proxy) checkRateLimit(ctx context.Context, w http.ResponseWriter) bool {
	if p.cfg.RateLimitInterval <= 0 {
		return true
	}

	p.rateMu.Lock()
	defer p.rateMu.Unlock()

	now := p.now()
	elapsed := now.Sub(p.lastRequestTime)
	if elapsed >= p.cfg.RateLimitInterval {
		p.lastRequestTime = now
		return true
	}

	waitNeeded := p.cfg.RateLimitInterval - elapsed

	if !p.cfg.RateLimitWait {
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("rate limit: please wait %.1fs before the next request", waitNeeded.Seconds()))
		return false
	}

	p.rateMu.Unlock()
	select {
	case <-ctx.Done():
		p.rateMu.Lock()
		return false
	case <-time.After(waitNeeded):
	}
	p.rateMu.Lock()
	p.lastRequestTime = p.now()
	return true
}
