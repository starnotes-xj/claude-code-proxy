package claudecodexproxy

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"
)

func (p *Proxy) checkRateLimit(ctx context.Context, w http.ResponseWriter) bool {
	if p.cfg.RateLimitInterval <= 0 {
		return true
	}

	p.rateMu.Lock()

	now := p.now()
	if p.lastRequestTime.IsZero() {
		p.lastRequestTime = now
		p.rateMu.Unlock()
		return true
	}

	elapsed := now.Sub(p.lastRequestTime)
	if elapsed >= p.cfg.RateLimitInterval {
		p.lastRequestTime = now
		p.rateMu.Unlock()
		return true
	}

	waitNeeded := p.cfg.RateLimitInterval - elapsed

	if !p.cfg.RateLimitWait {
		retryAfterSec := int(math.Ceil(waitNeeded.Seconds()))
		if retryAfterSec < 1 {
			retryAfterSec = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("rate limit: please wait %.1fs before the next request", waitNeeded.Seconds()))
		p.rateMu.Unlock()
		return false
	}

	nextAllowed := p.lastRequestTime.Add(p.cfg.RateLimitInterval)
	p.lastRequestTime = nextAllowed
	p.rateMu.Unlock()

	timer := time.NewTimer(waitNeeded)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
