package usagestore

import (
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

	daily, err := s.Daily(7, 8*3600, nil)
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

	summary, err := s.Summary(7, nil)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Requests != 1 || summary.OK != 1 || summary.Failed != 0 || summary.Credits != 0.5 || summary.Models != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	models, err := s.Models(7, nil)
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
	if summary, err := s.Summary(1, nil); err != nil || summary.Requests != 0 {
		t.Fatalf("summary on nil = %+v, %v", summary, err)
	}
}
