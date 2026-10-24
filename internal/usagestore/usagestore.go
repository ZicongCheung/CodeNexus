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
	"math"
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
	// CachedTokens is the share of the prompt the upstream served from cache.
	// It is billed below fresh input, so a cost stack is incomplete without it:
	// two turns with identical token counts can cost very different amounts.
	CachedTokens  int64   `json:"cached_tokens,omitempty"`
	Credits       float64 `json:"credits"`
	CreditsSource string  `json:"credits_source,omitempty"`
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
	// Before is an exclusive cursor: only records strictly older than this
	// timestamp are returned. It lets the console append older pages to a list
	// it already holds instead of re-fetching from the top, which is what a
	// scroll-to-load detail table needs. Page numbers would be meaningless
	// here, because the list is ordered by time and shifts as new turns land.
	Before time.Time
}

// ModelUsage is one model's share of a window.
type ModelUsage struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Cached   int64   `json:"cached_tokens"`
	Credits  float64 `json:"credits"`
}

// AccountUsage is one account's share of a window. It answers "which
// credential is spending this", which a model- or day-shaped view cannot.
type AccountUsage struct {
	Account  string  `json:"account"`
	Source   string  `json:"source,omitempty"`
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Cached   int64   `json:"cached_tokens"`
	Credits  float64 `json:"credits"`
	AvgMS    int64   `json:"avg_duration_ms"`
}

// WireUsage is one protocol's share of a window. The gateway speaks three
// wires, and their costs differ, so "which wire is expensive" is a question
// the console has to be able to answer.
type WireUsage struct {
	Wire     string  `json:"wire"`
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Cached   int64   `json:"cached_tokens"`
	Credits  float64 `json:"credits"`
	AvgMS    int64   `json:"avg_duration_ms"`
}

// DailyRow is one day's totals, with the models that contributed to it.
type DailyRow struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	OK       int64  `json:"ok"`
	Failed   int64  `json:"failed"`
	Input    int64  `json:"input_tokens"`
	Output   int64  `json:"output_tokens"`
	Total    int64  `json:"total_tokens"`
	// Cached counts the prompt tokens served from the upstream's cache. It is
	// reported alongside the total rather than folded into it, so the stacked
	// chart can show the billed-below-fresh share as its own band.
	Cached  int64        `json:"cached_tokens"`
	Credits float64      `json:"credits"`
	Models  []ModelUsage `json:"models"`
}

// Summary is a window's totals.
type Summary struct {
	Requests int64   `json:"requests"`
	OK       int64   `json:"ok"`
	Failed   int64   `json:"failed"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	Total    int64   `json:"total_tokens"`
	Cached   int64   `json:"cached_tokens"`
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
	cached_tokens     INTEGER NOT NULL DEFAULT 0,
	credits           REAL NOT NULL DEFAULT 0,
	credits_source    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_records(ts);
CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage_records(model, ts);
CREATE INDEX IF NOT EXISTS idx_usage_account_ts ON usage_records(account, ts);
CREATE INDEX IF NOT EXISTS idx_usage_wire_ts ON usage_records(wire, ts);
`

// cachedColumn is added separately because CREATE TABLE IF NOT EXISTS leaves
// an existing table untouched: a store created before cache accounting was
// recorded has every other column and would silently never gain this one.
const cachedColumn = `ALTER TABLE usage_records ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`

func (s *Store) migrate() error {
	if _, err := s.mu.Exec(schema); err != nil {
		return fmt.Errorf("usagestore: migrate: %w", err)
	}
	// A duplicate column means the table already has it, which is the common
	// case; anything else is a real failure worth reporting.
	if _, err := s.mu.Exec(cachedColumn); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("usagestore: migrate cached_tokens: %w", err)
		}
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.mu.Close() }

// Insert stores one row, replacing any earlier row with the same request id, and
// prunes past the configured bound.
// SetMaxRecords raises or lowers the retention ceiling at runtime. Lowering it
// does not prune immediately; the next insert brings the row count down.
func (s *Store) SetMaxRecords(max int) {
	if max < 100 {
		max = 100
	}
	s.max = max
}

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
		 cached_tokens, credits, credits_source)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_id) DO UPDATE SET
			ts=excluded.ts, wire=excluded.wire, source=excluded.source,
			account=excluded.account, model=excluded.model, stream=excluded.stream,
			status=excluded.status, outcome=excluded.outcome, attempts=excluded.attempts,
			duration_ms=excluded.duration_ms, prompt_tokens=excluded.prompt_tokens,
			completion_tokens=excluded.completion_tokens, total_tokens=excluded.total_tokens,
			cached_tokens=excluded.cached_tokens,
			credits=excluded.credits, credits_source=excluded.credits_source`,
		r.RequestID, ts.Unix(), r.Wire, r.Source, r.Account, r.Model, boolInt(r.Stream),
		r.Status, r.Outcome, r.Attempts, r.DurationMS, r.PromptTokens,
		r.CompletionTokens, r.TotalTokens, r.CachedTokens, r.Credits, r.CreditsSource,
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
		total_tokens, cached_tokens, credits, credits_source
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
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens,
			&r.Credits, &r.CreditsSource); err != nil {
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
	if !f.Before.IsZero() {
		add("ts < ?", f.Before.Unix())
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// Daily aggregates the window into per-day totals. dayOffsetSeconds shifts the
// day boundary to the operator's timezone, so a request at 23:00 UTC counts as
// the local day it belongs to.
func (s *Store) Daily(w Window, dayOffsetSeconds int, tz *time.Location) ([]DailyRow, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	if tz == nil {
		tz = time.Local
	}
	where, args := w.bound()

	// The bucket grid follows the window, not a preset length: a custom range
	// must produce a chart that starts and ends where the numbers above it do.
	//
	// Only "everything" needs the data to pick a start. A closed range draws
	// every day in it, including the empty ones — "this month had no requests
	// on the 3rd" is information, and a chart that silently shrinks to the days
	// that happen to have rows is not the chart the counters above it describe.
	start := w.From
	if start.IsZero() {
		var minTS sql.NullInt64
		if err := s.mu.QueryRow(`SELECT MIN(ts) FROM usage_records`).Scan(&minTS); err != nil {
			return nil, fmt.Errorf("usagestore: daily bounds: %w", err)
		}
		if !minTS.Valid {
			return []DailyRow{}, nil
		}
		local := time.Unix(minTS.Int64, 0).In(tz)
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tz)
	}
	end := w.To
	if end.IsZero() {
		end = time.Now().In(tz)
	}
	// Count calendar days by stepping, not by dividing: a DST shift makes one
	// day 23 or 25 hours long, and integer division then drops a bucket.
	days := 0
	for d := start; d.Before(end) && days < 4000; d = d.AddDate(0, 0, 1) {
		days++
	}
	if days < 1 {
		days = 1
	}

	type bucket struct {
		row    DailyRow
		models map[string]*ModelUsage
	}
	buckets := map[int]*bucket{}

	rows, err := s.mu.Query(`SELECT ts, wire, source, model, status, outcome,
		prompt_tokens, completion_tokens, total_tokens, cached_tokens, credits
		FROM usage_records WHERE `+where+` ORDER BY ts ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var wire, source, model, outcome string
		var status int
		var in, out, total, cached int64
		var credits float64
		if err := rows.Scan(&ts, &wire, &source, &model, &status, &outcome, &in, &out, &total, &cached, &credits); err != nil {
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
		b.row.Cached += cached
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
func (s *Store) Summary(w Window) (Summary, error) {
	var out Summary
	if s == nil || s.mu == nil {
		return out, nil
	}
	where, args := w.bound()
	err := s.mu.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN outcome='success' OR status<400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(credits),0),
			COUNT(DISTINCT CASE WHEN model<>'' THEN model END),
			COALESCE(MIN(ts),0), COALESCE(MAX(ts),0)
		FROM usage_records WHERE `+where, args...).Scan(
		&out.Requests, &out.OK, &out.Input, &out.Output, &out.Total, &out.Cached, &out.Credits,
		&out.Models, &out.First, &out.Last)
	if err != nil {
		return out, fmt.Errorf("usagestore: summary: %w", err)
	}
	out.Failed = out.Requests - out.OK
	out.Credits = round2(out.Credits)
	return out, nil
}

// Models lists the models seen in a window, heaviest first.
func (s *Store) Models(w Window) ([]ModelUsage, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	where, args := w.bound()
	rows, err := s.mu.Query(`SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(cached_tokens),0), COALESCE(SUM(credits),0)
		FROM usage_records WHERE `+where+` AND model<>'' GROUP BY model
		ORDER BY SUM(total_tokens) DESC LIMIT 50`, args...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: models: %w", err)
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.Input, &m.Output, &m.Total, &m.Cached, &m.Credits); err != nil {
			return nil, err
		}
		m.Credits = round2(m.Credits)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Accounts aggregates the window per account. The console shows three shapes
// of the same window (by account, by model, by wire); they are computed on
// demand from the same rows rather than pre-rolled, so a change in one column
// can never leave the three views disagreeing.
func (s *Store) Accounts(w Window) ([]AccountUsage, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	where, args := w.bound()
	rows, err := s.mu.Query(`SELECT account, source, COUNT(*),
			COALESCE(SUM(CASE WHEN status >= 400 OR outcome<>'success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(credits),0), COALESCE(CAST(AVG(duration_ms) AS INTEGER),0)
			FROM usage_records WHERE `+where+` AND account<>'' GROUP BY account, source
			ORDER BY SUM(total_tokens) DESC LIMIT 200`, args...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: accounts: %w", err)
	}
	defer rows.Close()
	var out []AccountUsage
	for rows.Next() {
		var a AccountUsage
		if err := rows.Scan(&a.Account, &a.Source, &a.Requests, &a.Failed, &a.Input,
			&a.Output, &a.Total, &a.Cached, &a.Credits, &a.AvgMS); err != nil {
			return nil, err
		}
		a.Credits = round2(a.Credits)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Wires aggregates the window per protocol. It answers "which endpoint this
// gateway exposes is the expensive one", which neither the model nor the
// account view can show: one account can serve all three wires.
func (s *Store) Wires(w Window) ([]WireUsage, error) {
	if s == nil || s.mu == nil {
		return nil, nil
	}
	where, args := w.bound()
	rows, err := s.mu.Query(`SELECT wire, COUNT(*),
			COALESCE(SUM(CASE WHEN status >= 400 OR outcome<>'success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(credits),0), COALESCE(CAST(AVG(duration_ms) AS INTEGER),0)
			FROM usage_records WHERE `+where+` AND wire<>'' GROUP BY wire
			ORDER BY SUM(total_tokens) DESC LIMIT 20`, args...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: wires: %w", err)
	}
	defer rows.Close()
	var out []WireUsage
	for rows.Next() {
		var x WireUsage
		if err := rows.Scan(&x.Wire, &x.Requests, &x.Failed, &x.Input,
			&x.Output, &x.Total, &x.Cached, &x.Credits, &x.AvgMS); err != nil {
			return nil, err
		}
		x.Credits = round2(x.Credits)
		out = append(out, x)
	}
	return out, rows.Err()
}

// Window is the span an aggregate covers. To is zero for every rolling preset
// ("today", "last 7 days"), which means "up to now"; From is zero only for a
// caller that wants the whole store.
//
// Both ends are exclusive at the top and inclusive at the bottom. Half-open is
// what keeps a custom range from double-counting the boundary second when the
// user picks one day after another.
//
// Source narrows the aggregate to one upstream when set. The console's page
// level filter rides on the window rather than on a per-method parameter so
// every view of the same selection covers exactly the same rows — a summary
// that ignored the filter would silently contradict the chart under it.
type Window struct {
	From   time.Time
	To     time.Time
	Source string
}

// Days builds the rolling preset window: N calendar days ending today in tz,
// opened at local midnight. The aggregations share it so every view of the
// same preset covers exactly the same rows.
func Days(days int, tz *time.Location) Window {
	if days <= 0 {
		days = 30
	}
	if tz == nil {
		tz = time.Local
	}
	from := time.Now().In(tz).AddDate(0, 0, -days+1)
	return Window{From: time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, tz)}
}

// Prev is the equally long window immediately before this one, which is what a
// period-over-period comparison has to measure against.
//
// Presets open at local midnight, so they are shifted by whole calendar days
// rather than by their own elapsed length. Shifting by length would compare
// today 00:00-09:00 against yesterday 15:00-24:00 the moment the clock passes
// noon, and the number would silently mean something else every hour.
func (w Window) Prev(now time.Time) Window {
	if w.From.IsZero() {
		return Window{}
	}
	loc := w.From.Location()
	end := w.To
	if end.IsZero() {
		end = now.In(loc)
	}
	span := end.Sub(w.From)
	if span <= 0 {
		return Window{}
	}
	if w.From.Hour() == 0 && w.From.Minute() == 0 && w.From.Second() == 0 && w.From.Nanosecond() == 0 {
		days := int(math.Ceil(span.Hours() / 24))
		if days < 1 {
			days = 1
		}
		return Window{From: w.From.AddDate(0, 0, -days), To: end.AddDate(0, 0, -days), Source: w.Source}
	}
	return Window{From: w.From.Add(-span), To: end.Add(-span), Source: w.Source}
}

// bound renders the window as a WHERE fragment and its arguments.
func (w Window) bound() (string, []any) {
	args := make([]any, 0, 3)
	from := int64(0)
	if !w.From.IsZero() {
		from = w.From.Unix()
	}
	args = append(args, from)
	where := "ts >= ?"
	if !w.To.IsZero() {
		where += " AND ts < ?"
		args = append(args, w.To.Unix())
	}
	if w.Source != "" {
		where += " AND source = ?"
		args = append(args, w.Source)
	}
	return where, args
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
