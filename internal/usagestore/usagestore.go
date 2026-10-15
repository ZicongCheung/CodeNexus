// Package usagestore persists per-request usage rows in SQLite so the console
// can show trends and history across restarts.
//
// The red line from ADR-49 holds here too: a row carries the narrow fields a
// dashboard needs — time, wire, account label, model, status, tokens, credits.
// Prompts, answers and credentials are never written; a label identifies an
// account without exposing its token.
//
// Daily totals are not stored. They are aggregated from the rows at query time,
// so a row that is corrected after the fact (a credit that settles late) is
// simply correct on the next query, with no bookkeeping to drift.
package usagestore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one persisted request row.
type Record struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Wire             string    `json:"wire"` // chat | anthropic | responses
	Source           string    `json:"source,omitempty"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
	Status           int       `json:"status"`
	Outcome          string    `json:"outcome"`
	Attempts         int       `json:"attempts,omitempty"`
	DurationMS       int64     `json:"duration_ms"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credits          float64   `json:"credits"`
	CreditsSource    string    `json:"credits_source,omitempty"`
}

// Filter narrows a record query. Empty string fields match everything.
type Filter struct {
	Wire    string
	Source  string
	Model   string
	Outcome string
	From    time.Time
	To      time.Time
	Limit   int
}

// ModelUsage is one model's share of a window.
type ModelUsage struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Credits  float64 `json:"credits"`
}

// DailyRow is one day's totals, with the models that contributed to it.
type DailyRow struct {
	Date     string       `json:"date"`
	Requests int64        `json:"requests"`
	OK       int64        `json:"ok"`
	Failed   int64        `json:"failed"`
	Input    int64        `json:"input_tokens"`
	Output   int64        `json:"output_tokens"`
	Total    int64        `json:"total_tokens"`
	Credits  float64      `json:"credits"`
	Models   []ModelUsage `json:"models"`
}

// Summary is a window's totals.
type Summary struct {
	Requests int64   `json:"requests"`
	OK       int64   `json:"ok"`
	Failed   int64   `json:"failed"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Credits  float64 `json:"credits"`
	Models   int64   `json:"models"`
	First    int64   `json:"first_ts"`
	Last     int64   `json:"last_ts"`
}

// Store is a SQLite-backed usage history.
type Store struct {
	mu  *sql.DB
	max int
}

// Open creates (or opens) the database at path and makes sure the schema is
// current. maxRecords bounds how many rows are kept; the oldest are pruned.
func Open(path string, maxRecords int) (*Store, error) {
	if path == "" {
		return nil, errors.New("usagestore: empty database path")
	}
	if maxRecords < 100 {
		maxRecords = 100
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("usagestore: open %s: %w", path, err)
	}
	// A single connection keeps SQLite's locking simple; every statement is
	// short and the store is not on the request's critical path.
	db.SetMaxOpenConns(1)
	s := &Store{mu: db, max: maxRecords}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS usage_records (
	request_id        TEXT PRIMARY KEY,
	ts                INTEGER NOT NULL,
	wire              TEXT NOT NULL DEFAULT '',
	source            TEXT NOT NULL DEFAULT '',
	account           TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL DEFAULT '',
	stream            INTEGER NOT NULL DEFAULT 0,
	status            INTEGER NOT NULL DEFAULT 0,
	outcome           TEXT NOT NULL DEFAULT '',
	attempts          INTEGER NOT NULL DEFAULT 0,
	duration_ms       INTEGER NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	credits           REAL NOT NULL DEFAULT 0,
	credits_source    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_records(ts);
CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage_records(model, ts);
`

func (s *Store) migrate() error {
	if _, err := s.mu.Exec(schema); err != nil {
		return fmt.Errorf("usagestore: migrate: %w", err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.mu.Close() }

// Insert stores one row, replacing any earlier row with the same request id, and
// prunes past the configured bound.
func (s *Store) Insert(r Record) error {
	if s == nil || s.mu == nil {
		return nil
	}
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := s.mu.Exec(`INSERT INTO usage_records
		(request_id, ts, wire, source, account, model, stream, status, outcome,
		 attempts, duration_ms, prompt_tokens, completion_tokens, total_tokens,
		 credits, credits_source)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_id) DO UPDATE SET
			ts=excluded.ts, wire=excluded.wire, source=excluded.source,
			account=excluded.account, model=excluded.model, stream=excluded.stream,
			status=excluded.status, outcome=excluded.outcome, attempts=excluded.attempts,
			duration_ms=excluded.duration_ms, prompt_tokens=excluded.prompt_tokens,
			completion_tokens=excluded.completion_tokens, total_tokens=excluded.total_tokens,
			credits=excluded.credits, credits_source=excluded.credits_source`,
		r.RequestID, ts.Unix(), r.Wire, r.Source, r.Account, r.Model, boolInt(r.Stream),
		r.Status, r.Outcome, r.Attempts, r.DurationMS, r.PromptTokens,
		r.CompletionTokens, r.TotalTokens, r.Credits, r.CreditsSource,
	)
	if err != nil {
		return fmt.Errorf("usagestore: insert: %w", err)
	}
	return s.prune()
}

// SetCredits corrects a row's charge after the fact. Settlement runs out of
// band, so the row the client already saw may learn its charge later.
func (s *Store) SetCredits(requestID string, credits float64, source string) error {
	if s == nil || s.mu == nil || requestID == "" {
		return nil
	}
	_, err := s.mu.Exec(`UPDATE usage_records SET credits=?, credits_source=? WHERE request_id=?`,
		credits, source, requestID)
	if err != nil {
		return fmt.Errorf("usagestore: set credits: %w", err)
	}
	return nil
}

// prune keeps at most max rows, dropping the oldest.
func (s *Store) prune() error {
	var n int64
	if err := s.mu.QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&n); err != nil {
		return err
	}
	if n <= int64(s.max) {
		return nil
	}
	_, err := s.mu.Exec(`DELETE FROM usage_records WHERE request_id IN (
		SELECT request_id FROM usage_records ORDER BY ts ASC LIMIT ?)`, n-int64(s.max))
	if err != nil {
		return fmt.Errorf("usagestore: prune: %w", err)
	}
	return nil
}

// Records returns the newest matching rows.
func (s *Store) Records(f Filter) ([]Record, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	where, args := recordWhere(f)
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.mu.Query(`SELECT request_id, ts, wire, source, account, model, stream,
		status, outcome, attempts, duration_ms, prompt_tokens, completion_tokens,
		total_tokens, credits, credits_source
		FROM usage_records `+where+` ORDER BY ts DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: records: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var ts int64
		var stream int
		if err := rows.Scan(&r.RequestID, &ts, &r.Wire, &r.Source, &r.Account, &r.Model,
			&stream, &r.Status, &r.Outcome, &r.Attempts, &r.DurationMS,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.Credits, &r.CreditsSource); err != nil {
			return nil, err
		}
		r.Time = time.Unix(ts, 0)
		r.Stream = stream != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func recordWhere(f Filter) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if f.Wire != "" {
		add("wire = ?", f.Wire)
	}
	if f.Source != "" {
		add("source = ?", f.Source)
	}
	if f.Model != "" {
		add("model = ?", f.Model)
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if !f.From.IsZero() {
		add("ts >= ?", f.From.Unix())
	}
	if !f.To.IsZero() {
		add("ts <= ?", f.To.Unix())
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// Daily aggregates the window into per-day totals. dayOffsetSeconds shifts the
// day boundary to the operator's timezone, so a request at 23:00 UTC counts as
// the local day it belongs to.
func (s *Store) Daily(days int, dayOffsetSeconds int, tz *time.Location) ([]DailyRow, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 30
	}
	if tz == nil {
		tz = time.Local
	}
	from := time.Now().In(tz).AddDate(0, 0, -days+1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, tz)

	type bucket struct {
		row    DailyRow
		models map[string]*ModelUsage
	}
	buckets := map[int]*bucket{}

	rows, err := s.mu.Query(`SELECT ts, wire, source, model, status, outcome,
		prompt_tokens, completion_tokens, total_tokens, credits
		FROM usage_records WHERE ts >= ? ORDER BY ts ASC`, start.Unix())
	if err != nil {
		return nil, fmt.Errorf("usagestore: daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var wire, source, model, outcome string
		var status int
		var in, out, total int64
		var credits float64
		if err := rows.Scan(&ts, &wire, &source, &model, &status, &outcome, &in, &out, &total, &credits); err != nil {
			return nil, err
		}
		local := time.Unix(ts, 0).In(tz)
		index := int(local.Sub(start) / (24 * time.Hour))
		if index < 0 {
			index = 0
		}
		b := buckets[index]
		if b == nil {
			b = &bucket{models: map[string]*ModelUsage{}}
			b.row.Date = local.Format("2006-01-02")
			buckets[index] = b
		}
		b.row.Requests++
		if outcome == "success" || status < 400 {
			b.row.OK++
		} else {
			b.row.Failed++
		}
		b.row.Input += in
		b.row.Output += out
		b.row.Total += total
		b.row.Credits += credits
		m := b.models[model]
		if m == nil {
			m = &ModelUsage{Model: model}
			b.models[model] = m
		}
		m.Requests++
		m.Input += in
		m.Output += out
		m.Total += total
		m.Credits += credits
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DailyRow, 0, days)
	for i := 0; i < days; i++ {
		b := buckets[i]
		if b == nil {
			out = append(out, DailyRow{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Models: []ModelUsage{}})
			continue
		}
		b.row.Credits = round2(b.row.Credits)
		b.row.Models = sortModels(b.models)
		out = append(out, b.row)
	}
	return out, nil
}

// Summary totals a window.
func (s *Store) Summary(days int, tz *time.Location) (Summary, error) {
	var out Summary
	if s == nil || s.mu == nil {
		return out, nil
	}
	if days <= 0 {
		days = 30
	}
	if tz == nil {
		tz = time.Local
	}
	from := time.Now().In(tz).AddDate(0, 0, -days+1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, tz)
	err := s.mu.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN outcome='success' OR status<400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(credits),0),
			COUNT(DISTINCT CASE WHEN model<>'' THEN model END),
			COALESCE(MIN(ts),0), COALESCE(MAX(ts),0)
		FROM usage_records WHERE ts >= ?`, start.Unix()).Scan(
		&out.Requests, &out.OK, &out.Input, &out.Output, &out.Total, &out.Credits,
		&out.Models, &out.First, &out.Last)
	if err != nil {
		return out, fmt.Errorf("usagestore: summary: %w", err)
	}
	out.Failed = out.Requests - out.OK
	out.Credits = round2(out.Credits)
	return out, nil
}

// Models lists the models seen in a window, heaviest first.
func (s *Store) Models(days int, tz *time.Location) ([]ModelUsage, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 30
	}
	if tz == nil {
		tz = time.Local
	}
	from := time.Now().In(tz).AddDate(0, 0, -days+1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, tz)
	rows, err := s.mu.Query(`SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(credits),0)
		FROM usage_records WHERE ts >= ? AND model<>'' GROUP BY model
		ORDER BY SUM(total_tokens) DESC LIMIT 50`, start.Unix())
	if err != nil {
		return nil, fmt.Errorf("usagestore: models: %w", err)
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.Input, &m.Output, &m.Total, &m.Credits); err != nil {
			return nil, err
		}
		m.Credits = round2(m.Credits)
		out = append(out, m)
	}
	return out, rows.Err()
}

func sortModels(m map[string]*ModelUsage) []ModelUsage {
	out := make([]ModelUsage, 0, len(m))
	for _, v := range m {
		v.Credits = round2(v.Credits)
		out = append(out, *v)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Total > out[j-1].Total; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
