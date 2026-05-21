package claudecodexproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckRateLimitRejectModeSetsRetryAfter(t *testing.T) {
	base := time.Unix(1000, 0)
	p := New(Config{
		RateLimitInterval: 2 * time.Second,
		RateLimitWait:     false,
	})
	p.now = func() time.Time { return base.Add(500 * time.Millisecond) }
	p.lastRequestTime = base
	defer p.Close()

	rec := httptest.NewRecorder()
	if ok := p.checkRateLimit(context.Background(), rec); ok {
		t.Fatalf("checkRateLimit() = true, want false")
	}
	if got := rec.Code; got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if !strings.Contains(rec.Body.String(), "rate limit") {
		t.Fatalf("response body = %q, want rate limit message", rec.Body.String())
	}
}

func TestCheckRateLimitWaitModeReservesNextSlotBeforeWaiting(t *testing.T) {
	base := time.Unix(2000, 0)
	p := New(Config{
		RateLimitInterval: 50 * time.Millisecond,
		RateLimitWait:     true,
	})
	p.now = func() time.Time { return base }
	defer p.Close()

	if ok := p.checkRateLimit(context.Background(), httptest.NewRecorder()); !ok {
		t.Fatalf("first checkRateLimit() = false, want true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- p.checkRateLimit(ctx, httptest.NewRecorder())
	}()
	time.Sleep(5 * time.Millisecond)

	p.rateMu.Lock()
	reserved := p.lastRequestTime
	p.rateMu.Unlock()
	if want := base.Add(50 * time.Millisecond); !reserved.Equal(want) {
		t.Fatalf("reserved lastRequestTime = %v, want %v", reserved, want)
	}

	if ok := <-done; ok {
		t.Fatalf("second checkRateLimit() = true, want false due to context timeout")
	}
}
