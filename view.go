package main

// Vistas: el panel (TUI), `turn_watch once` (texto plano) y `turn_watch status`
// (tooltip de la barra).
//
// El panel, de arriba abajo:
//   - encabezado de una línea: límite, modo, tope de agentes, cola, hora;
//   - uso actual: CPU, RAM y cola, una línea cada una con una barra de ancho fijo
//     apilada por color (compilaciones / agentes, sin sus compilaciones / otros
//     programas) y, debajo, el reparto en números; sin historia;
//   - lista compacta: una fila por compilación y por agente/worktree (y las
//     terminadas hace poco), con selección y detalle desplegable (Enter);
//   - una línea fija con las teclas.
//
// Solo colores ANSI estándar (0–7): los define el tema de la terminal, así que se
// ven bien en temas claros y oscuros.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	dim     = "\x1b[2m"
	rev     = "\x1b[7m"
	red     = "\x1b[31m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	blue    = "\x1b[34m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
)

// Las tres partes de las gráficas, de abajo arriba.
type category struct {
	name, short string
	fg, bg      string
	plain       string // para `once`, sin color
}

var cats = [3]category{
	{"compilaciones", "compil.", "\x1b[33m", "\x1b[43m", "█"},
	{"agentes", "agentes", "\x1b[35m", "\x1b[45m", "▓"},
	{"otros", "otros", "\x1b[34m", "\x1b[44m", "░"},
}

var eighths = []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// ---------- texto con ancho fijo ----------

// seg: un trozo de texto con su estilo (secuencias SGR).
type seg struct{ st, tx string }

func runeLen(s string) int { return len([]rune(s)) }

func segsLen(ss []seg) int {
	n := 0
	for _, s := range ss {
		n += runeLen(s.tx)
	}
	return n
}

// fitSegs ocupa exactamente w columnas: corta con … si no cabe, rellena si sobra.
// base va delante de cada trozo (p. ej. el vídeo inverso de la fila elegida).
func fitSegs(base string, ss []seg, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	cutting := segsLen(ss) > w
	budget := w
	if cutting {
		budget = w - 1
	}
	used := 0
	lastSt := ""
	for _, s := range ss {
		if budget <= 0 {
			break
		}
		r := []rune(s.tx)
		if len(r) > budget {
			r = r[:budget]
		}
		if len(r) == 0 {
			continue
		}
		budget -= len(r)
		used += len(r)
		b.WriteString(base + s.st + string(r) + reset)
		lastSt = s.st
	}
	if cutting {
		b.WriteString(base + lastSt + "…" + reset)
		used++
	}
	if used < w {
		b.WriteString(base + strings.Repeat(" ", w-used) + reset)
	}
	return b.String()
}

// pick: la primera variante que cabe entera en w; si ninguna, la última cortada.
func pick(w int, variants ...[]seg) []seg {
	for _, v := range variants {
		if segsLen(v) <= w {
			return v
		}
	}
	return variants[len(variants)-1]
}

// cut corta texto plano a n columnas con … (n ≥ 1).
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func truncate(s string, n int) string { return cut(s, max(n, 1)) }

func gb(mbs int) string { return fmt.Sprintf("%.1f", float64(mbs)/1024) }

// ---------- encabezado y pie ----------

func modeText(st *State) string {
	if st.System {
		return "todo el sistema"
	}
	return "solo tu usuario"
}

func headerLine(st *State, cols int) string {
	now := time.Now().Format("15:04:05")
	name := seg{bold, " turn_watch "}
	var long, mid, short []seg
	switch {
	case st == nil:
		long = []seg{name, {bold + red, "⚠ el servicio no corre: " + startHint()}}
		mid = []seg{name, {bold + red, "⚠ servicio parado"}}
		short = mid
	default:
		var lim, limS seg
		if st.LimitOn {
			lim = seg{bold + green, fmt.Sprintf("● límite %d de %d núcleos", st.Slots, st.CPUs)}
			limS = seg{bold + green, fmt.Sprintf("● %d/%d", st.Slots, st.CPUs)}
		} else {
			lim = seg{bold + yellow, fmt.Sprintf("○ límite desactivado (%d al activar)", loadConfig().Slots)}
			limS = seg{bold + yellow, "○ sin límite"}
		}
		tope, topeS := seg{yellow, "no"}, seg{yellow, "no"}
		if st.LimitOn && st.AgentsCap {
			tope = seg{green, fmt.Sprintf("sí (%d procesos)", st.AgentsCapped)}
			topeS = seg{green, "sí"}
		}
		if st.NoCap {
			tope = seg{dim, "no (en macOS el tope es por cola y pausa; no hay cgroups)"}
			topeS = seg{dim, "no (macOS)"}
		}
		sep := seg{dim, " · "}
		long = []seg{name, lim, sep, {"", modeText(st)}, sep, {"", "tope agentes: "}, tope}
		mid = []seg{name, limS, sep, {"", map[bool]string{true: "sistema", false: "usuario"}[st.System]}, sep, {"", "tope: "}, topeS}
		short = []seg{name, limS}
		if st.Slots > 0 {
			p := pool(st.Slots)
			q := seg{"", fmt.Sprintf("cola %d/%d", max(p-st.SlotsFree, 0), p)}
			if st.SlotsFree == 0 {
				q = seg{yellow, fmt.Sprintf("cola llena %d/%d", p, p)}
			}
			long = append(long, sep, q)
			mid = append(mid, sep, q)
		}
		if !daemonUp(st) {
			warn := seg{bold + red, fmt.Sprintf("⚠ servicio sin datos hace %s", dur(time.Since(st.Updated)))}
			long = append([]seg{name, warn, sep}, long[1:]...)
			mid = []seg{name, warn}
			short = mid
		}
	}
	right := []seg{{dim, now + " "}}
	for _, v := range [][]seg{long, mid, short} {
		if segsLen(v)+segsLen(right)+1 <= cols {
			return fitSegs("", v, cols-segsLen(right)) + fitSegs("", right, segsLen(right))
		}
	}
	return fitSegs("", short, cols)
}

func footLine(cols int) string {
	k := func(s string) seg { return seg{bold, s} }
	t := func(s string) seg { return seg{dim, s} }
	return fitSegs("", pick(cols,
		[]seg{t(" "), k("q"), t(" salir · "), k("+/-"), t(" límite · "), k("d"), t("/"), k("a"), t(" desactivar/activar · "), k("s"), t(" sistema · "), k("t"), t(" tope agentes · "), k("↑↓"), t(" elegir · "), k("Enter"), t(" detalle")},
		[]seg{t(" "), k("q"), t(" salir · "), k("+/-"), t(" límite · "), k("d/a"), t(" · "), k("s"), t(" sistema · "), k("t"), t(" tope agentes · "), k("↑↓ Enter")},
		[]seg{t(" "), k("q"), t(" salir "), k("+/-"), t(" límite "), k("d/a s t"), t(" "), k("↑↓ Enter")},
	), cols)
}

// ---------- uso actual ----------

// usageBar: barra horizontal de ancho fijo con las tres partes apiladas por color;
// mark (>0) marca con una raya roja una posición (el límite) si cae en lo vacío.
func usageBar(v [3]float64, maxV float64, w int, mark float64) string {
	if maxV <= 0 {
		maxV = 1
	}
	cellsOf := func(x float64) int { return min(max(int(x/maxV*float64(w)+0.5), 0), w) }
	var b strings.Builder
	used, acc := 0, 0.0
	for i := 0; i < 3; i++ {
		acc += max(v[i], 0)
		if n := cellsOf(acc) - used; n > 0 {
			b.WriteString(cats[i].fg + strings.Repeat("█", n) + reset)
			used += n
		}
	}
	m := -1
	if mark > 0 && mark < maxV {
		m = cellsOf(mark)
	}
	for c := used; c < w; c++ {
		if c == m {
			b.WriteString(red + "│" + reset)
		} else {
			b.WriteString(dim + "░" + reset)
		}
	}
	return b.String()
}

// usageBlock: CPU, RAM y cola, una línea cada una (más el reparto debajo si full).
//
//	CPU   ███████████████░░│░  11.6 / 12 núcleos   límite 11
//	      ■ compilaciones 9.8 · ■ agentes 0.2 · ■ otros 1.6
func usageBlock(st *State, cols int, full bool) []string {
	const lw = 7 // " CPU   "
	bw := min(max(cols-lw-34, 8), 40)
	u := st.Use
	label := func(s string) string { return fitSegs("", []seg{{bold, " " + s}}, lw) }
	sw := func(i int) seg { return seg{cats[i].fg, "■ "} }
	split := func(a, b, c string) string {
		return fitSegs("", []seg{{"", strings.Repeat(" ", lw)}, sw(0), {"", "compilaciones " + a}, {dim, " · "},
			sw(1), {"", "agentes " + b}, {dim, " · "}, sw(2), {"", "otros " + c}}, cols)
	}
	var out []string
	cpuTxt := []seg{{"", "  "}, {bold, fmt.Sprintf("%.1f", u.Cores())}, {"", fmt.Sprintf(" / %d núcleos", st.CPUs)}}
	if st.LimitOn {
		cpuTxt = append(cpuTxt, seg{red, fmt.Sprintf("   límite %d", st.Slots)})
	}
	limit := 0.0
	if st.LimitOn {
		limit = float64(st.Slots)
	}
	out = append(out, label("CPU")+usageBar([3]float64{u.B, u.A, u.O}, float64(st.CPUs), bw, limit)+fitSegs("", cpuTxt, cols-lw-bw))
	if full {
		out = append(out, split(fmt.Sprintf("%.1f", u.B), fmt.Sprintf("%.1f", u.A), fmt.Sprintf("%.1f", u.O)))
	}
	memTxt := []seg{{"", "  "}, {bold, gb(st.MemUsedMB)}, {"", " / " + gb(st.MemTotMB) + " GB"}}
	out = append(out, label("RAM")+usageBar([3]float64{float64(u.BM), float64(u.AM), float64(u.OM)}, float64(st.MemTotMB), bw, 0)+fitSegs("", memTxt, cols-lw-bw))
	if full {
		out = append(out, split(gb(u.BM)+" GB", gb(u.AM)+" GB", gb(u.OM)+" GB"))
	}
	if st.Slots > 0 {
		p := pool(st.Slots)
		inUse := max(p-st.SlotsFree, 0)
		var bar strings.Builder
		n := min(int(float64(inUse)/float64(p)*float64(bw)+0.5), bw)
		bar.WriteString(cats[0].fg + strings.Repeat("█", n) + reset + dim + strings.Repeat("░", bw-n) + reset)
		txt := []seg{{"", "  "}, {bold, fmt.Sprintf("%d/%d", inUse, p)}, {"", " turnos"}}
		if w := waiting(st); w > 0 {
			txt = append(txt, seg{yellow, fmt.Sprintf(" · esperando: %d", w)})
		} else if st.SlotsFree == 0 {
			txt = append(txt, seg{yellow, " · llena"})
		}
		out = append(out, label("Cola")+bar.String()+fitSegs("", txt, cols-lw-bw))
		if full {
			for _, h := range st.Holds {
				out = append(out, fitSegs("", []seg{{"", strings.Repeat(" ", lw)}, {dim, "reservado: " + h}}, cols))
			}
		}
	}
	return out
}

// waiting: trabajos de compilación en pausa esperando turno.
func waiting(st *State) int {
	n := 0
	for _, b := range st.Active {
		n += b.Paused
	}
	return n
}

// ---------- la lista ----------

const (
	kindBuild = iota
	kindAgent
	kindDone
)

// item: una fila de la lista (y su detalle al desplegarla).
type item struct {
	key    string
	kind   int
	name   []seg // proyecto ⎇ worktree (rama)
	what   string
	cores  float64 // −1: no aplica
	memMB  int
	age    time.Duration
	detail []string
	sub    string // detalle compacto (una línea) para cuando está plegada
}

func nameSegs(project, worktree, branch string) []seg {
	ss := []seg{{bold, project}}
	if p := place(worktree, branch); p != "" {
		ss = append(ss, seg{cyan, " ⎇ " + p})
	}
	return ss
}

func buildPhase(st *State, b *Build) string {
	if st.Slots > 0 && st.SlotsFree == 0 && len(b.Activity) == 0 && b.Cores < 0.3 {
		return "en cola: esperando turno"
	}
	return b.Phase
}

func items(st *State) []item {
	if st == nil {
		return nil
	}
	var out []item
	for _, b := range st.Active {
		what := buildPhase(st, b)
		if b.What != "" {
			what += " · " + b.What
		}
		d := []string{progressText(b)}
		if b.Total > 0 {
			d[0] += fmt.Sprintf(" · el proyecto tiene ~%d (lo que está en caché no se recompila)", b.Total)
		}
		d = append(d, fmt.Sprintf("pico %.1f núcleos · %s (pico %s) · %d procesos%s", b.PeakCores, mb(b.MemMB), mb(b.PeakMemMB), b.Procs,
			map[bool]string{true: " · dentro de Docker", false: ""}[b.Docker]))
		d = append(d, b.Activity...)
		if b.Dir != "" {
			d = append(d, "carpeta: "+tildePath(b.Dir))
		}
		sub := strings.Join(b.Activity, " · ")
		if sub == "" {
			sub = progressText(b)
		}
		out = append(out, item{key: "b" + b.Key, kind: kindBuild, name: nameSegs(b.Project, b.Worktree, b.Branch),
			what: what, cores: b.Cores, memMB: b.MemMB, age: time.Since(b.Started), detail: d, sub: sub})
	}
	// Orden del servicio (proyecto y worktree, fijo) y siempre las mismas filas: un
	// grupo sin procesos sigue unos minutos como "inactivo" en vez de desaparecer.
	for _, g := range st.Agents {
		cores, mem := g.Own()
		var parts []string
		if len(g.Agents) > 0 {
			parts = append(parts, strings.Join(g.Agents, ", "))
		}
		switch {
		case g.Idle:
			parts = append(parts, "inactivo · hace "+dur(time.Since(g.LastSeen)))
		case g.Builds > 0:
			parts = append(parts, "compilando (ver arriba)")
		default:
			// Un comando solo si de verdad trabaja (si no, cambiaría cada segundo).
			for _, tc := range g.Top {
				if tc.Cores >= 0.5 && !isAgentLabel(tc.Cmd, g.Agents) {
					parts = append(parts, tc.Cmd)
					break
				}
			}
		}
		what := strings.Join(parts, " · ")
		if what == "" {
			what = "en espera"
		}
		var d []string
		line := fmt.Sprintf("%d procesos", g.Procs)
		if g.Builds > 0 {
			s := "compilación"
			if g.Builds > 1 {
				s = "compilaciones"
			}
			line += fmt.Sprintf(" · además %d %s (fila propia): %.1f núcleos, %s", g.Builds, s, g.BuildCores, mb(g.BuildMemMB))
		}
		d = append(d, line)
		if g.Dir != "" {
			d = append(d, "carpeta: "+tildePath(g.Dir))
		}
		for _, tc := range g.Top {
			cmd := tc.Cmd
			if tc.N > 1 {
				cmd += fmt.Sprintf(" ×%d", tc.N)
			}
			d = append(d, fmt.Sprintf("%4.1f núcleos %8s  %s", tc.Cores, mb(tc.MemMB), cmd))
		}
		// Tiempo: desde el agente más antiguo o, si no hay, desde que se vio el grupo.
		age := time.Duration(0)
		switch {
		case !g.Started.IsZero():
			age = time.Since(g.Started)
		case !g.FirstSeen.IsZero():
			age = time.Since(g.FirstSeen)
		}
		key := g.Key
		if key == "" {
			key = g.Label()
		}
		out = append(out, item{key: "a" + key, kind: kindAgent, name: nameSegs(g.Project, g.Worktree, g.Branch),
			what: what, cores: cores, memMB: mem, age: age, detail: d})
	}
	for i, b := range st.Recent {
		if i == 5 {
			break
		}
		what := fmt.Sprintf("terminó hace %s", dur(time.Since(b.Ended)))
		if b.What != "" {
			what += " · " + b.What
		}
		out = append(out, item{key: "d" + b.Key, kind: kindDone, name: nameSegs(b.Project, b.Worktree, b.Branch),
			what: what, cores: -1, memMB: -1, age: b.Ended.Sub(b.Started),
			detail: []string{fmt.Sprintf("duró %s · %d crates · pico %.1f núcleos · pico %s", dur(b.Ended.Sub(b.Started)), b.Done, b.PeakCores, mb(b.PeakMemMB))}})
	}
	return out
}

// isAgentLabel: el comando es el propio agente (no dice nada nuevo).
func isAgentLabel(cmd string, agents []string) bool {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return true
	}
	for _, a := range agents {
		if f[0] == strings.Fields(a)[0] {
			return true
		}
	}
	return false
}

// columnas de la lista según el ancho.
type cols struct {
	name, what, cores, mem, age int
}

func layoutCols(w int) cols {
	c := cols{cores: 12, mem: 8, age: 7}
	// marcas (4) + separadores
	rest := w - 4 - c.cores - c.mem - c.age - 4
	// En terminales angostas se va primero el tiempo: que la rama quepa entera.
	if rest < 40 || w < 90 {
		c.age = 0
		rest = w - 4 - c.cores - c.mem - 3
	}
	if rest < 26 {
		c.what = 0
		c.name = max(w-4-c.cores-c.mem-2, 8)
		return c
	}
	c.name = min(max(rest*11/20, min(rest*2/3, 36)), 48)
	c.what = min(rest-c.name, 60)
	return c
}

func coresCell(v float64, cpus int, color string, w int, base string) string {
	if v < 0 {
		return fitSegs(base, nil, w)
	}
	bw := w - 5
	frac := min(max(v/float64(max(cpus, 1)), 0), 1)
	full := frac * float64(bw)
	n := int(full)
	part := ""
	if n < bw {
		part = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}[int((full-float64(n))*8)]
	}
	barTxt := strings.Repeat("█", n) + part
	return fitSegs(base, []seg{{"", fmt.Sprintf("%4.1f ", v)}, {color, barTxt}, {dim, strings.Repeat("·", bw-runeLen(barTxt))}}, w)
}

func rowLine(it item, c cols, cpus int, selected, open bool) string {
	base := ""
	if selected {
		base = rev
	}
	mark := "▸ "
	if open {
		mark = "▾ "
	}
	if it.detail == nil {
		mark = "  "
	}
	var sw seg
	switch it.kind {
	case kindBuild:
		sw = seg{cats[0].fg, "■ "}
	case kindAgent:
		sw = seg{cats[1].fg, "■ "}
	default:
		sw = seg{green, "✓ "}
	}
	var b strings.Builder
	b.WriteString(fitSegs(base, []seg{{dim, mark}, sw}, 4))
	name := it.name
	if it.kind == kindDone {
		name = []seg{{dim, plainSegs(it.name)}}
	}
	b.WriteString(fitSegs(base, name, c.name))
	b.WriteString(fitSegs(base, nil, 1))
	if c.what > 0 {
		st := ""
		if it.kind == kindDone {
			st = dim
		}
		b.WriteString(fitSegs(base, []seg{{st, it.what}}, c.what))
		b.WriteString(fitSegs(base, nil, 1))
	}
	color := cats[0].fg
	if it.kind == kindAgent {
		color = cats[1].fg
	}
	b.WriteString(coresCell(it.cores, cpus, color, c.cores, base))
	b.WriteString(fitSegs(base, nil, 1))
	m := ""
	if it.memMB >= 0 {
		m = mb(it.memMB)
	}
	b.WriteString(fitSegs(base, []seg{{"", fmt.Sprintf("%*s", c.mem, m)}}, c.mem))
	if c.age > 0 {
		a := ""
		if it.age > 0 {
			a = dur(it.age)
		}
		b.WriteString(fitSegs(base, []seg{{dim, fmt.Sprintf("%*s", c.age, a)}}, c.age))
	}
	return b.String()
}

func plainSegs(ss []seg) string {
	var b strings.Builder
	for _, s := range ss {
		b.WriteString(s.tx)
	}
	return b.String()
}

func listHeader(c cols, w int, above, below int) string {
	ss := []seg{{dim, "    "}, {dim, fmt.Sprintf("%-*s ", c.name, cut("PROYECTO ⎇ RAMA", c.name))}}
	if c.what > 0 {
		ss = append(ss, seg{dim, fmt.Sprintf("%-*s ", c.what, cut("QUÉ HACE", c.what))})
	}
	ss = append(ss, seg{dim, fmt.Sprintf("%-*s ", c.cores, "NÚCLEOS")}, seg{dim, fmt.Sprintf("%*s", c.mem, "RAM")})
	if c.age > 0 {
		ss = append(ss, seg{dim, fmt.Sprintf("%*s", c.age, "TIEMPO")})
	}
	line := fitSegs("", ss, w)
	if above > 0 || below > 0 {
		var hint []string
		if above > 0 {
			hint = append(hint, fmt.Sprintf("↑%d", above))
		}
		if below > 0 {
			hint = append(hint, fmt.Sprintf("↓%d", below))
		}
		h := " " + strings.Join(hint, " ") + " "
		// La pista va sobre la cabecera, a la derecha.
		line = fitSegs("", ss, w-runeLen(h)) + fitSegs("", []seg{{bold + yellow, h}}, runeLen(h))
	}
	return line
}

// ---------- el panel ----------

// ui: lo que el panel recuerda entre cuadros.
type ui struct {
	sel    string          // clave de la fila elegida
	selIdx int             // su posición (por si desaparece)
	open   map[string]bool // filas desplegadas
	off    int             // primera línea visible de la lista
	keys   []string        // filas del último cuadro, en orden
	page   int             // alto de la lista
}

func newUI() *ui { return &ui{open: map[string]bool{}} }

func (u *ui) move(d int) {
	if len(u.keys) == 0 {
		return
	}
	i := min(max(u.selIdx+d, 0), len(u.keys)-1)
	u.selIdx, u.sel = i, u.keys[i]
}

func (u *ui) toggle() {
	if u.sel != "" {
		u.open[u.sel] = !u.open[u.sel]
	}
}

// panel arma la pantalla entera: exactamente rows líneas de cols columnas.
func panel(st *State, u *ui, rows, cols int) []string {
	out := make([]string, 0, rows)
	out = append(out, headerLine(st, cols))
	if rows <= 2 {
		return append(out, footLine(cols))[:rows]
	}
	avail := rows - 2
	its := items(st)

	// Selección: por clave; si la fila desapareció, la que quedó en su lugar.
	u.keys = u.keys[:0]
	for _, it := range its {
		u.keys = append(u.keys, it.key)
	}
	found := false
	for i, k := range u.keys {
		if k == u.sel {
			u.selIdx, found = i, true
		}
	}
	if !found && len(u.keys) > 0 {
		u.selIdx = min(max(u.selIdx, 0), len(u.keys)-1)
		u.sel = u.keys[u.selIdx]
	}
	for k := range u.open {
		if !contains(u.keys, k) {
			delete(u.open, k)
		}
	}

	// Líneas de la lista: un título por grupo, cada fila y, debajo, su detalle
	// (desplegada: todo; plegada: una línea compacta si la tiene).
	lc := layoutCols(cols)
	var lines []string
	start := make([]int, len(its)+1)
	cpus := 1
	if st != nil {
		cpus = st.CPUs
	}
	titles := map[int]string{kindBuild: "Compilaciones", kindAgent: "Agentes", kindDone: "Terminadas"}
	count := map[int]int{}
	for _, it := range its {
		count[it.kind]++
	}
	section := -1
	for i, it := range its {
		if it.kind != section {
			section = it.kind
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, fitSegs("", []seg{{bold, " " + titles[section]}, {dim, fmt.Sprintf(" (%d)", count[section])}}, cols))
		}
		start[i] = len(lines)
		open := u.open[it.key]
		lines = append(lines, rowLine(it, lc, cpus, i == u.selIdx, open))
		switch {
		case open:
			for _, d := range it.detail {
				lines = append(lines, fitSegs("", []seg{{"", "      "}, {dim, d}}, cols))
			}
		case it.sub != "":
			lines = append(lines, fitSegs("", []seg{{"", "      "}, {dim, it.sub}}, cols))
		}
	}
	start[len(its)] = len(lines)
	if len(its) == 0 {
		msg := "Sin compilaciones ni agentes en marcha."
		if st == nil {
			msg = "Sin datos del servicio."
		}
		lines = []string{fitSegs("", []seg{{dim, "    " + msg}}, cols)}
	}

	// Uso actual: con el reparto debajo si queda sitio para al menos 6 líneas de
	// lista; si no, una línea por cosa; si ni eso, nada.
	var top []string
	if st != nil {
		top = usageBlock(st, cols, true)
		if avail-len(top)-2 < min(len(lines), 6) {
			top = usageBlock(st, cols, false)
		}
		if avail-len(top)-2 < min(len(lines), 3) {
			top = nil
		}
	}
	if len(top) > 0 {
		out = append(out, "")
		out = append(out, top...)
	}
	out = append(out, "")

	// La lista, con desplazamiento para que la fila elegida (y su detalle) se vea.
	room := rows - 1 - len(out)
	if room >= 1 {
		page := room - 1
		u.page = max(page, 1)
		if len(its) > 0 && page > 0 {
			a, b := start[u.selIdx], start[u.selIdx+1]
			// El título de su grupo, si está justo encima, también.
			if a > 0 && (u.selIdx == 0 || its[u.selIdx-1].kind != its[u.selIdx].kind) {
				a--
			}
			if b-a > page {
				b = a + page
			}
			if a < u.off {
				u.off = a
			}
			if b > u.off+page {
				u.off = b - page
			}
		}
		u.off = min(max(u.off, 0), max(len(lines)-page, 0))
		end := min(u.off+page, len(lines))
		out = append(out, listHeader(lc, cols, u.off, len(lines)-end))
		if page > 0 {
			out = append(out, lines[u.off:end]...)
		}
	}
	for len(out) < rows-1 {
		out = append(out, "")
	}
	return append(out[:rows-1], footLine(cols))
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- `turn_watch once` ----------

// renderPlain: el estado en texto plano (sin colores), para leer o pegar.
func renderPlain(st *State, width int) string {
	if st == nil {
		return "turn_watch: el servicio no está corriendo (" + startHint() + ")\n"
	}
	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }
	w("turn_watch · %s\n", time.Now().Format("15:04:05"))
	if !daemonUp(st) {
		w("⚠ el servicio no actualiza desde hace %s\n", dur(time.Since(st.Updated)))
	}
	if st.LimitOn {
		w("Límite activo: %d de %d núcleos (%s)", st.Slots, st.CPUs, modeText(st))
		if st.Slots > 0 {
			w(" · cola %d/%d", max(pool(st.Slots)-st.SlotsFree, 0), pool(st.Slots))
		}
		if st.NoCap {
			w(" · tope de agentes: no\n")
			w("En macOS el tope es por cola y pausa; no hay cgroups (ni tope de CPU).\n")
		} else {
			w(" · tope de agentes: %s\n", siNo(st.AgentsCap))
		}
	} else {
		w("Límite desactivado (%d núcleos al activarlo) · tope de agentes: no\n", loadConfig().Slots)
	}
	for _, h := range st.Holds {
		w("  reservado: %s\n", h)
	}
	u := st.Use
	bw := min(max(width-12, 10), 50)
	pbar := func(v [3]float64, maxV float64) string {
		var b strings.Builder
		used, acc := 0, 0.0
		for i := 0; i < 3; i++ {
			acc += max(v[i], 0)
			n := min(int(acc/max(maxV, 1)*float64(bw)+0.5), bw) - used
			if n > 0 {
				b.WriteString(strings.Repeat(cats[i].plain, n))
				used += n
			}
		}
		return b.String() + strings.Repeat("·", bw-used)
	}
	w("\nCPU  [%s] %.1f de %d núcleos\n", pbar([3]float64{u.B, u.A, u.O}, float64(st.CPUs)), u.Cores(), st.CPUs)
	w("     compilaciones %.1f · agentes %.1f · otros %.1f\n", u.B, u.A, u.O)
	w("RAM  [%s] %s de %s GB\n", pbar([3]float64{float64(u.BM), float64(u.AM), float64(u.OM)}, float64(st.MemTotMB)), gb(st.MemUsedMB), gb(st.MemTotMB))
	w("     compilaciones %s · agentes %s · otros %s GB\n", gb(u.BM), gb(u.AM), gb(u.OM))
	w("     (%s compilaciones · %s agentes · %s otros)\n", cats[0].plain, cats[1].plain, cats[2].plain)

	its := items(st)
	section := -1
	titles := map[int]string{kindBuild: "Compilaciones", kindAgent: "Agentes", kindDone: "Terminadas"}
	for _, it := range its {
		if it.kind != section {
			section = it.kind
			w("\n%s\n", titles[section])
		}
		line := "  " + plainSegs(it.name) + " — " + it.what
		if it.cores >= 0 {
			line += fmt.Sprintf(" · %.1f núcleos · %s", it.cores, mb(it.memMB))
		}
		if it.age > 0 && it.kind != kindDone {
			line += " · " + dur(it.age)
		}
		w("%s\n", cut(line, width))
		for _, d := range it.detail {
			w("      %s\n", cut(d, width-6))
		}
	}
	if len(its) == 0 {
		w("\nSin compilaciones ni agentes en marcha.\n")
	}
	return sb.String()
}

// ---------- `turn_watch status` (la barra) ----------

func tooltip(st *State) string {
	var lines []string
	if st.LimitOn {
		lines = append(lines, fmt.Sprintf("turn_watch · límite %d de %d núcleos (%s)", st.Slots, st.CPUs, modeText(st)))
	} else {
		lines = append(lines, "turn_watch · límite desactivado")
	}
	u := st.Use
	lines = append(lines,
		fmt.Sprintf("CPU %.1f/%d: compilaciones %.1f · agentes %.1f · otros %.1f", u.Cores(), st.CPUs, u.B, u.A, u.O),
		fmt.Sprintf("RAM %s/%s GB: compilaciones %s · agentes %s · otros %s", gb(st.MemUsedMB), gb(st.MemTotMB), gb(u.BM), gb(u.AM), gb(u.OM)))
	if len(st.Active) > 0 {
		lines = append(lines, "", fmt.Sprintf("Compilando (%d)%s:", len(st.Active), queueText(st)))
		for _, b := range st.Active {
			lines = append(lines, fmt.Sprintf("  %s — %s · %.1f núcl. · %s", b.Label(), buildPhase(st, b), b.Cores, dur(time.Since(b.Started))))
		}
	}
	if len(st.Agents) > 0 {
		tope := "sin tope"
		if st.LimitOn && st.AgentsCap {
			tope = "con tope"
		}
		lines = append(lines, "", fmt.Sprintf("Agentes (%s):", tope))
		// Las mismas filas y el mismo orden que el panel.
		shown := 0
		for _, it := range items(st) {
			if it.kind != kindAgent {
				continue
			}
			if shown++; shown > 6 {
				lines = append(lines, fmt.Sprintf("  … y %d más", len(st.Agents)-6))
				break
			}
			lines = append(lines, fmt.Sprintf("  %s — %s · %.1f núcl. · %s", plainSegs(it.name), it.what, it.cores, mb(it.memMB)))
		}
	}
	if len(st.Active) == 0 && len(st.Recent) > 0 {
		b := st.Recent[0]
		lines = append(lines, "", fmt.Sprintf("Última: %s — %s, hace %s", b.Label(), dur(b.Ended.Sub(b.Started)), dur(time.Since(b.Ended))))
	}
	lines = append(lines, "", "Clic: abre el panel")
	return strings.Join(lines, "\n")
}

// status: una línea JSON para la barra.
func status() {
	st := readState()
	out := map[string]any{"active": 0, "text": "", "tooltip": "turn_watch: el servicio no corre"}
	if st != nil && time.Since(st.Updated) < 10*time.Second {
		var cores float64
		for _, b := range st.Active {
			cores += b.Cores
		}
		out["active"] = len(st.Active)
		out["text"] = fmt.Sprint(len(st.Active))
		out["cores"] = cores
		out["tooltip"] = tooltip(st)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
