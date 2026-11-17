package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "kache", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseKacheStats(t *testing.T) {
	s, err := parseKacheStats(fixture(t, "stats-ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Hits() != 1230 || s.Total() != 1500 || s.Window != "24h" {
		t.Fatalf("%+v", s)
	}
	if pct, ok := s.Ratio(); !ok || pct < 81.9 || pct > 82.1 {
		t.Errorf("ratio %.2f %v", pct, ok)
	}
	if s.TimeSavedMs != 4332000 || s.StoreBytes != 3435973837 || s.StoreLimitBytes != 10737418240 || s.Entries != 6120 {
		t.Errorf("%+v", s)
	}
	if s.Lifetime == nil || s.Lifetime.Hits != 48211 || s.Lifetime.TimeSavedMs != 151200000 {
		t.Errorf("lifetime %+v", s.Lifetime)
	}

	// Sin compilaciones: no hay porcentaje (no es 0 %).
	e, err := parseKacheStats(fixture(t, "stats-empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Ratio(); ok {
		t.Error("ratio con 0 compilaciones")
	}

	for name, want := range map[string]string{
		"stats-missing-field.json": "misses",
		"stats-failed.json":        "invalid --since",
		"stats-schema2.json":       "versión 2",
	} {
		if _, err := parseKacheStats(fixture(t, name)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (quería %q)", name, err, want)
		}
	}
	for _, bad := range []string{"", "kache 2.1.1\nHit rate 82%", `{"command":"stats","success":true}`, string(fixture(t, "doctor-issues.json"))} {
		if _, err := parseKacheStats([]byte(bad)); err == nil {
			t.Errorf("aceptó %q", bad[:min(len(bad), 30)])
		}
	}
}

func TestParseDaemonStatusAndDoctor(t *testing.T) {
	if up, v, err := parseDaemonStatus(fixture(t, "daemon-status-up.json")); err != nil || !up || v != "2.1.1" {
		t.Errorf("up: %v %q %v", up, v, err)
	}
	if up, _, err := parseDaemonStatus(fixture(t, "daemon-status-down.json")); err != nil || up {
		t.Errorf("down: %v %v", up, err)
	}
	d, err := parseDoctor(fixture(t, "doctor-issues.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Issues != 1 || len(d.Failed) != 1 || !strings.HasPrefix(d.Failed[0], "Cache FS:") {
		t.Errorf("%+v", d)
	}
}

// fakeKache responde con fixtures y anota qué se le pidió.
type fakeKache struct {
	answers map[string][]byte
	errs    map[string]error
	calls   []string
}

func (f *fakeKache) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	return f.answers[key], f.errs[key]
}

func watcherWith(t *testing.T, f *fakeKache, wrapper string) *kacheWatcher {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".cargo"), 0o755)
	if wrapper != "" {
		_ = os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[build]\nrustc-wrapper = \""+wrapper+"\"\n"), 0o644)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // sin kache-setup-error.txt de verdad
	return &kacheWatcher{
		run: f.run, find: func() string { return "/fake/kache" }, home: home,
		lookupEnv: envMap(nil), disabled: func() string { return "" },
	}
}

const (
	cmdStatus = "--json daemon status"
	cmdStats  = "--json stats --since 24h"
	cmdDoctor = "--json doctor"
)

func TestKacheWatcher(t *testing.T) {
	now := time.Now()
	t.Run("ok", func(t *testing.T) {
		f := &fakeKache{answers: map[string][]byte{
			cmdStatus: fixture(t, "daemon-status-up.json"), cmdStats: fixture(t, "stats-ok.json"), cmdDoctor: fixture(t, "doctor-issues.json"),
		}}
		w := watcherWith(t, f, "kache")
		w.Refresh(now)
		k := w.Snapshot()
		if k.State != kacheOK || k.Stats == nil || k.Doctor == nil || k.Doctor.Issues != 1 {
			t.Fatalf("%+v", k)
		}
		line, sev := kacheLine(k)
		if sev != sevOK || !strings.Contains(line, "82 % aciertos (1230/1500 en 24h)") || !strings.Contains(line, "ahorró 1h12m") {
			t.Errorf("%q", line)
		}
		// doctor no se repite antes de doctorEvery.
		f.calls = nil
		w.Refresh(now.Add(kacheEvery))
		for _, c := range f.calls {
			if c == cmdDoctor {
				t.Error("doctor otra vez antes de tiempo")
			}
		}
		if w.Snapshot().Doctor == nil {
			t.Error("se perdió el último doctor")
		}
	})
	t.Run("daemon parado: no se piden estadísticas", func(t *testing.T) {
		f := &fakeKache{answers: map[string][]byte{cmdStatus: fixture(t, "daemon-status-down.json"), cmdDoctor: fixture(t, "doctor-issues.json")}}
		w := watcherWith(t, f, "kache")
		w.Refresh(now)
		for _, c := range f.calls {
			if c == cmdStats {
				t.Error("pidió stats con el daemon parado (kache stats lo arrancaría)")
			}
		}
		if k := w.Snapshot(); k.State != kacheDaemonDown || k.Stats != nil {
			t.Errorf("%+v", k)
		}
	})
	t.Run("stats ilegibles: estado de error, sin 0 %", func(t *testing.T) {
		f := &fakeKache{answers: map[string][]byte{
			cmdStatus: fixture(t, "daemon-status-up.json"), cmdStats: []byte("Hit rate: 82%\n"), cmdDoctor: fixture(t, "doctor-issues.json"),
		}, errs: map[string]error{cmdStats: errors.New("exit status 1")}}
		w := watcherWith(t, f, "kache")
		w.Refresh(now)
		k := w.Snapshot()
		line, sev := kacheLine(k)
		if k.State != kacheError || sev != sevError || !strings.Contains(line, "estadísticas de kache no disponibles") || strings.Contains(line, "%") {
			t.Errorf("%s: %q", k.State, line)
		}
		if k.LastError == "" || k.LastErrorAt.IsZero() {
			t.Error("sin último error")
		}
	})
	t.Run("otro wrapper", func(t *testing.T) {
		f := &fakeKache{answers: map[string][]byte{cmdStatus: fixture(t, "daemon-status-up.json"), cmdStats: fixture(t, "stats-ok.json"), cmdDoctor: fixture(t, "doctor-issues.json")}}
		w := watcherWith(t, f, "sccache")
		w.Refresh(now)
		if k := w.Snapshot(); k.State != kacheConflict || !strings.Contains(k.Detail, "sccache") {
			t.Errorf("%+v", k)
		}
	})
	t.Run("sin configurar", func(t *testing.T) {
		f := &fakeKache{answers: map[string][]byte{cmdStatus: fixture(t, "daemon-status-up.json"), cmdStats: fixture(t, "stats-ok.json"), cmdDoctor: fixture(t, "doctor-issues.json")}}
		w := watcherWith(t, f, "")
		recordSetupError("kache init falló: boom")
		w.Refresh(now)
		if k := w.Snapshot(); k.State != kacheUnconfig || !strings.Contains(k.Detail, "boom") {
			t.Errorf("%+v", k)
		}
	})
	t.Run("sin binario", func(t *testing.T) {
		f := &fakeKache{}
		w := watcherWith(t, f, "kache")
		w.find = func() string { return "" }
		w.Refresh(now)
		if k := w.Snapshot(); k.State != kacheMissing || len(f.calls) != 0 {
			t.Errorf("%+v %v", k, f.calls)
		}
	})
	t.Run("desactivada", func(t *testing.T) {
		f := &fakeKache{}
		w := watcherWith(t, f, "kache")
		w.disabled = func() string { return "desactivada (TURN_WATCH_SIN_KACHE)" }
		w.Refresh(now)
		if k := w.Snapshot(); k.State != kacheDisabled || len(f.calls) != 0 {
			t.Errorf("%+v", k)
		}
	})
	if l, _ := kacheLine(nil); !strings.Contains(l, "sin datos") {
		t.Errorf("nil: %q", l)
	}
}
