//go:build sqlite

package claudecodexproxy

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteUsageStore struct {
	db *sql.DB
}

func newUsageStore(path string) (usageStore, error) {
	if path == "" {
		return &noopUsageStore{}, nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS token_usage_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at_ms INTEGER NOT NULL,
		endpoint TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_events_created_at_model
		ON token_usage_events(created_at_ms, model)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create index: %w", err)
	}
	return &sqliteUsageStore{db: db}, nil
}

func (s *sqliteUsageStore) RecordUsage(event usageEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO token_usage_events (created_at_ms, endpoint, model, input_tokens, output_tokens)
		 VALUES (?, ?, ?, ?, ?)`,
		event.CreatedAtMs, event.Endpoint, event.Model, event.InputTokens, event.OutputTokens,
	)
	return err
}

func (s *sqliteUsageStore) QuerySummary(period string) (usageSummary, error) {
	now := time.Now()
	var since time.Time
	switch period {
	case "week":
		since = now.AddDate(0, 0, -7)
	case "month":
		since = now.AddDate(0, -1, 0)
	default:
		since = now.AddDate(0, 0, -1)
		period = "day"
	}
	sinceMs := since.UnixMilli()

	rows, err := s.db.Query(
		`SELECT model, COUNT(*), SUM(input_tokens), SUM(output_tokens)
		 FROM token_usage_events WHERE created_at_ms >= ?
		 GROUP BY model ORDER BY model`,
		sinceMs,
	)
	if err != nil {
		return usageSummary{}, err
	}
	defer rows.Close()

	summary := usageSummary{
		Period:      period,
		GeneratedAt: now,
	}
	for rows.Next() {
		var mu modelUsage
		if err := rows.Scan(&mu.Model, &mu.Events, &mu.InputTokens, &mu.OutputTokens); err != nil {
			return usageSummary{}, err
		}
		summary.ByModel = append(summary.ByModel, mu)
		summary.TotalEvents += mu.Events
		summary.TotalInput += mu.InputTokens
		summary.TotalOutput += mu.OutputTokens
	}
	if err := rows.Err(); err != nil {
		return usageSummary{}, err
	}
	sort.Slice(summary.ByModel, func(i, j int) bool {
		return summary.ByModel[i].Model < summary.ByModel[j].Model
	})
	return summary, nil
}

func (s *sqliteUsageStore) Close() error {
	return s.db.Close()
}
