package usagestore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T, maxRecords int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), maxRecords)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertQueryAndCorrectCredits(t *testing.T) {
	s := openTemp(t, 100)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	rec := Record{
		Time: base, RequestID: "req-1", Wire: "chat", Source: "workbuddy",
		Account: "acct-a", Model: "glm-5.2", Stream: true, Status: 200,
		Outcome: "success", Attempts: 1, DurationMS: 1200,
		PromptTokens: 100, CompletionTokens: 40, TotalTokens: 140,
	}
	if err := s.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := s.Records(Filter{})
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "req-1" || !rows[0].Stream {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Credits != 0 {
		t.Fatalf("credits = %v, want the unsettled zero", rows[0].Credits)
	}

	// Settlement lands later and corrects the row in place.
	if err := s.SetCredits("req-1", 1.25, "session_usage"); err != nil {
		t.Fatalf("set credits: %v", err)
	}
	rows, _ = s.Records(Filter{Model: "glm-5.2"})
	if rows[0].Credits != 1.25 || rows[0].CreditsSource != "session_usage" {
		t.Fatalf("corrected row = %+v", rows[0])
	}
}

func TestFiltersNarrowTheQuery(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now()
	for i, r := range []Record{
		{RequestID: "a", Time: now.Add(-3 * time.Hour), Wire: "chat", Model: "m1", Outcome: "success", Status: 200},
		{RequestID: "b", Time: now.Add(-2 * time.Hour), Wire: "anthropic", Model: "m2", Outcome: "http_error", Status: 500},
		{RequestID: "c", Time: now.Add(-1 * time.Hour), Wire: "chat", Model: "m1", Outcome: "success", Status: 200},
	} {
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, _ := s.Records(Filter{Wire: "chat"})
	if len(rows) != 2 {
		t.Fatalf("wire filter = %d rows", len(rows))
	}
	rows, _ = s.Records(Filter{Outcome: "http_error"})
	if len(rows) != 1 || rows[0].RequestID != "b" {
		t.Fatalf("outcome filter = %+v", rows)
	}
	rows, _ = s.Records(Filter{From: now.Add(-90 * time.Minute)})
	if len(rows) != 1 || rows[0].RequestID != "c" {
		t.Fatalf("time filter = %+v", rows)
	}
	rows, _ = s.Records(Filter{Limit: 2})
	if len(rows) != 2 || rows[0].RequestID != "c" {
		t.Fatalf("limit/order = %+v", rows)
	}
}

func TestDailyAggregationFollowsTheCorrectedRow(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now()
	if err := s.Insert(Record{
		Time: now, RequestID: "a", Wire: "chat", Model: "m1",
		Outcome: "success", Status: 200, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The credit settles after the row was written.
	if err := s.SetCredits("a", 0.5, "upstream"); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	daily, err := s.Daily(Days(7, nil), 8*3600, nil)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	today := daily[len(daily)-1]
	if today.Requests != 1 || today.Total != 15 || today.Credits != 0.5 {
		t.Fatalf("today = %+v", today)
	}
	if len(today.Models) != 1 || today.Models[0].Model != "m1" || today.Models[0].Total != 15 {
		t.Fatalf("models = %+v", today.Models)
	}
	// The boundary must not leak into a neighbouring day.
	for _, row := range daily[:len(daily)-1] {
		if row.Requests != 0 {
			t.Fatalf("stale bucket: %+v", row)
		}
	}

	summary, err := s.Summary(Days(7, nil))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Requests != 1 || summary.OK != 1 || summary.Failed != 0 || summary.Credits != 0.5 || summary.Models != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	models, err := s.Models(Days(7, nil))
	if err != nil || len(models) != 1 || models[0].Model != "m1" {
		t.Fatalf("models = %+v, %v", models, err)
	}
}

func TestPruneDropsTheOldest(t *testing.T) {
	s := openTemp(t, 105)
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 108; i++ {
		id := "req-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := s.Insert(Record{
			Time: base.Add(time.Duration(i) * time.Minute), RequestID: id,
			Wire: "chat", Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, _ := s.Records(Filter{Limit: 500})
	if len(rows) != 105 {
		t.Fatalf("rows = %d, want the cap", len(rows))
	}
	// The oldest three must be gone.
	for _, r := range rows {
		if r.RequestID == "req-aa" || r.RequestID == "req-ab" || r.RequestID == "req-ac" {
			t.Fatalf("oldest row survived: %+v", r)
		}
	}
	if rows[0].RequestID != "req-ed" {
		t.Fatalf("newest = %q, want req-ed", rows[0].RequestID)
	}
}

func TestNilStoreIsANoOp(t *testing.T) {
	var s *Store
	if err := s.Insert(Record{}); err != nil {
		t.Fatalf("insert on nil: %v", err)
	}
	if err := s.SetCredits("x", 1, "upstream"); err != nil {
		t.Fatalf("set credits on nil: %v", err)
	}
	if rows, err := s.Records(Filter{}); err != nil || rows != nil {
		t.Fatalf("records on nil = %v, %v", rows, err)
	}
	if summary, err := s.Summary(Days(1, nil)); err != nil || summary.Requests != 0 {
		t.Fatalf("summary on nil = %+v, %v", summary, err)
	}
}

// cached_tokens is a later addition to a table that already exists in the
// field. The migration has to add the column to an old store rather than only
// declaring it for new ones, or every upgraded install silently reports zero
// cache hits forever.
func TestCachedColumnIsAddedToAnExistingStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")

	// A store shaped like one created before cache accounting existed.
	old, err := Open(path, 100)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := old.mu.Exec(`DROP TABLE usage_records`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := old.mu.Exec(`CREATE TABLE usage_records (
		request_id TEXT PRIMARY KEY, ts INTEGER NOT NULL, wire TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '', account TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '', stream INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0, outcome TEXT NOT NULL DEFAULT '',
		attempts INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0, credits REAL NOT NULL DEFAULT 0,
		credits_source TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	old.Close()

	reopened, err := Open(path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if err := reopened.Insert(Record{
		RequestID: "r1", Time: time.Now(), Wire: "chat",
		PromptTokens: 1000, TotalTokens: 1200, CachedTokens: 800,
	}); err != nil {
		t.Fatalf("insert after migrate: %v", err)
	}
	rows, err := reopened.Records(Filter{})
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(rows) != 1 || rows[0].CachedTokens != 800 {
		t.Fatalf("cached tokens = %+v, want 800", rows)
	}
}

// The detail table appends older pages as the user scrolls, so the cursor must
// be exclusive: passing back the oldest timestamp already shown has to return
// the next older rows and never the row that timestamp belongs to.
func TestBeforeCursorPagesBackwardsWithoutRepeating(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now().Truncate(time.Second)
	for i, id := range []string{"newest", "middle", "oldest"} {
		if err := s.Insert(Record{
			RequestID: id, Time: now.Add(-time.Duration(i) * time.Hour),
			Wire: "chat", Status: 200, Outcome: "success",
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	first, err := s.Records(Filter{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 2 || first[0].RequestID != "newest" || first[1].RequestID != "middle" {
		t.Fatalf("first page = %+v", first)
	}

	second, err := s.Records(Filter{Limit: 2, Before: first[len(first)-1].Time})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 1 || second[0].RequestID != "oldest" {
		t.Fatalf("second page = %+v, want only the oldest row", second)
	}
}

// The three breakdowns are separate queries over the same rows; if one of them
// counted differently the console would show two answers to one question.
func TestBreakdownsAgreeOnTheSameWindow(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now()
	for i, r := range []Record{
		{RequestID: "1", Time: now, Wire: "chat", Source: "workbuddy", Account: "a", Model: "m1",
			Status: 200, Outcome: "success", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		{RequestID: "2", Time: now, Wire: "anthropic", Source: "trae-cn", Account: "b", Model: "m1",
			Status: 200, Outcome: "success", PromptTokens: 200, CompletionTokens: 50, TotalTokens: 250},
		{RequestID: "3", Time: now, Wire: "chat", Source: "workbuddy", Account: "a", Model: "m2",
			Status: 500, Outcome: "http_error", PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10},
	} {
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	accounts, err := s.Accounts(Days(1, time.UTC))
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	wires, err := s.Wires(Days(1, time.UTC))
	if err != nil {
		t.Fatalf("wires: %v", err)
	}
	models, err := s.Models(Days(1, time.UTC))
	if err != nil {
		t.Fatalf("models: %v", err)
	}

	sum := func(ns []int64) int64 {
		var total int64
		for _, n := range ns {
			total += n
		}
		return total
	}
	accountTotals := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		accountTotals = append(accountTotals, a.Total)
	}
	wireTotals := make([]int64, 0, len(wires))
	for _, w := range wires {
		wireTotals = append(wireTotals, w.Total)
	}
	modelTotals := make([]int64, 0, len(models))
	for _, m := range models {
		modelTotals = append(modelTotals, m.Total)
	}

	const want int64 = 410
	if got := sum(accountTotals); got != want {
		t.Fatalf("accounts total = %d, want %d", got, want)
	}
	if got := sum(wireTotals); got != want {
		t.Fatalf("wires total = %d, want %d", got, want)
	}
	if got := sum(modelTotals); got != want {
		t.Fatalf("models total = %d, want %d", got, want)
	}
}

// A custom range is half-open: the lower bound counts, the upper bound does
// not. Picking 1 Aug then 2 Aug must not count the same second in both.
func TestCustomWindowIsHalfOpen(t *testing.T) {
	s := openTemp(t, 100)
	tz := time.UTC
	// Local midnight of three consecutive days, so "days" in Daily() lines up.
	day := func(offset int) time.Time {
		n := time.Now().In(tz).AddDate(0, 0, -offset)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, tz)
	}
	rows := []Record{
		{RequestID: "outside-low", Time: day(3).Add(time.Hour), TotalTokens: 1},
		{RequestID: "inside-1", Time: day(2), TotalTokens: 10},
		{RequestID: "inside-2", Time: day(2).Add(23 * time.Hour), TotalTokens: 20},
		{RequestID: "at-top", Time: day(1), TotalTokens: 30},
	}
	for _, r := range rows {
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	win := Window{From: day(2), To: day(1)}
	sum, err := s.Summary(win)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 2 || sum.Total != 30 {
		t.Fatalf("summary = %d requests / %d tokens, want 2/30", sum.Requests, sum.Total)
	}
	// The chart covers exactly the one day the half-open range names: the top
	// bound belongs to the next bucket, so it is not drawn here.
	daily, err := s.Daily(win, 0, tz)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(daily) != 1 || daily[0].Date != win.From.Format("2006-01-02") {
		t.Fatalf("buckets = %+v, want one bucket for %s", daily, win.From.Format("2006-01-02"))
	}
}

// A range with no rows still draws its days. An empty chart would read as "the
// query failed"; zero-height bars read as "nothing happened", which is true.
func TestEmptyRangeStillDrawsItsDays(t *testing.T) {
	s := openTemp(t, 100)
	tz := time.UTC
	day := func(offset int) time.Time {
		n := time.Now().In(tz).AddDate(0, 0, -offset)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, tz)
	}
	daily, err := s.Daily(Window{From: day(3), To: day(0)}, 0, tz)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(daily) != 3 {
		t.Fatalf("buckets = %d, want 3 empty days", len(daily))
	}
	for _, row := range daily {
		if row.Requests != 0 {
			t.Fatalf("empty range reported %+v", row)
		}
	}
}

// "Everything" is a zero window. It must reach back past the rolling preset
// that would otherwise cap the console at 366 days.
func TestZeroWindowCoversTheWholeStore(t *testing.T) {
	s := openTemp(t, 100)
	old := time.Now().AddDate(-2, 0, 0)
	if err := s.Insert(Record{RequestID: "ancient", Time: old, TotalTokens: 7}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sum, err := s.Summary(Window{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 1 || sum.Total != 7 {
		t.Fatalf("summary = %+v, want the two-year-old row", sum)
	}
}

// The comparison window must line up with the same clock time yesterday, not
// with "the same number of hours before now". At 09:00 a one-day preset covers
// 00:00-09:00, so its predecessor is yesterday 00:00-09:00; sliding the window
// back by nine hours would silently compare against yesterday evening.
func TestPrevKeepsTheClockOnCalendarPresets(t *testing.T) {
	tz := time.FixedZone("CST", 8*3600)

	// The clock is derived from the window rather than written into the test.
	// A hardcoded "now" expired the moment the machine passed midnight: the
	// preset then opened a day later than the test assumed, the span went
	// negative, and Prev returned an empty window — a test that failed once a
	// day for a reason that had nothing to do with the code.
	day := Days(1, tz)
	now := day.From.Add(9*time.Hour + 30*time.Minute)

	prev := day.Prev(now)
	wantFrom := day.From.AddDate(0, 0, -1)
	wantTo := now.AddDate(0, 0, -1)
	if !prev.From.Equal(wantFrom) || !prev.To.Equal(wantTo) {
		t.Fatalf("prev = %v..%v, want %v..%v", prev.From, prev.To, wantFrom, wantTo)
	}

	week := Days(7, tz)
	prevWeek := week.Prev(now)
	if got, want := prevWeek.From, week.From.AddDate(0, 0, -7); !got.Equal(want) {
		t.Fatalf("week prev from = %v, want %v", got, want)
	}
	if span, prevSpan := now.Sub(week.From), prevWeek.To.Sub(prevWeek.From); span != prevSpan {
		t.Fatalf("spans differ: %v vs %v", span, prevSpan)
	}
}

// "All time" has no predecessor. Returning an empty window keeps the caller
// from comparing the whole store against itself and reporting +0%.
func TestPrevOfAllTimeIsEmpty(t *testing.T) {
	if got := (Window{}).Prev(time.Now()); !got.From.IsZero() || !got.To.IsZero() {
		t.Fatalf("prev of all time = %v..%v, want zero", got.From, got.To)
	}
}

// The console's page-level source filter rides on the window, so every
// aggregate of one selection covers the same rows. A summary that ignored the
// filter would contradict the chart drawn under it.
func TestSourceFilterNarrowsEveryAggregate(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now().Truncate(time.Second)
	rows := []Record{
		{RequestID: "wb-1", Time: now, Source: "workbuddy", Account: "a", Model: "m1", Wire: "chat",
			Status: 200, Outcome: "success", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		{RequestID: "tr-1", Time: now, Source: "trae-cn", Account: "b", Model: "m2", Wire: "anthropic",
			Status: 200, Outcome: "success", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
	for _, r := range rows {
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	win := Window{From: now.Add(-time.Hour), Source: "workbuddy"}
	sum, err := s.Summary(win)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 1 || sum.Total != 15 {
		t.Fatalf("summary = %d requests / %d tokens, want 1/15", sum.Requests, sum.Total)
	}
	models, err := s.Models(win)
	if err != nil || len(models) != 1 || models[0].Model != "m1" {
		t.Fatalf("models = %+v (%v), want only m1", models, err)
	}
	accounts, err := s.Accounts(win)
	if err != nil || len(accounts) != 1 || accounts[0].Account != "a" {
		t.Fatalf("accounts = %+v (%v), want only a", accounts, err)
	}
	wires, err := s.Wires(win)
	if err != nil || len(wires) != 1 || wires[0].Wire != "chat" {
		t.Fatalf("wires = %+v (%v), want only chat", wires, err)
	}
	daily, err := s.Daily(win, 0, time.Local)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	total := int64(0)
	for _, d := range daily {
		total += d.Total
	}
	if total != 15 {
		t.Fatalf("daily total = %d, want 15", total)
	}

	// The comparison window carries the same filter, or a filtered present
	// would be measured against an unfiltered past.
	if prev := win.Prev(now.Add(time.Hour)); prev.Source != "workbuddy" {
		t.Fatalf("prev source = %q, want workbuddy", prev.Source)
	}
}

// SQLite's AVG hands back a float. Scanning that into an int64 column only
// survives while the average happens to land on a whole number — one 850ms
// beside one 900ms and the accounts breakdown errors out instead of rendering.
// The cast lives in SQL so the Go type stays a plain int64.
func TestAverageDurationOfMixedRowsStillScans(t *testing.T) {
	s := openTemp(t, 100)
	now := time.Now().Truncate(time.Second)
	for i, ms := range []int64{850, 900} {
		if err := s.Insert(Record{
			RequestID: fmt.Sprintf("m-%d", i), Time: now, Source: "workbuddy",
			Account: "a", Model: "m", Wire: "chat", Status: 200, Outcome: "success",
			DurationMS: ms, TotalTokens: 1,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	accounts, err := s.Accounts(Window{From: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].AvgMS != 875 {
		t.Fatalf("accounts = %+v (%v), want one row at 875ms", accounts, err)
	}
	wires, err := s.Wires(Window{From: now.Add(-time.Hour)})
	if err != nil || len(wires) != 1 || wires[0].AvgMS != 875 {
		t.Fatalf("wires = %+v (%v), want one row at 875ms", wires, err)
	}
}
