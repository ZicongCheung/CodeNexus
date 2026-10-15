package panel

// The usage page's data endpoints. They read the persisted history, which is a
// different layer from the in-memory request log: this one survives a restart
// and answers "what happened over the last N days", the other answers "what is
// happening right now".

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/usagestore"
)

// usageWindow resolves the ?days= parameter, bounded to a year.
func usageWindow(r *http.Request) int {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 366 {
		days = 366
	}
	return days
}

// usageLocation resolves the operator timezone, falling back to local time.
func (c *Console) usageLocation() *time.Location {
	name := strings.TrimSpace(c.cfg.Schedule.Timezone)
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}

func (c *Console) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	if c.usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	days := usageWindow(r)
	tz := c.usageLocation()
	summary, err := c.usage.Summary(days, tz)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	models, err := c.usage.Models(days, tz)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if models == nil {
		models = []usagestore.ModelUsage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"days":      days,
		"summary":   summary,
		"models":    models,
	})
}

func (c *Console) handleUsageDaily(w http.ResponseWriter, r *http.Request) {
	if c.usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	days := usageWindow(r)
	daily, err := c.usage.Daily(days, dayOffsetSeconds(c.usageLocation()), c.usageLocation())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if daily == nil {
		daily = []usagestore.DailyRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "days": days, "daily": daily})
}

// handleUsageRecords serves the filterable detail table.
func (c *Console) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	if c.usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}
	filter := usagestore.Filter{
		Wire:    q.Get("wire"),
		Source:  q.Get("source"),
		Model:   q.Get("model"),
		Outcome: q.Get("outcome"),
		From:    parseTimeParam(q.Get("from")),
		To:      parseTimeParam(q.Get("to")),
		Limit:   limit,
	}
	records, err := c.usage.Records(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if records == nil {
		records = []usagestore.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "records": records, "count": len(records)})
}

// parseTimeParam accepts unix seconds or RFC3339; an unparseable value means
// "no bound".
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	return time.Time{}
}

// dayOffsetSeconds shifts the SQL day boundary to the operator's timezone, so a
// request at 23:00 UTC counts as the local day it belongs to.
func dayOffsetSeconds(loc *time.Location) int {
	_, offset := time.Now().In(loc).Zone()
	return offset
}
