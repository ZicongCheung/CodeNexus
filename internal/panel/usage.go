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

// usageWindow resolves the window a request asks for. Three shapes reach here,
// in priority order:
//
//	?from=&to=   a custom range from the console's date pickers
//	?all=1       everything the store holds
//	?days=N      a rolling preset
//
// The custom range wins because it is the most specific thing the user can
// say. Both bounds go through parseTimeParam, so a malformed value falls back
// to the preset rather than producing an accidentally empty window.
//
// ?source= narrows to one upstream and rides on the window itself, so the
// summary, the daily chart and the three breakdowns of one selection can never
// disagree about which rows they cover.
func (c *Console) usageWindow(r *http.Request) usagestore.Window {
	q := r.URL.Query()
	from := parseTimeParam(q.Get("from"))
	to := parseTimeParam(q.Get("to"))
	src := strings.TrimSpace(q.Get("source"))
	var win usagestore.Window
	switch {
	case !from.IsZero() || !to.IsZero():
		win = usagestore.Window{From: from, To: to}
	case q.Get("all") != "":
		win = usagestore.Window{}
	default:
		days := 30
		if v := q.Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				days = n
			}
		}
		win = usagestore.Days(days, c.usageLocation())
	}
	win.Source = src
	return win
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
	st := c.usage.Load()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	win := c.usageWindow(r)
	summary, err := st.Summary(win)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// All three breakdowns are returned together. Switching a dimension is a
	// sort of data the console already holds, never a round trip: the table has
	// no loading state to flicker through when the user clicks "by wire".
	models, err := st.Models(win)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	accounts, err := st.Accounts(win)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	wires, err := st.Wires(win)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if models == nil {
		models = []usagestore.ModelUsage{}
	}
	if accounts == nil {
		accounts = []usagestore.AccountUsage{}
	}
	if wires == nil {
		wires = []usagestore.WireUsage{}
	}
	res := map[string]any{
		"available": true,
		"summary":   summary,
		"models":    models,
		"accounts":  accounts,
		"wires":     wires,
	}
	// Period over period, measured against the equally long window immediately
	// before this one. It travels with the summary so a caller that wants a
	// delta never has to issue a second round trip for it; "all time" has no
	// predecessor, so the field is simply absent there.
	if !win.From.IsZero() {
		prevWin := win.Prev(time.Now())
		prev, err := st.Summary(prevWin)
		if err != nil {
			// A failed baseline is not a failed page: the summary above is
			// complete, and the console renders "no baseline" when prev is
			// absent.
			c.warn("console: usage prev window: %v", err)
		} else {
			res["prev"] = prev
			res["prev_from"] = prevWin.From.Unix()
			if !prevWin.To.IsZero() {
				res["prev_to"] = prevWin.To.Unix()
			}
		}
	}
	// Echo the resolved bounds so the console can label the window it is
	// actually showing instead of the label it clicked.
	if !win.From.IsZero() {
		res["from"] = win.From.Unix()
	}
	if !win.To.IsZero() {
		res["to"] = win.To.Unix()
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Console) handleUsageDaily(w http.ResponseWriter, r *http.Request) {
	st := c.usage.Load()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	win := c.usageWindow(r)
	daily, err := st.Daily(win, dayOffsetSeconds(c.usageLocation()), c.usageLocation())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if daily == nil {
		daily = []usagestore.DailyRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "buckets": len(daily), "daily": daily})
}

// handleUsageRecords serves the filterable detail table.
func (c *Console) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	st := c.usage.Load()
	if st == nil {
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
		// before is the cursor the console passes back to append the next
		// older batch. Without it every "load more" would refetch the newest
		// rows it already shows.
		Before: parseTimeParam(q.Get("before")),
	}
	records, err := st.Records(filter)
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
