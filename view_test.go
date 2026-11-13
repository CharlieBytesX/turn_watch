package main

import (
	"fmt"

	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func visible(s string) string { return ansiRE.ReplaceAllString(s, "") }

// demoState: un estado inventado pero realista para probar el panel.
func demoState() *State {
	now := time.Now()
	st := &State{Updated: now, CPUs: 16, CPUPct: 55, MemUsedMB: 19800, MemTotMB: 31700,
		Slots: 10, SlotsFree: 2, LimitOn: true, System: true, AgentsCap: true, AgentsCapped: 33}
	st.Use = Usage{B: 6.4, A: 1.1, O: 1.3, BM: 4200, AM: 3100, OM: 9000}
	st.Active = []*Build{
		{Key: "1-1", Project: "miapp", Worktree: "agent-0123456789abcdef0", Branch: "worktree-agent-0123456789abcdef0", What: "test",
			Started: now.Add(-3*time.Minute - 10*time.Second), Phase: "compilando 4 crates",
			Activity: []string{"en pausa: 2 trabajos esperando turno", "compilando: miapp_media, miapp_server, tokio, serde_json", "enlazando: miapp_proto"},
			Done:     212, Rate: 34, Total: 480, Cores: 5.6, PeakCores: 9.8, MemMB: 3900, PeakMemMB: 5200, Procs: 17},
		{Key: "2-2", Project: "turn_watch-con-un-nombre-muy-largo-para-probar-el-corte", Started: now.Add(-40 * time.Second),
			Phase: "enlazando turn_watch", Cores: 1.2, MemMB: 800, Procs: 3},
	}
	st.Agents = []*AgentGroup{
		{Project: "miapp", Worktree: "agent-0123456789abcdef0", Branch: "main", Agents: []string{"claude"}, Cores: 7.1, BuildCores: 5.6,
			MemMB: 5100, BuildMemMB: 3900, Procs: 40, Builds: 1, Started: now.Add(-52 * time.Minute),
			Top: []TopCmd{{Cmd: "miapp_media-cdc7 (test)", Cores: 0.9, N: 1, MemMB: 600}, {Cmd: "postgres", Cores: 0.2, N: 6, MemMB: 300}}},
		{Project: "~", Agents: []string{"claude ×2"}, Cores: 0.4, MemMB: 900, Procs: 9, Started: now.Add(-3 * time.Hour),
			Top: []TopCmd{{Cmd: "claude", Cores: 0.3, N: 2, MemMB: 800}}},
		{Project: "otroapp", Branch: "feat/login", Agents: []string{"codex"}, Cores: 0.1, MemMB: 350, Procs: 4, Started: now.Add(-9 * time.Minute)},
	}
	st.Recent = []*Build{{Key: "3-3", Project: "miapp", What: "Linux x86_64 · check", Started: now.Add(-12 * time.Minute), Ended: now.Add(-9 * time.Minute), Done: 120, PeakCores: 8.1, PeakMemMB: 2100}}
	return st
}

// El panel ocupa exactamente la terminal: ni una fila ni una columna de más.
func TestPanelFits(t *testing.T) {
	st := demoState()
	dump := os.Getenv("TURN_WATCH_DUMP_DIR")
	for _, sz := range [][2]int{{24, 80}, {40, 120}, {60, 200}, {12, 60}, {8, 40}, {3, 20}, {30, 100}} {
		rows, cols := sz[0], sz[1]
		u := newUI()
		u.open["b1-1"] = true
		u.open["amiapp [agent-0123456…] (main)"] = true
		lines := panel(st, u, rows, cols)
		if len(lines) != rows {
			t.Errorf("%dx%d: %d filas", rows, cols, len(lines))
		}
		for i, l := range lines {
			if n := runeLen(visible(l)); n > cols {
				t.Errorf("%dx%d fila %d: %d columnas: %q", rows, cols, i, n, visible(l))
			}
		}
		if dump != "" {
			var plain []string
			for _, l := range lines {
				plain = append(plain, strings.TrimRight(visible(l), " "))
			}
			name := filepath.Join(dump, fmt.Sprintf("demo-%dx%d.txt", cols, rows))
			_ = os.WriteFile(name, []byte(strings.Join(plain, "\n")+"\n"), 0o644)
			// Con colores: verlo con `cat` en una terminal.
			_ = os.WriteFile(strings.TrimSuffix(name, ".txt")+".ans", []byte(strings.Join(lines, "\x1b[0m\n")+"\x1b[0m\n"), 0o644)
		}
	}
	// Sin servicio también.
	if l := panel(nil, newUI(), 24, 80); len(l) != 24 {
		t.Errorf("sin estado: %d filas", len(l))
	}
	if s := renderPlain(st, 100); !strings.Contains(s, "Compilaciones") {
		t.Errorf("once: %s", s)
	} else if dump != "" {
		_ = os.WriteFile(filepath.Join(dump, "demo-once.txt"), []byte(s), 0o644)
		_ = os.WriteFile(filepath.Join(dump, "demo-status.txt"), []byte(tooltip(st)+"\n"), 0o644)
	}
}

// Los grupos de agentes no brincan: persisten sin procesos, orden fijo, números suaves.
func TestStabilize(t *testing.T) {
	tr := newAgentTracker()
	t0 := time.Now()
	g := func(p, w string, c float64) *AgentGroup {
		return &AgentGroup{Project: p, Worktree: w, Cores: c, MemMB: 100}
	}
	out := tr.stabilize([]*AgentGroup{g("zeta", "", 0.1), g("miapp", "agent-b", 0.1), g("miapp", "agent-a", 0.1)}, t0)
	want := []string{"miapp/agent-a", "miapp/agent-b", "zeta/"}
	check := func(out []*AgentGroup) {
		t.Helper()
		if len(out) != len(want) {
			t.Fatalf("%d grupos, quería %d", len(out), len(want))
		}
		for i, x := range out {
			if x.Project+"/"+x.Worktree != want[i] {
				t.Fatalf("orden %d: %s", i, x.Project+"/"+x.Worktree)
			}
		}
	}
	check(out)
	// Un segundo después: uno sin procesos, otro con un pico de CPU.
	out = tr.stabilize([]*AgentGroup{g("miapp", "agent-a", 5.5), g("zeta", "", 0.1)}, t0.Add(time.Second))
	check(out)
	if !out[1].Idle {
		t.Error("agent-b debería seguir, como inactivo")
	}
	if c := out[0].Cores; c > 2 || c < 1 {
		t.Errorf("pico suavizado: %.2f (quería ~1,4)", c)
	}
	// Pasada la ventana, desaparece.
	out = tr.stabilize([]*AgentGroup{g("zeta", "", 0.1)}, t0.Add(groupKeep+2*time.Second))
	if len(out) != 1 {
		t.Errorf("tras %v quedan %d grupos", groupKeep, len(out))
	}
}
