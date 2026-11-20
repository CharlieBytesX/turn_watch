// turn_watch: ve las compilaciones de Rust que corren en esta máquina, tanto
// `cargo` directo como las que corren dentro de `docker build` / `uc build`.
//
//	turn_watch            el panel (TUI). El límite de núcleos (cola + pausa y, en
//	                     Linux, tope de CPU) lo mantiene el servicio de fondo.
//	turn_watch install    lo deja todo listo para el usuario (ver `turn_watch help`).
//
// Solo lee la lista de procesos (/proc en Linux; sysctl, ps y lsof en macOS: ver
// proc_linux.go y proc_darwin.go): no necesita permisos especiales. De los procesos
// de otro usuario (los de Docker corren como root) no se puede leer el directorio
// de trabajo; para esos, el total de crates sale del Cargo.lock del proyecto desde
// el que se lanzó `docker build` / `uc build`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tick        = time.Second
	historyLen  = 60
	recentKeep  = 30 * time.Minute
	minNotify   = 20 * time.Second // compilaciones más cortas no avisan
	maxRecent   = 10
	stateFolder = "turn_watch"
)

// Build es una compilación: un `cargo` raíz y todo lo que cuelga de él.
type Build struct {
	Key       string    `json:"key"`
	Project   string    `json:"project"`
	Worktree  string    `json:"worktree,omitempty"` // si se lanzó desde un worktree de git
	Dir       string    `json:"dir,omitempty"`      // carpeta del repo o worktree
	Branch    string    `json:"branch,omitempty"`
	What      string    `json:"what"` // plataforma / paquete / subcomando
	Docker    bool      `json:"docker"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	Phase     string    `json:"phase"`    // qué hace ahora, en una frase
	Activity  []string  `json:"activity"` // detalle: crates, build scripts, enlazado…
	Done      int       `json:"done"`     // crates listos: compilados o de la caché (progress.go)
	Rate      int       `json:"rate"`     // crates empezados en el último minuto
	Total     int       `json:"total,omitempty"`
	Cores     float64   `json:"cores"`
	PeakCores float64   `json:"peak_cores"`
	History   []float64 `json:"history"`
	MemMB     int       `json:"mem_mb"`
	PeakMemMB int       `json:"peak_mem_mb"`
	Procs     int       `json:"procs"`
	Paused    int       `json:"paused,omitempty"` // trabajos en pausa esperando turno

	seen     map[string]bool
	seenAt   []time.Time
	rootPid  int
	lastCPU  uint64
	lastTime time.Time
	memEMA   float64 // RAM suavizada

	targetDir string          // su carpeta target ("" si no se sabe)
	fpSeen    map[string]bool // unidades terminadas según las huellas de cargo
	fpDone    int
	fpLast    time.Time
}

func (b *Build) Label() string {
	s := placeLabel(b.Project, b.Worktree, b.Branch)
	if b.What != "" {
		s += " · " + b.What
	}
	return s
}

type State struct {
	Updated   time.Time `json:"updated"`
	Load      float64   `json:"load"`
	CPUs      int       `json:"cpus"`
	CPUPct    float64   `json:"cpu_pct"` // uso total de la máquina
	Use       Usage     `json:"use"`     // reparto de ahora: compilaciones / agentes / otros
	MemUsedMB int       `json:"mem_used_mb"`
	MemTotMB  int       `json:"mem_total_mb"`
	Slots     int       `json:"slots"`      // núcleos de la cola (0: sin cola)
	SlotsFree int       `json:"slots_free"` // turnos libres ahora
	Holds     []string  `json:"holds"`      // reservas de `turn_watch hold`
	LimitOn   bool      `json:"limit_on"`   // límite activo (lo mantiene el servicio)
	System    bool      `json:"system"`     // el ayudante de root está corriendo
	DaemonPID int       `json:"daemon_pid"`
	Active    []*Build  `json:"active"`
	Recent    []*Build  `json:"recent"`

	// Agentes de código (claude, codex…): todo lo que lanzan, por worktree.
	Agents       []*AgentGroup `json:"agents,omitempty"`
	AgentsCap    bool          `json:"agents_cap"`    // configurado (agents_cap)
	AgentsCapped int           `json:"agents_capped"` // procesos en turn_watch-agentes.scope
	// NoCap: este sistema no tiene cómo poner tope de CPU (macOS: sin cgroups); el
	// límite es solo la cola y la pausa.
	NoCap bool `json:"no_cap,omitempty"`

	// Kache: salud y estadísticas de la caché (nil: servicio de una versión anterior).
	Kache *KacheStatus `json:"kache,omitempty"`
}

// Usage: en qué se va la máquina, en núcleos y MB. Las tres partes no se pisan:
// "agentes" es lo que lanzan los agentes sin contar sus compilaciones, y "otros" es
// el resto del equipo (escritorio, navegador, Docker que no compila…).
type Usage struct {
	B  float64 `json:"b"`  // compilaciones (núcleos)
	A  float64 `json:"a"`  // agentes
	O  float64 `json:"o"`  // otros
	BM int     `json:"bm"` // compilaciones (MB)
	AM int     `json:"am"`
	OM int     `json:"om"`
}

func (u Usage) Cores() float64 { return u.B + u.A + u.O }
func (u Usage) Mem() int       { return u.BM + u.AM + u.OM }

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

func main() {
	if isShim() {
		shim()
		return
	}
	cmd := "watch"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "daemon":
		daemon()
	case "status":
		status()
	case "watch":
		watch()
	case "sistema":
		sistemaCmd(os.Args[2:])
	case "enforce":
		enforceCmd(os.Args[2:])
	case "hold":
		hold(os.Args[2:])
	case "on", "off":
		if !daemonUp(readState()) {
			fmt.Fprintln(os.Stderr, "turn_watch: el servicio no está corriendo ("+startHint()+")")
			os.Exit(1)
		}
		request(cmd)
		if cmd == "on" {
			ensureHelper()
		}
		fmt.Println("límite:", map[string]string{"on": "activado", "off": "desactivado"}[cmd])
	case "limit":
		limitCmd(os.Args[2:])
	case "install":
		install()
	case "uninstall":
		uninstall()
	case "version", "--version", "-V":
		fmt.Println("turn_watch", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "once":
		st := readState()
		fmt.Print(renderPlainWarn(st, 100, localWarnings(st)))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// homeOrEmpty: el directorio de inicio, o "" (sin fallar: lo usa el servicio).
func homeOrEmpty() string {
	h, _ := os.UserHomeDir()
	return h
}

func statePath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = runtimeBase()
	}
	return filepath.Join(dir, stateFolder, "state.json")
}

// ---------- procesos ----------

// proc: un proceso, leído de /proc (Linux) o de sysctl + ps (macOS).
type proc struct {
	pid, ppid int
	uid       int
	comm      string
	args      []string
	start     uint64 // ticks desde el arranque
	cpu       uint64 // utime + stime + cutime + cstime en ticks
	own       uint64 // utime + stime: solo el proceso
	kids      uint64 // cutime + cstime: hijos ya terminados y recogidos
	paused    bool   // detenido (SIGSTOP): esperando turno
	zombie    bool
	rssKB     int64
	// psArgs: argumentos según ps (macOS), partidos por espacios; por si no se
	// pueden leer los exactos.
	psArgs []string
}

// clkTck: unidad de los tiempos de CPU y de inicio (ticks por segundo). En Linux es
// sysconf(_SC_CLK_TCK); en macOS se convierte a la misma unidad.
const clkTck = 100.0

func (p *proc) loadArgs() []string {
	if p.args == nil {
		p.args = readArgs(p)
	}
	return p.args
}

func isCargo(p *proc) bool { return p.comm == "cargo" || p.comm == "cargo-zigbuild" }

// ---------- detección ----------

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

func underDocker(procs map[int]*proc, p *proc) bool {
	for q := procs[p.ppid]; q != nil && q.pid > 1; q = procs[q.ppid] {
		if q.comm == "runc" || strings.HasPrefix(q.comm, "containerd-shim") || q.comm == "buildkitd" {
			return true
		}
	}
	return false
}

// cargoLock busca Cargo.lock subiendo desde dir y cuenta sus paquetes.
func cargoLock(dir string) (string, int) {
	for d := dir; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "Cargo.lock"))
		if err == nil {
			return d, strings.Count(string(b), "[[package]]")
		}
	}
	return "", 0
}

// dockerProject: el proyecto desde el que el usuario lanzó un docker/uc build.
func dockerProject(procs map[int]*proc) (string, int, string) {
	me := os.Getuid()
	for _, p := range procs {
		if p.uid != me || (p.comm != "docker" && p.comm != "uc" && p.comm != "docker-buildx") {
			continue
		}
		args := p.loadArgs()
		if !strings.Contains(strings.Join(args, " "), "build") {
			continue
		}
		cwd := procCwd(p, true)
		if dir, n := cargoLock(cwd); dir != "" {
			return filepath.Base(dir), n, dir
		}
	}
	return "", 0, ""
}

func friendlyTarget(t string) string {
	switch {
	case t == "":
		return ""
	case strings.Contains(t, "apple-darwin"):
		if strings.HasPrefix(t, "universal2") {
			return "macOS (universal)"
		}
		return "macOS " + strings.SplitN(t, "-", 2)[0]
	case strings.Contains(t, "windows"):
		return "Windows"
	case strings.Contains(t, "linux"):
		s := "Linux " + strings.SplitN(t, "-", 2)[0]
		if i := strings.LastIndex(t, ".2."); i > 0 {
			s += " (glibc " + t[i+1:] + ")"
		}
		return s
	}
	return t
}

func describe(root *proc, docker bool, dproj string, dtotal int, ddir string) (project, worktree, branch, what string, total int) {
	args := root.loadArgs()
	sub := ""
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			sub = a
			break
		}
	}
	pkg := argValue(args, "-p")
	if pkg == "" {
		pkg = argValue(args, "--package")
	}
	var parts []string
	if docker {
		project, total = dproj, dtotal
		if project == "" {
			project = pkg
		}
		if project == "" {
			project = "?"
		}
		if ddir != "" {
			if p, w, br := gitInfo(ddir); p != "" {
				project, worktree, branch = p, w, br
			}
		}
		project += " (docker)"
	} else {
		cwd := procCwd(root, true)
		dir, n := cargoLock(cwd)
		total = n
		project = filepath.Base(dir)
		if dir == "" {
			project = filepath.Base(cwd)
		}
		if p, w, br := gitInfo(cwd); p != "" {
			project, worktree, branch = p, w, br
		}
	}
	if t := friendlyTarget(argValue(args, "--target")); t != "" {
		parts = append(parts, t)
	}
	if pkg != "" && !strings.HasPrefix(project, pkg) {
		parts = append(parts, pkg)
	}
	if sub != "" && sub != "build" && sub != "zigbuild" {
		parts = append(parts, sub)
	}
	return project, worktree, branch, strings.Join(parts, " · "), total
}

// buildScriptCrate: …/build/<crate>-<hash>/build-script-build -> <crate>
func buildScriptCrate(path string) string {
	d := filepath.Base(filepath.Dir(path))
	if i := strings.LastIndexByte(d, '-'); i > 0 {
		return d[:i]
	}
	return d
}

// activity: qué está haciendo el subárbol de un build.
type activity struct {
	compiling []string
	linking   []string
	cached    []string // en manos del rustc-wrapper (kache) sin rustc: de la caché
	scripts   []string
	native    int // compiladores de C/C++
	fetching  bool
	metadata  bool
	paused    int // trabajos en pausa esperando turno
}

func (a *activity) phase(b *Build, cpu float64) (string, []string) {
	var lines []string
	if a.paused > 0 {
		lines = append(lines, fmt.Sprintf("en pausa: %d trabajos esperando turno", a.paused))
	}
	if len(a.compiling) > 0 {
		lines = append(lines, "compilando: "+list(a.compiling, 6))
	}
	if len(a.scripts) > 0 {
		lines = append(lines, "build.rs de: "+list(a.scripts, 4))
	}
	if a.native > 0 {
		lines = append(lines, fmt.Sprintf("C/C++: %d archivos a la vez", a.native))
	}
	if len(a.linking) > 0 {
		lines = append(lines, "enlazando: "+list(a.linking, 3))
	}
	if len(a.cached) > 0 {
		lines = append(lines, "de la caché: "+list(a.cached, 6))
	}
	if a.paused > 0 && len(a.compiling) == 0 && len(a.scripts) == 0 && len(a.linking) == 0 {
		return "en pausa: esperando turno", lines
	}
	switch {
	case len(a.linking) > 0 && len(a.compiling) <= len(a.linking):
		return "enlazando " + a.linking[0], lines
	case len(a.compiling) > 0:
		if len(a.compiling) == 1 {
			return "compilando " + a.compiling[0], lines
		}
		return fmt.Sprintf("compilando %d crates", len(a.compiling)), lines
	case len(a.scripts) > 0:
		return "corriendo build.rs de " + a.scripts[0], lines
	case len(a.cached) > 0:
		return "sacando de la caché " + list(a.cached, 2), lines
	case a.native > 0:
		return "compilando C/C++", lines
	case a.fetching:
		return "descargando dependencias (git)", lines
	case a.metadata:
		return "leyendo el proyecto (cargo metadata)", lines
	case b.Done == 0 && cpu < 0.3:
		return "esperando: índice de crates / descargas", lines
	case cpu < 0.3:
		return "esperando", lines
	}
	return "trabajando", lines
}

func list(xs []string, max int) string {
	if len(xs) <= max {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:max], ", ") + fmt.Sprintf(" +%d", len(xs)-max)
}

// scan actualiza builds con lo que hay ahora en /proc. Devuelve las que terminaron.
func scan(procs map[int]*proc, children map[int][]*proc, builds map[string]*Build, boot, now time.Time) []*Build {
	var dproj, ddir string
	var dtotal int
	dockerChecked := false
	alive := map[string]bool{}
	for _, p := range procs {
		if !isCargo(p) {
			continue
		}
		nested := false
		for q := procs[p.ppid]; q != nil && q.pid > 1; q = procs[q.ppid] {
			if isCargo(q) {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		key := fmt.Sprintf("%d-%d", p.pid, p.start)
		alive[key] = true
		b := builds[key]
		if b == nil {
			docker := underDocker(procs, p)
			if docker && !dockerChecked {
				dproj, dtotal, ddir = dockerProject(procs)
				dockerChecked = true
			}
			project, worktree, branch, what, total := describe(p, docker, dproj, dtotal, ddir)
			// Universal = dos arquitecturas: cada crate se compila dos veces.
			if strings.Contains(what, "universal") {
				total *= 2
			}
			b = &Build{
				Key: key, Project: project, Worktree: worktree, Branch: branch, What: what, Docker: docker, Total: total,
				Started: boot.Add(time.Duration(float64(p.start) / clkTck * float64(time.Second))),
				seen:    map[string]bool{},
				rootPid: p.pid,
			}
			if docker {
				b.Dir = gitRoot(ddir)
			} else if cwd := procCwd(p, true); cwd != "" {
				b.Dir = gitRoot(cwd)
				b.targetDir = resolveTargetDir(p.loadArgs(), func(k string) string { return procEnv(p, k) }, cwd, cargoHomeDir(os.LookupEnv, homeOrEmpty()))
			}
			builds[key] = b
		}
		var cpu uint64
		var rss int64
		var act activity
		n := 0
		stack := []*proc{p}
		for len(stack) > 0 {
			q := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cpu += q.cpu
			rss += q.rssKB
			n++
			if q.paused && isJob(q) {
				act.paused++
				stack = append(stack, children[q.pid]...)
				continue
			}
			switch {
			case q.comm == "rustc":
				c := argValue(q.loadArgs(), "--crate-name")
				if c == "" {
					break
				}
				if c == "build_script_build" {
					c = "build.rs"
				}
				b.mark(c + "@" + argValue(q.args, "--target"))
				// rustc que lanzó al enlazador: ya está enlazando.
				linking := false
				for _, ch := range children[q.pid] {
					if isLinker(ch) {
						linking = true
					}
				}
				if linking {
					act.linking = append(act.linking, c)
				} else {
					act.compiling = append(act.compiling, c)
				}
			case isRustcWrapper(q):
				// kache/sccache envolviendo un rustc: cuenta ya (un acierto de la caché
				// no lanza rustc). Si lanzó rustc, ese hijo dice qué hace.
				c := argValue(q.loadArgs(), "--crate-name")
				if c == "" {
					break
				}
				if c == "build_script_build" {
					c = "build.rs"
				}
				b.mark(c + "@" + argValue(q.args, "--target"))
				hasRustc := false
				for _, ch := range children[q.pid] {
					if ch.comm == "rustc" {
						hasRustc = true
					}
				}
				if !hasRustc {
					act.cached = append(act.cached, c)
				}
			case strings.HasPrefix(q.comm, "build-script"):
				act.scripts = append(act.scripts, buildScriptCrate(q.loadArgs()[0]))
			case isNativeCompiler(q):
				act.native++
			case strings.HasPrefix(q.comm, "git"):
				act.fetching = true
			}
			if isCargo(q) && q != p {
				for _, a := range q.loadArgs() {
					if a == "metadata" {
						act.metadata = true
					}
				}
			}
			stack = append(stack, children[q.pid]...)
		}
		sort.Strings(act.compiling)
		sort.Strings(act.cached)
		b.scanFingerprints(now)
		b.updateDone()
		b.Procs = n
		b.Rate = 0
		for _, t := range b.seenAt {
			if now.Sub(t) <= time.Minute {
				b.Rate++
			}
		}
		memRaw := float64(rss / 1024)
		if int(memRaw) > b.PeakMemMB {
			b.PeakMemMB = int(memRaw)
		}
		if b.lastTime.IsZero() {
			b.memEMA = memRaw
		} else {
			b.memEMA = ema(b.memEMA, memRaw, now.Sub(b.lastTime))
		}
		b.MemMB = int(b.memEMA + 0.5)
		if !b.lastTime.IsZero() && cpu >= b.lastCPU {
			if secs := now.Sub(b.lastTime).Seconds(); secs > 0 {
				raw := float64(cpu-b.lastCPU) / clkTck / secs
				if raw > b.PeakCores {
					b.PeakCores = raw
				}
				// Suavizado (~3 s): que no salte de 0,1 a 5,5 entre lecturas.
				b.Cores = round2(ema(b.Cores, raw, now.Sub(b.lastTime)))
			}
		}
		b.History = appendHist(b.History, b.Cores)
		b.lastCPU, b.lastTime = cpu, now
		b.Phase, b.Activity = act.phase(b, b.Cores)
		b.Paused = act.paused
	}
	var ended []*Build
	for k, b := range builds {
		if !alive[k] {
			b.Ended = now
			b.Phase, b.Activity, b.Cores, b.Paused = "", nil, 0, 0
			ended = append(ended, b)
			delete(builds, k)
		}
	}
	return ended
}

func isLinker(p *proc) bool {
	switch p.comm {
	case "ld", "ld.lld", "lld", "ld.bfd", "ld.gold", "mold", "collect2", "x86_64-w64-ming", "ld64.lld":
		return true
	case "cc", "gcc", "clang", "zig", "c++":
		// cc/zig colgando de rustc = enlazado.
		return true
	}
	return strings.HasSuffix(p.comm, "-ld") || strings.HasPrefix(p.comm, "zigcc")
}

func isNativeCompiler(p *proc) bool {
	switch p.comm {
	case "cc1", "cc1plus", "clang", "clang++", "cc", "gcc", "g++", "c++", "zig", "nasm", "cmake", "make":
		return true
	}
	return strings.HasPrefix(p.comm, "x86_64-w64-")
}

func appendHist(h []float64, v float64) []float64 {
	h = append(h, v)
	if len(h) > historyLen {
		h = h[len(h)-historyLen:]
	}
	return h
}

// ---------- monitor ----------

// Monitor sigue las compilaciones. Lo usan el daemon (barra y avisos) y el TUI.
type Monitor struct {
	boot                time.Time
	builds              map[string]*Build
	recent              []*Build
	lastBusy, lastTotal uint64
	pct                 float64 // CPU de la máquina, suavizada
	lastTick            time.Time
	agents              *agentTracker
}

func newMonitor() *Monitor {
	m := &Monitor{boot: bootTime(), builds: map[string]*Build{}, agents: newAgentTracker()}
	m.lastBusy, m.lastTotal = cpuTimes()
	return m
}

// Tick lee /proc (una sola vez) y devuelve el estado y las compilaciones que terminaron.
func (m *Monitor) Tick(now time.Time) (*State, []*Build) {
	procs := readProcs()
	children := map[int][]*proc{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	ended := scan(procs, children, m.builds, m.boot, now)
	agents := m.agents.update(procs, children, m.builds, m.boot, now)
	m.recent = append(ended, m.recent...)
	kept := m.recent[:0]
	for _, b := range m.recent {
		if now.Sub(b.Ended) < recentKeep && len(kept) < maxRecent {
			kept = append(kept, b)
		}
	}
	m.recent = kept
	busy, total := cpuTimes()
	pct := 0.0
	if total > m.lastTotal {
		pct = 100 * float64(busy-m.lastBusy) / float64(total-m.lastTotal)
	}
	// Suavizado (~3 s), como el resto de los números del panel.
	if m.lastTick.IsZero() {
		m.pct = pct
	} else {
		m.pct = ema(m.pct, pct, now.Sub(m.lastTick))
	}
	m.lastTick = now
	pct = m.pct
	m.lastBusy, m.lastTotal = busy, total
	used, tot := memInfo()
	cpus := numCPU()
	st := &State{Updated: now, Load: loadAvg(), CPUs: cpus, CPUPct: pct, MemUsedMB: used, MemTotMB: tot}
	st.Holds = holds(procs)
	st.Slots, st.SlotsFree = queueStatus()
	for _, b := range m.builds {
		st.Active = append(st.Active, b)
	}
	sort.Slice(st.Active, func(i, j int) bool { return st.Active[i].Started.Before(st.Active[j].Started) })
	st.Recent = m.recent
	st.Agents = agents

	// Reparto para las gráficas. Las mediciones no son simultáneas: lo que no cuadra
	// se recorta a cero en vez de dar números negativos.
	var u Usage
	for _, b := range st.Active {
		u.B += b.Cores
		u.BM += b.MemMB
	}
	for _, g := range agents {
		u.A += max(g.Cores-g.BuildCores, 0)
		u.AM += max(g.MemMB-g.BuildMemMB, 0)
	}
	u.O = max(pct/100*float64(cpus)-u.B-u.A, 0)
	u.OM = max(used-u.BM-u.AM, 0)
	u.B, u.A, u.O = round2(u.B), round2(u.A), round2(u.O)
	st.Use = u
	return st, ended
}

// Busy: algo compila o tiene turnos reservados.
func (m *Monitor) Busy(st *State) bool { return len(m.builds) > 0 || len(st.Holds) > 0 }

// Sample: entre lecturas completas, mirar cada 100 ms qué rustc hay (la mayoría de
// los crates compila en menos de un segundo y si no se perderían).
func (m *Monitor) Sample() { sample(m.builds) }

// ---------- daemon ----------

// daemon: barra (estado en $XDG_RUNTIME_DIR/turn_watch/state.json) y avisos al terminar.
// No limita nada: el límite es del TUI.
func daemon() {
	m := newMonitor()
	path := statePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	// Lo que dejó en pausa el servicio de antes del cambio de nombre (rustwatch); lo
	// de una sesión anterior de turn_watch lo reanuda newEnforcer.
	resumeFile(oldRuntimeDir(), os.Getuid(), "stopped.json")
	migrateConfig()
	ticks := 0
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	fast := time.NewTicker(100 * time.Millisecond)
	defer fast.Stop()
	t := time.NewTicker(tick)
	defer t.Stop()

	// El límite vive aquí: se activa/desactiva con `turn_watch on|off` (o el TUI) y
	// sigue activo aunque se cierre el TUI.
	var q *Queue
	enf := newEnforcer(false)
	acap := newAgentCap()
	cfg := loadConfig()
	// kache se consulta aparte: sus comandos tardan y el bucle no debe esperar.
	kw := newKacheWatcher()
	kstop := make(chan struct{})
	defer close(kstop)
	go kw.Loop(kstop)
	apply := func(active bool) {
		switch {
		case active && q == nil:
			var err error
			if q, err = startQueue(loadConfig().Slots); err != nil {
				fmt.Fprintln(os.Stderr, "turn_watch: no se pudo activar el límite:", err)
				q = nil
			}
		case !active && q != nil:
			enf.Release()
			q.Stop()
			q = nil
		}
	}
	defer func() {
		enf.Release()
		if q != nil {
			q.Stop()
		}
	}()
	apply(loadConfig().Active)
	fastN := 0
	for {
		if b, err := os.ReadFile(controlPath()); err == nil {
			_ = os.Remove(controlPath())
			on := strings.TrimSpace(string(b)) == "on"
			setActiveConfig(on)
			apply(on)
		}
		now := time.Now()
		if c, ok := loadConfigOK(); ok {
			cfg = c
		}
		m.agents.names = cfg.agentNames()
		st, ended := m.Tick(now)
		if q != nil {
			q.Tick(m.Busy(st), now)
			st.Slots, st.SlotsFree = q.Slots(), q.Free()
		}
		st.LimitOn = q != nil
		// Tope de los agentes: dentro de turn_watch.slice mientras el límite esté activo.
		st.AgentsCap = cfg.agentsCap() && agentCapSupported
		st.NoCap = !agentCapSupported
		switch {
		case !st.AgentsCap:
			acap.Release()
		case q != nil:
			acap.Capture(m.agents.tree)
		}
		st.AgentsCapped = acap.Captured()
		st.System = helperAlive()
		st.DaemonPID = os.Getpid()
		st.Kache = kw.Snapshot()
		for _, b := range ended {
			if b.Ended.Sub(b.Started) >= minNotify {
				notify(b)
			}
		}
		writeState(path, st)
		if ticks++; ticks%30 == 1 {
			cleanupOldSlice()
		}
	wait:
		for {
			select {
			case <-sig:
				_ = os.Remove(path)
				return
			case <-fast.C:
				m.Sample()
				fastN++
				if fastN%2 == 0 && q != nil {
					if helperAlive() {
						enf.Release() // lo hace el ayudante de root
					} else {
						enf.Tick(q.Slots())
					}
				}
			case <-t.C:
				break wait
			}
		}
	}
}

func writeState(path string, st *State) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func notify(b *Build) {
	body := fmt.Sprintf("Terminó en %s · %d crates · pico %.1f núcleos, %s",
		dur(b.Ended.Sub(b.Started)), b.Done, b.PeakCores, mb(b.PeakMemMB))
	notifyDesktop("Rust: "+b.Label(), body)
}

func readState() *State {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	st := &State{}
	if json.Unmarshal(b, st) != nil {
		return nil
	}
	return st
}

// ---------- formato ----------

func dur(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func mb(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(n)/1024)
	}
	return fmt.Sprintf("%d MB", n)
}

// mark anota un crate (por destino) la primera vez que se ve compilando.
func (b *Build) mark(key string) {
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	now := time.Now()
	b.seenAt = append(b.seenAt, now)
	for len(b.seenAt) > 0 && now.Sub(b.seenAt[0]) > time.Minute {
		b.seenAt = b.seenAt[1:]
	}
}

// progressText: cuántos crates están listos (compilados o de la caché) y a qué
// ritmo. No hay porcentaje: lo que ya estaba al día no se toca y cargo no dice
// cuánto falta.
func progressText(b *Build) string {
	s := fmt.Sprintf("%d crates listos", b.Done)
	if b.Done == 1 {
		s = "1 crate listo"
	}
	if b.Rate > 0 {
		s += fmt.Sprintf(" · %d/min", b.Rate)
	}
	return s
}

// daemonUp: el servicio escribió su estado hace poco.
func daemonUp(st *State) bool { return st != nil && time.Since(st.Updated) < 5*time.Second }

// request le pide al servicio activar ("on") o desactivar ("off") el límite.
func request(what string) {
	_ = os.WriteFile(controlPath(), []byte(what), 0o600)
	setActiveConfig(what == "on")
}

// setLimit cambia el límite (en caliente si está activo) y lo guarda.
func setLimit(n int) {
	n = min(max(n, 1), numCPU())
	setSlotsConfig(n)
	if queueActive() {
		_ = os.WriteFile(limitRequestPath(), []byte(strconv.Itoa(n)), 0o600)
	}
}

// ensureHelper: con el modo de todo el sistema, lanza el ayudante de root cuando el
// límite ya está activo (puede pedir la contraseña de sudo).
func ensureHelper() {
	for i := 0; i < 30 && !queueActive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !queueActive() {
		return
	}
	if c := startRootHelper(); c != nil {
		go func() { _ = c.Wait() }()
		for i := 0; i < 20 && !helperAlive(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// holds: reservas activas de `turn_watch hold` (descripción del comando). Solo se
// leen los argumentos de los procesos que se llaman turn_watch (o rustwatch).
func holds(procs map[int]*proc) []string {
	var out []string
	for _, p := range procs {
		if p.comm != "turn_watch" && p.comm != oldName {
			continue
		}
		args := p.loadArgs()
		if len(args) < 4 || (filepath.Base(args[0]) != "turn_watch" && filepath.Base(args[0]) != "rustwatch") || args[1] != "hold" {
			continue
		}
		cmd := args[4:]
		if len(cmd) > 4 {
			cmd = cmd[:4]
		}
		out = append(out, fmt.Sprintf("%s núcleos · %s", args[2], strings.Join(cmd, " ")))
	}
	sort.Strings(out)
	return out
}

func queueText(st *State) string {
	if st.Slots == 0 {
		return ""
	}
	pool := max(st.Slots-1, 1)
	s := fmt.Sprintf(" · cola %d/%d", max(pool-st.SlotsFree, 0), pool)
	if st.SlotsFree == 0 {
		s += " (llena)"
	}
	return s
}

// version la pone la compilación de la release: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `turn_watch: tus compilaciones de Rust y lo que lanzan los agentes de código, a la
vista y con límite de núcleos. (Antes se llamaba rustwatch; ese nombre sigue
funcionando como enlace.)

El límite lo mantiene un servicio de fondo (systemd de usuario en Linux, launchd en
macOS): sigue activo aunque cierres el panel, hasta que lo desactives.

  turn_watch              abre el panel y activa el límite si estaba apagado:
                          arriba, CPU y RAM (compilaciones / agentes / otros, con
                          la marca del límite); abajo, una fila por compilación y
                          por agente/worktree.
                          ↑↓ (o k/j, rueda) eligen fila, Enter despliega el detalle,
                          +/- cambian el límite, d lo desactiva, a lo activa,
                          s activa el modo de todo el sistema (sudo), t alterna el
                          tope de los agentes; q, Esc o Ctrl-C cierran el panel
                          (el límite sigue).
  turn_watch on | off     activar o desactivar el límite sin abrir el panel
  turn_watch limit [N]    ver o cambiar el límite (en caliente)
  turn_watch sistema on|off  on: también root, Docker y otros usuarios (pide sudo);
                          off: solo tu usuario
  turn_watch once         el estado actual, una vez, en texto plano (con kache:
                          aciertos de 24 h, tiempo ahorrado, almacén y avisos)
  turn_watch hold N -- cmd  reservar N núcleos de la cola para cmd
  turn_watch install      instalar para este usuario: el comando, el envoltorio de
                          cargo, la caché kache (la detecta o baja el binario
                          oficial de GitHub, con versión y SHA-256 fijados), el
                          servicio y, en Omarchy, el indicador de la barra. Migra
                          una instalación de rustwatch.
  turn_watch uninstall    quitar todo lo anterior
  turn_watch version      la versión

Agentes de código (claude, codex, opencode, pi, ante, aider, gemini, cursor-agent, goose;
lista en config.json, "agents"): todo lo que lanzan (tests, builds, servidores,
scripts) se agrupa por proyecto, worktree y rama, con CPU, RAM y los comandos que
más CPU usan. En Linux, con el límite activo y "agents_cap": true (por defecto), el
servicio mete a los agentes y a todos sus descendientes en turn_watch.slice/
turn_watch-agentes.scope: lo que lancen después nace ahí y comparte el tope de
núcleos con las compilaciones. Nada de esto se pausa (la pausa es solo para rustc,
build.rs, cc1). Con "agents_cap": false (o t en el panel) salen a
turn_watch-agentes-libre.scope.

En macOS no hay cgroups: el límite es la cola de núcleos y la pausa de los trabajos
de compilación que sobran; no hay tope de CPU ni para cargo ni para los agentes.

Configuración: ~/.config/turn_watch/config.json (en macOS, ~/Library/Application
Support/turn_watch/config.json); "kache": false apaga la caché y
"kache_in_slice": true (Linux) mete kache.service en turn_watch.slice al instalar.
Variables: TURN_WATCH_SIN_KACHE=1 (install sin kache); TURN_WATCH_KACHE_VERSION=vX.Y.Z
con TURN_WATCH_KACHE_SHA256=<hex> o =release (otra versión de kache, a propósito; si
no, la fijada); se aceptan también las viejas RUSTWATCH_*.
`
