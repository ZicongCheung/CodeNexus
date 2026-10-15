package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/config"
)

// newConfigConsole builds a console wired to a real config file in a temp dir,
// with a hot-apply hook that records what it was asked to apply.
func newConfigConsole(t *testing.T) (*Console, *config.Config, string, *[]string) {
	t.Helper()
	c, _, _ := newConsole(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.AuthDir = dir
	cfg.Listen = "127.0.0.1:0"
	path := filepath.Join(dir, "config.json")
	if err := config.Save(path, &cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	c.cfg = &cfg
	c.cfgPath = path
	var applied []string
	c.applyHot = func(next *config.Config, diff []string) ([]string, []string) {
		applied = append(applied, next.LogLevel)
		// Only report what actually changed, the way the real hook does.
		out := []string{}
		for _, p := range diff {
			if p == "log_level" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	return c, &cfg, path, &applied
}

func postConfig(c *Console, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec
}

// currentConfig is the config as the API sees it, used to build an edit.
func currentConfig(t *testing.T, c *Console) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d", rec.Code)
	}
	var body struct {
		Schema []map[string]any `json:"schema"`
		Config map[string]any   `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Schema) == 0 {
		t.Fatal("the schema is what makes the form possible; it must not be empty")
	}
	return body.Config
}

func TestConfigGetServesSchemaAndValues(t *testing.T) {
	c, _, _, _ := newConfigConsole(t)
	values := currentConfig(t, c)
	if values["log_level"] == nil {
		t.Fatalf("config missing log_level: %v", values)
	}
}

// A typo in a field name must not be silently dropped: the user would think the
// setting was saved when nothing changed.
func TestConfigSaveRejectsUnknownField(t *testing.T) {
	c, _, _, _ := newConfigConsole(t)
	values := currentConfig(t, c)
	values["log_levl"] = "debug"
	rec := postConfig(c, mustJSON(t, map[string]any{"config": values}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestConfigSaveRejectsValueOutsideEnum(t *testing.T) {
	c, _, _, _ := newConfigConsole(t)
	values := currentConfig(t, c)
	values["log_level"] = "loud"
	rec := postConfig(c, mustJSON(t, map[string]any{"config": values}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for a value outside the enum", rec.Code)
	}
}

func TestConfigSavesAndTakesEffect(t *testing.T) {
	c, _, path, applied := newConfigConsole(t)
	values := currentConfig(t, c)
	values["log_level"] = "debug"
	rec := postConfig(c, mustJSON(t, map[string]any{"config": values}))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Applied   []string `json:"applied"`
		Restart   []string `json:"restart_required"`
		Snapshots []struct {
			ID string `json:"id"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !contains(body.Applied, "log_level") {
		t.Fatalf("applied = %v, want log_level", body.Applied)
	}
	if len(body.Restart) != 0 {
		t.Fatalf("restart = %v, want none", body.Restart)
	}
	// The file is what a restart reads, so it has to carry the change too.
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if onDisk.LogLevel != "debug" {
		t.Fatalf("on-disk log_level = %q", onDisk.LogLevel)
	}
	// The hook is what makes the change live rather than pending.
	if len(*applied) != 1 || (*applied)[0] != "debug" {
		t.Fatalf("hot apply did not run: %v", *applied)
	}
	if len(body.Snapshots) == 0 {
		t.Fatal("a save must leave something to roll back to")
	}
}

// A field that can lock the operator out asks before it writes, so one stray
// click cannot take the gateway down.
func TestConfigSaveAsksBeforeDangerousFields(t *testing.T) {
	c, cfg, _, _ := newConfigConsole(t)
	values := currentConfig(t, c)
	values["listen"] = "127.0.0.1:9"

	rec := postConfig(c, mustJSON(t, map[string]any{"config": values}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 asking for confirmation", rec.Code)
	}
	if cfg.Listen == "127.0.0.1:9" {
		t.Fatal("a dangerous edit must not be applied before it is confirmed")
	}

	rec = postConfig(c, mustJSON(t, map[string]any{"config": values, "confirm": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed save = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Restart []string `json:"restart_required"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !contains(body.Restart, "listen") {
		t.Fatalf("restart = %v, want listen: it cannot be applied in place", body.Restart)
	}
}

// The rollback is the way back when an edit leaves the gateway unreachable, so
// it has to restore both the file and the running config.
func TestConfigRollbackRestoresPreviousValues(t *testing.T) {
	c, _, path, _ := newConfigConsole(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	values := currentConfig(t, c)
	values["log_level"] = "debug"
	if rec := postConfig(c, mustJSON(t, map[string]any{"config": values})); rec.Code != http.StatusOK {
		t.Fatalf("save = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config/snapshots", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	var snaps struct {
		Snapshots []struct {
			ID string `json:"id"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snaps); err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	if len(snaps.Snapshots) == 0 {
		t.Fatal("no snapshot was taken")
	}

	rollback := httptest.NewRequest(http.MethodPost, "/api/config/rollback",
		strings.NewReader(mustJSON(t, map[string]any{"id": snaps.Snapshots[0].ID})))
	rollback.RemoteAddr = "127.0.0.1:5555"
	rollback.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	c.ServeHTTP(rec, rollback)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback = %d body=%s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("rollback did not restore the file")
	}
	if c.cfg.LogLevel == "debug" {
		t.Fatalf("running config still %q after rollback", c.cfg.LogLevel)
	}
}

func TestConfigSaveWithNoChangesAppliesNothing(t *testing.T) {
	c, _, _, _ := newConfigConsole(t)
	values := currentConfig(t, c)
	rec := postConfig(c, mustJSON(t, map[string]any{"config": values}))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var body struct {
		Changed []string `json:"changed"`
		Applied []string `json:"applied"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Changed) != 0 || len(body.Applied) != 0 {
		t.Fatalf("changed=%v applied=%v, want nothing", body.Changed, body.Applied)
	}
}

func TestRestartRequiresSupport(t *testing.T) {
	c, _, _, _ := newConfigConsole(t)
	req := httptest.NewRequest(http.MethodPost, "/api/restart", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501 when the process cannot restart itself", rec.Code)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
