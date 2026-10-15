package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/configschema"
)

// Config paths are dotted JSON paths, so a round trip through a map is enough
// to read and compare any field without reflection.
func flatten(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	out := map[string]any{}
	walk("", m, out)
	return out
}

func walk(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			walk(path, nested, out)
			continue
		}
		out[path] = v
	}
}

// changed returns the schema paths whose value differs between two configs.
func changed(before, after map[string]any) []string {
	var out []string
	for path, v := range after {
		old, ok := before[path]
		if !ok || fmt.Sprint(old) != fmt.Sprint(v) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// validate rejects values that would leave the gateway unusable. It runs before
// anything is written, so a bad edit never reaches disk.
func validate(cfg *config.Config) error {
	if strings.TrimSpace(cfg.Listen) == "" {
		return errors.New("监听地址不能为空")
	}
	if !strings.Contains(cfg.Listen, ":") {
		return errors.New("监听地址需要形如 127.0.0.1:6828")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return errors.New("数据目录不能为空")
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		return errors.New("凭证目录不能为空")
	}
	for _, f := range configschema.Fields() {
		if len(f.Options) == 0 {
			continue
		}
		got, _ := flatten(cfg)[f.Path].(string)
		if got == "" {
			continue
		}
		ok := false
		for _, o := range f.Options {
			if o == got {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s 的取值 %q 不在允许范围内（%s）", f.Label, got, strings.Join(f.Options, " / "))
		}
	}
	return nil
}

// handleConfigGet serves the schema plus the current values. The response also
// names the fields that need a restart, so the form can say so up front instead
// of promising an effect it cannot deliver.
func (c *Console) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schema":     configschema.Fields(),
		"config":     c.cfg,
		"path":       c.cfgPath,
		"snapshots":  c.listSnapshots(),
		"canRestart": c.restart != nil,
	})
}

type configSaveRequest struct {
	Config  map[string]any `json:"config"`
	Confirm bool           `json:"confirm"`
}

// handleConfigSave writes the config, applies what can be applied, and reports
// exactly what took effect and what still needs a restart.
func (c *Console) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var req configSaveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Config == nil {
		writeErr(w, http.StatusBadRequest, "缺少 config")
		return
	}
	// Unknown fields are a typo until proven otherwise; silently dropping one
	// would make the user think a setting was saved when it was not.
	b, err := json.Marshal(req.Config)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var next config.Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("配置不合法：%w", err).Error())
		return
	}
	if err := validate(&next); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	before := flatten(c.cfg)
	after := flatten(&next)
	diff := changed(before, after)
	if len(diff) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"changed": []string{}, "applied": []string{}, "restart_required": []string{}})
		return
	}

	// A dangerous field can lock the operator out or move storage, so it takes
	// a second, explicit confirmation rather than a single click.
	var risky []string
	for _, f := range configschema.Fields() {
		if !f.Danger {
			continue
		}
		for _, p := range diff {
			if p == f.Path {
				risky = append(risky, f.Path)
			}
		}
	}
	if len(risky) > 0 && !req.Confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":            "以下字段会影响服务可达性，需确认后保存",
			"confirm_required": risky,
		})
		return
	}

	if err := c.snapshot(); err != nil {
		// A missing backup is not a reason to refuse the edit, but the user
		// should know there is nothing to roll back to.
		c.logf("config snapshot failed: %v", err)
	}
	if err := config.Save(c.cfgPath, &next); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("写入配置文件失败：%w", err).Error())
		return
	}
	applied, restart := c.apply(next, diff)
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":          diff,
		"applied":          applied,
		"restart_required": restart,
		"snapshots":        c.listSnapshots(),
	})
}

// apply hands the new config to the components that can absorb it and returns
// what was applied and what the operator still has to restart for.
func (c *Console) apply(next config.Config, diff []string) (applied, restart []string) {
	touched := map[string]bool{}
	for _, p := range diff {
		touched[p] = true
	}
	// Without a hot-apply hook the console can only report what changed, not
	// what was pushed into a running component.
	applied = append(applied, nonRestart(diff)...)
	if c.applyHot != nil {
		hot, cold := c.applyHot(&next, diff)
		if hot != nil {
			applied = hot
		}
		if cold != nil {
			restart = cold
		}
	}
	for _, p := range diff {
		if configschema.NeedsRestart(p) && !contains(restart, p) {
			restart = append(restart, p)
		}
	}
	// The in-memory config is the last thing to change, so a component that
	// reads it sees the new value only after it has been reconfigured.
	*c.cfg = next
	return applied, restart
}

// nonRestart keeps only the paths that can be applied in place.
func nonRestart(diff []string) []string {
	var out []string
	for _, p := range diff {
		if !configschema.NeedsRestart(p) {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Snapshots keep the last good config, so an edit that leaves the gateway
// unreachable can be undone from the console itself instead of by hand.
const maxSnapshots = 20

func (c *Console) snapshotDir() string {
	return filepath.Join(c.cfg.DataDir, "config-snapshots")
}

func (c *Console) snapshot() error {
	dir := c.snapshotDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := time.Now().Format("20060102-150405") + ".json"
	b, err := json.MarshalIndent(c.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		return err
	}
	return c.pruneSnapshots()
}

func (c *Console) pruneSnapshots() error {
	dir := c.snapshotDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) <= maxSnapshots {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries[:len(entries)-maxSnapshots] {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

type snapshotEntry struct {
	ID   string `json:"id"`
	Time string `json:"time"`
}

func (c *Console) listSnapshots() []snapshotEntry {
	entries, err := os.ReadDir(c.snapshotDir())
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	out := make([]snapshotEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, snapshotEntry{ID: strings.TrimSuffix(name, ".json"), Time: name})
	}
	return out
}

func (c *Console) handleConfigSnapshots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": c.listSnapshots()})
}

type rollbackRequest struct {
	ID string `json:"id"`
}

// handleConfigRollback restores a saved snapshot. It is the way back when an
// edit makes the gateway unreachable: the console is still serving, so the user
// never has to edit the file by hand.
func (c *Console) handleConfigRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.ContainsAny(req.ID, `/\`) {
		writeErr(w, http.StatusBadRequest, "快照名不合法")
		return
	}
	path := filepath.Join(c.snapshotDir(), req.ID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "找不到该快照")
		return
	}
	var restored config.Config
	if err := json.Unmarshal(b, &restored); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("快照无法解析：%w", err).Error())
		return
	}
	if err := validate(&restored); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.Save(c.cfgPath, &restored); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("写入配置文件失败：%w", err).Error())
		return
	}
	diff := changed(flatten(c.cfg), flatten(&restored))
	applied, restart := c.apply(restored, diff)
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":          diff,
		"applied":          applied,
		"restart_required": restart,
		"snapshots":        c.listSnapshots(),
	})
}

// handleRestart spawns a replacement process and then lets this one finish, so
// a structural change (listener, data directory, credential encryption) takes
// effect without the user going to the terminal.
func (c *Console) handleRestart(w http.ResponseWriter, r *http.Request) {
	if c.restart == nil {
		writeErr(w, http.StatusNotImplemented, "当前运行方式不支持自动重启，请手动重启进程")
		return
	}
	if err := config.Save(c.cfgPath, c.cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("保存配置失败：%w", err).Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true})
	// Flush the response before the process goes away.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		if err := c.restart(); err != nil {
			c.logf("restart failed: %v", err)
		}
	}()
}
