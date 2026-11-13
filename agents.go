package main

// Agentes de código (claude, codex, opencode…): todo lo que lanzan —tests, builds,
// servidores, scripts— agrupado por proyecto, worktree y rama, con su CPU y RAM.
//
// - Un "agente" es un proceso del usuario cuyo comm o basename de argv[0] está en la
//   lista (config.json: "agents"). Para los que corren con node/bun también se mira
//   argv[1] (p. ej. `node /…/bin/codex`).
// - Cada descendiente va al grupo de SU directorio de trabajo (gitInfo); si no se
//   puede leer (otro usuario), al de su padre.
// - CPU: delta entre lecturas por pid. Lo que un hijo gastó entre la última lectura y
//   su muerte aparece en el cutime del padre al recogerlo; para no contarlo dos veces,
//   al padre se le resta lo que ya se le había contado al hijo.
// - Lo que ya muestra la sección de compilaciones no se repite: los procesos de un
//   cargo cuentan para el grupo, pero salen resumidos como "compilación: …".

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AgentGroup: lo que corre bajo los agentes en un proyecto/worktree/rama.
type AgentGroup struct {
	Project  string   `json:"project"`
	Worktree string   `json:"worktree,omitempty"`
	Dir      string   `json:"dir,omitempty"` // carpeta del repo o worktree
	Branch   string   `json:"branch,omitempty"`
	Agents   []string `json:"agents,omitempty"` // "claude ×2"
	Cores    float64  `json:"cores"`
	MemMB    int      `json:"mem_mb"`
	Procs    int      `json:"procs"`
	Builds   int      `json:"builds,omitempty"` // compilaciones en curso (tienen fila propia)
	// Parte de Cores/MemMB que son esas compilaciones (para no contarlas dos veces).
	BuildCores float64   `json:"build_cores,omitempty"`
	BuildMemMB int       `json:"build_mem_mb,omitempty"`
	Started    time.Time `json:"started"` // el agente más antiguo del grupo
	// Estabilidad: clave fija, cuándo se vio por primera y por última vez, y si
	// ahora no tiene procesos (se sigue mostrando un rato como "inactivo").
	Key       string    `json:"key"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Idle      bool      `json:"idle,omitempty"`
	Top       []TopCmd  `json:"top,omitempty"`
	History   []float64 `json:"history,omitempty"`
}

// Own: núcleos y MB sin contar sus compilaciones.
func (g *AgentGroup) Own() (float64, int) {
	return max(g.Cores-g.BuildCores, 0), max(g.MemMB-g.BuildMemMB, 0)
}

// TopCmd: un comando (o varios iguales) y su CPU.
type TopCmd struct {
	Cmd   string  `json:"cmd"`
	Cores float64 `json:"cores"`
	N     int     `json:"n"`
	MemMB int     `json:"mem_mb"`
}

func (g *AgentGroup) Label() string { return placeLabel(g.Project, g.Worktree, g.Branch) }

type pidCPU struct {
	start     uint64
	own, kids uint64
	ppid      int
}

type gitCache struct {
	project, worktree, branch string
	root                      string // carpeta del repo o worktree
	git                       bool
	at                        time.Time
}

type agentTracker struct {
	names    []string
	last     map[int]pidCPU
	lastTime time.Time
	git      map[string]gitCache
	hist     map[string][]float64
	// Cachés por pid (con inicio y comm, por si el pid se recicla o hace exec): así
	// cada /proc/<pid>/cmdline se lee una vez y no en cada vuelta.
	agentC map[int]procMemo
	labelC map[int]procMemo
	// tree: el último árbol de agentes (agentes y descendientes), para el tope.
	tree []*proc
	// memo: los grupos vistos hace poco, para que la lista no brinque.
	memo map[string]*groupMemo
}

func newAgentTracker() *agentTracker {
	return &agentTracker{names: defaultAgents, last: map[int]pidCPU{}, git: map[string]gitCache{}, hist: map[string][]float64{}, agentC: map[int]procMemo{}, labelC: map[int]procMemo{}, memo: map[string]*groupMemo{}}
}

var interpreters = map[string]bool{
	"node": true, "bun": true, "deno": true, "python": true, "python3": true, "ruby": true, "perl": true,
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "npx": true, "bunx": true, "uv": true, "env": true,
}

// isAgent: el proceso es un agente de código de la lista.
func isAgent(p *proc, names []string) bool {
	args := p.loadArgs()
	cands := []string{p.comm}
	if len(args) > 0 && args[0] != "" {
		b := filepath.Base(strings.Fields(args[0] + " ")[0])
		// claude se re-ejecuta como ugrep/rg (comm "claude", argv[0] "ugrep"): esos
		// no son agentes, solo herramientas suyas.
		if b != p.comm && !interpreters[b] {
			cands = cands[:0]
		}
		cands = append(cands, b)
		if interpreters[b] && len(args) > 1 {
			cands = append(cands, filepath.Base(args[1]))
		}
	}
	for _, c := range cands {
		for _, n := range names {
			if c == n {
				return true
			}
		}
	}
	return false
}

// agentName: el nombre de la lista con el que coincide (para "claude ×2").
func agentName(p *proc, names []string) string {
	args := p.loadArgs()
	for _, n := range names {
		if p.comm == n {
			return n
		}
		for i, a := range args {
			if i > 1 {
				break
			}
			if filepath.Base(a) == n {
				return n
			}
		}
	}
	return p.comm
}

// groupOf: proyecto, worktree y rama de un directorio (con caché de 15 s: gitInfo
// sube por el árbol haciendo stat).
func (t *agentTracker) groupOf(dir string, now time.Time) (project, worktree, branch string, git bool) {
	if c, ok := t.git[dir]; ok && now.Sub(c.at) < 15*time.Second {
		return c.project, c.worktree, c.branch, c.git
	}
	p, w, b := gitInfo(dir)
	git = p != ""
	// Las dependencias que cargo baja de git (~/.cargo/git/checkouts/…) tienen .git,
	// pero no son un proyecto: lo que corre ahí se queda en el grupo de su padre.
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, filepath.Join(h, ".cargo")+"/") {
		git = false
	}
	if !git {
		p = tildePath(dir)
	}
	root := ""
	if git {
		root = gitRoot(dir)
	}
	t.git[dir] = gitCache{p, w, b, root, git, now}
	return p, w, b, git
}

func tildePath(dir string) string {
	if h, err := os.UserHomeDir(); err == nil && (dir == h || strings.HasPrefix(dir, h+"/")) {
		return "~" + strings.TrimPrefix(dir, h)
	}
	return dir
}

// update arma los grupos con lo que hay ahora en /proc.
func (t *agentTracker) update(procs map[int]*proc, children map[int][]*proc, builds map[string]*Build, boot, now time.Time) []*AgentGroup {
	me := os.Getuid()
	isAg := map[int]bool{}
	for _, p := range procs {
		if p.uid == me && t.isAgentCached(p) {
			isAg[p.pid] = true
		}
	}
	// Raíces: agentes sin otro agente por encima.
	var roots []*proc
	for pid := range isAg {
		p := procs[pid]
		nested := false
		for q := procs[p.ppid]; q != nil && q.pid > 1; q = procs[q.ppid] {
			if isAg[q.pid] {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, p)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].pid < roots[j].pid })

	// Procesos de cada compilación en curso: salen resumidos.
	inBuild := map[int]*Build{}
	for _, b := range builds {
		stack := []int{b.rootPid}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, seen := inBuild[pid]; seen {
				continue
			}
			inBuild[pid] = b
			for _, c := range children[pid] {
				stack = append(stack, c.pid)
			}
		}
	}

	type gkey struct{ project, worktree, branch, root string }
	type item struct {
		p     *proc
		key   gkey
		delta float64 // ticks
	}
	var items []*item
	byPid := map[int]*item{}
	type frame struct {
		p      *proc
		parent gkey
	}
	stack := make([]frame, 0, len(roots))
	for _, r := range roots {
		stack = append(stack, frame{p: r})
	}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if byPid[f.p.pid] != nil {
			continue
		}
		key := f.parent
		// Un proceso fuera de un repo (un build.rs en ~/.cargo/registry, algo en /tmp)
		// se queda en el grupo de su padre; solo un cwd de git (o la raíz) abre grupo.
		// En macOS leer el cwd es caro: solo para las raíces (los agentes).
		if cwd := procCwd(f.p, f.parent.project == ""); cwd != "" {
			if pr, w, b, git := t.groupOf(cwd, now); git || key.project == "" {
				key = gkey{pr, w, b, t.git[cwd].root}
			}
		} else if key.project == "" {
			key = gkey{project: "?"}
		}
		it := &item{p: f.p, key: key}
		items = append(items, it)
		byPid[f.p.pid] = it
		for _, c := range children[f.p.pid] {
			stack = append(stack, frame{p: c, parent: key})
		}
	}
	t.tree = t.tree[:0]
	for _, it := range items {
		t.tree = append(t.tree, it.p)
	}

	// CPU: delta por pid.
	secs := now.Sub(t.lastTime).Seconds()
	first := t.lastTime.IsZero()
	lastTicks := uint64(0)
	if !first {
		lastTicks = uint64(t.lastTime.Sub(boot).Seconds() * clkTck)
	}
	for _, it := range items {
		p := it.p
		prev, ok := t.last[p.pid]
		switch {
		case ok && prev.start == p.start:
			if p.own >= prev.own {
				it.delta += float64(p.own - prev.own)
			}
			if p.kids >= prev.kids {
				it.delta += float64(p.kids - prev.kids)
			}
		case !first && p.start >= lastTicks:
			// Nació después de la última lectura: todo lo que gastó es de ahora.
			it.delta = float64(p.own + p.kids)
		}
	}
	// Los que murieron: su total ya contado vuelve en el cutime del padre (solo
	// donde hay cutime: en macOS no, y restarlo dejaría al padre en negativo).
	for pid, prev := range t.last {
		if !hasChildTimes {
			break
		}
		if p := procs[pid]; p != nil && p.start == prev.start {
			continue
		}
		// Si el padre también murió (p. ej. rustc bajo un envoltorio como kache), lo
		// recoge el primer antepasado vivo: se sube por los muertos conocidos.
		ppid := prev.ppid
		for hops := 0; hops < 64; hops++ {
			if par := byPid[ppid]; par != nil {
				par.delta -= float64(prev.own + prev.kids)
				break
			}
			up, ok := t.last[ppid]
			if !ok {
				break
			}
			ppid = up.ppid
		}
	}
	t.last = make(map[int]pidCPU, len(items))
	for _, it := range items {
		t.last[it.p.pid] = pidCPU{start: it.p.start, own: it.p.own, kids: it.p.kids, ppid: it.p.ppid}
	}
	t.lastTime = now

	// Grupos.
	groups := map[gkey]*AgentGroup{}
	tops := map[gkey]map[string]*TopCmd{}
	builds2 := map[gkey]map[*Build]bool{}
	agentsN := map[gkey]map[string]int{}
	for _, it := range items {
		g := groups[it.key]
		if g == nil {
			g = &AgentGroup{Project: it.key.project, Worktree: it.key.worktree, Branch: it.key.branch, Dir: it.key.root}
			groups[it.key] = g
			tops[it.key] = map[string]*TopCmd{}
			builds2[it.key] = map[*Build]bool{}
			agentsN[it.key] = map[string]int{}
		}
		cores := 0.0
		if !first && secs > 0 && it.delta > 0 {
			cores = it.delta / clkTck / secs
		}
		mem := int(it.p.rssKB / 1024)
		g.Cores += cores
		g.MemMB += mem
		g.Procs++
		if isAg[it.p.pid] && !isAg[it.p.ppid] {
			agentsN[it.key][agentName(it.p, t.names)]++
			st := boot.Add(time.Duration(float64(it.p.start) / clkTck * float64(time.Second)))
			if g.Started.IsZero() || st.Before(g.Started) {
				g.Started = st
			}
		}
		if b := inBuild[it.p.pid]; b != nil {
			// Las compilaciones tienen su propia fila: aquí solo se apartan.
			builds2[it.key][b] = true
			g.BuildCores += cores
			g.BuildMemMB += mem
			continue
		}
		label := t.labelCached(it.p)
		tc := tops[it.key][label]
		if tc == nil {
			tc = &TopCmd{Cmd: label}
			tops[it.key][label] = tc
		}
		tc.Cores += cores
		tc.MemMB += mem
		tc.N++
	}
	out := make([]*AgentGroup, 0, len(groups))
	seenHist := map[string]bool{}
	for k, g := range groups {
		g.Builds = len(builds2[k])
		for n, c := range agentsN[k] {
			if c > 1 {
				n += fmt.Sprintf(" ×%d", c)
			}
			g.Agents = append(g.Agents, n)
		}
		sort.Strings(g.Agents)
		var ts []TopCmd
		for _, tc := range tops[k] {
			ts = append(ts, *tc)
		}
		sort.Slice(ts, func(i, j int) bool {
			if ts[i].Cores != ts[j].Cores {
				return ts[i].Cores > ts[j].Cores
			}
			if ts[i].MemMB != ts[j].MemMB {
				return ts[i].MemMB > ts[j].MemMB
			}
			return ts[i].Cmd < ts[j].Cmd
		})
		if len(ts) > 3 {
			ts = ts[:3]
		}
		g.Top = ts
		hk := g.Label()
		own, _ := g.Own()
		t.hist[hk] = appendHist(t.hist[hk], own)
		g.History = t.hist[hk]
		seenHist[hk] = true
		out = append(out, g)
	}
	for k := range t.hist {
		if !seenHist[k] {
			delete(t.hist, k)
		}
	}
	t.prune(procs)
	for d, c := range t.git {
		if now.Sub(c.at) > time.Minute {
			delete(t.git, d)
		}
	}
	return t.stabilize(out, now)
}

// commandLabel: el comando, recortado y legible.
//
//	/…/target/debug/deps/miapp_server-1a2b3c4d5e6f7a8b → miapp_server-1a2b (test)
//	postgres: miapp miapp 127.0.0.1(5432) idle       → postgres
//	node /…/node_modules/.bin/vite --port 5173      → node vite --port 5173
//	bash -c "cargo test -p x"                        → bash -c cargo test -p x
func commandLabel(p *proc) string {
	args := p.loadArgs()
	if len(args) == 0 || args[0] == "" {
		return p.comm
	}
	a0 := args[0]
	// Títulos reescritos (postgres: …, sshd: …): la primera palabra.
	if strings.ContainsAny(a0, " :") && len(args) == 1 {
		w := strings.Fields(a0)
		if len(w) > 0 {
			return strings.TrimSuffix(w[0], ":")
		}
	}
	base := filepath.Base(a0)
	if strings.Contains(a0, "/target/") && filepath.Base(filepath.Dir(a0)) == "deps" {
		// Binario de test de cargo: nombre-<hash de 16> → nombre-<4 del hash>.
		if i := strings.LastIndexByte(base, '-'); i > 0 && len(base)-i-1 == 16 {
			base = base[:i+5]
		}
		return base + " (test)"
	}
	if (base == "sh" || base == "bash" || base == "zsh" || base == "dash" || base == "fish") && len(args) > 2 && args[1] == "-c" {
		return base + ": " + truncate(shellScript(args[2]), 52)
	}
	parts := []string{base}
	n := len(base)
	for _, a := range args[1:] {
		if n > 60 {
			break
		}
		if strings.HasPrefix(a, "/") || strings.HasPrefix(a, "./") || strings.HasPrefix(a, "~/") {
			a = filepath.Base(a)
		}
		a = strings.Join(strings.Fields(a), " ")
		parts = append(parts, a)
		n += len(a) + 1
	}
	return truncate(strings.Join(parts, " "), 56)
}

// shellScript: lo que de verdad corre un `bash -c`. Las herramientas de los agentes
// lo envuelven (Claude Code: "source …snapshot… && eval '<comando>' && pwd -P >| …").
func shellScript(s string) string {
	if i := strings.Index(s, "eval '"); i >= 0 {
		s = s[i+len("eval '"):]
		if j := strings.Index(s, "' && pwd -P"); j >= 0 {
			s = s[:j]
		} else {
			s = strings.TrimSuffix(s, "'")
		}
		s = strings.ReplaceAll(s, `'"'"'`, "'")
	}
	return strings.Join(strings.Fields(s), " ")
}

type procMemo struct {
	start uint64
	comm  string
	agent bool
	label string
}

func (t *agentTracker) isAgentCached(p *proc) bool {
	if m, ok := t.agentC[p.pid]; ok && m.start == p.start && m.comm == p.comm {
		return m.agent
	}
	a := isAgent(p, t.names)
	t.agentC[p.pid] = procMemo{start: p.start, comm: p.comm, agent: a}
	return a
}

func (t *agentTracker) labelCached(p *proc) string {
	if m, ok := t.labelC[p.pid]; ok && m.start == p.start && m.comm == p.comm {
		return m.label
	}
	l := commandLabel(p)
	t.labelC[p.pid] = procMemo{start: p.start, comm: p.comm, label: l}
	return l
}

// prune olvida los pids que ya no existen.
func (t *agentTracker) prune(procs map[int]*proc) {
	for pid := range t.agentC {
		if procs[pid] == nil {
			delete(t.agentC, pid)
		}
	}
	for pid := range t.labelC {
		if procs[pid] == nil {
			delete(t.labelC, pid)
		}
	}
}

// ---------- estabilidad ----------

// Los grupos salen de procesos efímeros (cada bash, cargo o test vive segundos):
// sin memoria, la lista brincaría. Un grupo visto sigue en la lista groupKeep
// desde su último proceso, los números se suavizan (media móvil exponencial de
// ~smoothTau) y el orden es fijo: proyecto y worktree, nunca la CPU del momento.
const (
	groupKeep = 3 * time.Minute
	smoothTau = 3 * time.Second
)

type groupMemo struct {
	g                 AgentGroup
	first, seen, at   time.Time
	cores, buildCores float64
	mem, buildMem     float64
}

// ema: un paso de la media móvil exponencial con constante smoothTau.
func ema(prev, raw float64, dt time.Duration) float64 {
	if dt <= 0 {
		return prev
	}
	a := 1 - math.Exp(-dt.Seconds()/smoothTau.Seconds())
	return prev + a*(raw-prev)
}

func groupKey(project, worktree string) string { return project + "\x00" + worktree }

func (t *agentTracker) stabilize(cur []*AgentGroup, now time.Time) []*AgentGroup {
	// Misma carpeta con otra rama (cambió entre lecturas): un solo grupo.
	merged := map[string]*AgentGroup{}
	var order []string
	for _, g := range cur {
		k := groupKey(g.Project, g.Worktree)
		if o := merged[k]; o != nil {
			o.Cores += g.Cores
			o.BuildCores += g.BuildCores
			o.MemMB += g.MemMB
			o.BuildMemMB += g.BuildMemMB
			o.Procs += g.Procs
			o.Builds += g.Builds
			o.Agents = append(o.Agents, g.Agents...)
			continue
		}
		merged[k] = g
		order = append(order, k)
	}
	seen := map[string]bool{}
	for _, k := range order {
		g := merged[k]
		seen[k] = true
		m := t.memo[k]
		if m == nil {
			m = &groupMemo{first: now, at: now, cores: g.Cores, buildCores: g.BuildCores, mem: float64(g.MemMB), buildMem: float64(g.BuildMemMB)}
			t.memo[k] = m
		} else {
			dt := now.Sub(m.at)
			m.cores = ema(m.cores, g.Cores, dt)
			m.buildCores = ema(m.buildCores, g.BuildCores, dt)
			m.mem = ema(m.mem, float64(g.MemMB), dt)
			m.buildMem = ema(m.buildMem, float64(g.BuildMemMB), dt)
			m.at = now
		}
		m.seen = now
		m.g = *g
	}
	out := make([]*AgentGroup, 0, len(t.memo))
	for k, m := range t.memo {
		if !seen[k] {
			if now.Sub(m.seen) > groupKeep {
				delete(t.memo, k)
				continue
			}
			// Sin procesos ahora: se apaga suave y queda como inactivo.
			dt := now.Sub(m.at)
			m.cores, m.buildCores = ema(m.cores, 0, dt), ema(m.buildCores, 0, dt)
			m.mem, m.buildMem = ema(m.mem, 0, dt), ema(m.buildMem, 0, dt)
			m.at = now
			m.g.Procs, m.g.Builds, m.g.Top, m.g.Agents = 0, 0, nil, nil
			m.g.Idle = true
		} else {
			m.g.Idle = false
		}
		g := m.g
		g.Key = k
		g.Cores, g.BuildCores = round2(m.cores), round2(min(m.buildCores, m.cores))
		g.MemMB, g.BuildMemMB = int(m.mem+0.5), int(min(m.buildMem, m.mem)+0.5)
		g.FirstSeen, g.LastSeen = m.first, m.seen
		out = append(out, &g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Los de un proyecto juntos; dentro, el repo principal y luego los worktrees.
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		// Por lo que se muestra (la rama); el worktree solo desempata.
		if pa, pb := place(a.Worktree, a.Branch), place(b.Worktree, b.Branch); pa != pb {
			return pa < pb
		}
		return a.Worktree < b.Worktree
	})
	return out
}
