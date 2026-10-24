package configschema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/config"
)

// configPaths walks the Config struct and returns every dotted path a field can
// live at. Using reflection rather than a JSON round trip matters: the optional
// fields carry omitempty, so a marshalled default would silently omit exactly
// the paths most likely to be misspelled.
func configPaths(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walk(path, ft)
				continue
			}
			out[path] = true
		}
	}
	walk("", reflect.TypeOf(config.Config{}))
	return out
}

// A typo in a path would sail through the server — the field would simply
// never resolve — and the operator would be left staring at a control that
// saves nothing. The test makes that a build failure instead.
func TestEveryPathExistsInConfig(t *testing.T) {
	known := configPaths(t)
	for _, f := range Fields() {
		if !known[f.Path] {
			t.Errorf("schema path %q (%s) does not exist in config.Config", f.Path, f.Label)
		}
	}
}

func TestPathsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fields() {
		if seen[f.Path] {
			t.Errorf("duplicate schema path %q", f.Path)
		}
		seen[f.Path] = true
	}
}

func TestRestartPathsAreEditable(t *testing.T) {
	known := map[string]bool{}
	for _, f := range Fields() {
		known[f.Path] = true
	}
	for _, p := range Restart {
		if !known[p] {
			t.Errorf("Restart lists %q, which no field exposes; the restart notice can never appear", p)
		}
	}
}

func TestRestartFieldsAreCold(t *testing.T) {
	byPath := map[string]Field{}
	for _, f := range Fields() {
		byPath[f.Path] = f
	}
	for _, p := range Restart {
		f, ok := byPath[p]
		if !ok {
			continue // reported by TestRestartPathsAreEditable
		}
		if f.Hot {
			t.Errorf("%q is listed in Restart but marked hot", p)
		}
	}
}

// The schedule section is organised by task, everything else by block. A field
// that drifts between the two shows up as an untitled card in the console.
func TestScheduleFieldsCarryATask(t *testing.T) {
	for _, f := range Fields() {
		if f.Group != SectionSchedule {
			if f.Task != "" {
				t.Errorf("%q is not in %s but names task %q", f.Path, SectionSchedule, f.Task)
			}
			continue
		}
		if f.Task == "" {
			t.Errorf("%q is a schedule field without a task", f.Path)
		}
	}
}

// Every upstream field must live in a platform block; otherwise it renders
// above the first heading and reads as if it belonged to neither source.
func TestUpstreamFieldsCarryABlock(t *testing.T) {
	for _, f := range Fields() {
		if f.Group == SectionUpstream && f.Block == "" {
			t.Errorf("%q is an upstream field without a block", f.Path)
		}
	}
}

func TestBlockNamesAreKnown(t *testing.T) {
	known := map[string]bool{
		BlockPool: true, BlockRouter: true, BlockUpCommon: true,
		BlockUpWorkBuddy: true, BlockUpTraeCN: true,
	}
	for _, f := range Fields() {
		if f.Block != "" && !known[f.Block] {
			t.Errorf("%q names unknown block %q", f.Path, f.Block)
		}
	}
}

func TestEverySectionIsUsed(t *testing.T) {
	want := []string{
		SectionService, SectionSchedule, SectionPool, SectionUpstream,
		SectionFeature, SectionObserv, SectionSecurity,
	}
	seen := map[string]bool{}
	for _, f := range Fields() {
		seen[f.Group] = true
	}
	for _, s := range want {
		if !seen[s] {
			t.Errorf("section %q has no fields", s)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("got %d distinct sections, want %d", len(seen), len(want))
	}
}

func TestEnumFieldsCarryOptions(t *testing.T) {
	for _, f := range Fields() {
		if f.Kind == "enum" && len(f.Options) < 2 {
			t.Errorf("%q is an enum with %d options", f.Path, len(f.Options))
		}
	}
}

// The header switch is drawn as a toggle, so it has to be a bool, and there can
// only be one per group — two would leave the console picking arbitrarily.
func TestPrimaryIsASingleBoolPerGroup(t *testing.T) {
	seen := map[string]string{}
	for _, f := range Fields() {
		if !f.Primary {
			continue
		}
		if f.Kind != "bool" {
			t.Errorf("%q is primary but is a %s", f.Path, f.Kind)
		}
		group := f.Group + "/" + f.Task + "/" + f.Block
		if prev, dup := seen[group]; dup {
			t.Errorf("group %s has two primary fields: %q and %q", group, prev, f.Path)
		}
		seen[group] = f.Path
	}
}
