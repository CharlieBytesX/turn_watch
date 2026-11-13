//go:build darwin

package main

// Lectura de procesos y de la máquina en macOS, que no tiene /proc:
//   - la estructura (pid, ppid, uid, nombre, estado, inicio) sale de sysctl
//     kern.proc.all (KinfoProc), dentro del proceso y sin lanzar nada;
//   - la CPU acumulada, la RAM y la %CPU de cada proceso, de UNA llamada a `ps`
//     por lectura (readProcs); la pausa (readProcsLite) no la necesita;
//   - los argumentos exactos, de sysctl kern.procargs2 (solo cuando se piden); si
//     no se pueden leer, los de ps partidos por espacios;
//   - el directorio de trabajo, de `lsof`, que es lento: solo para las raíces
//     (cargo y agentes) y con caché por pid. Los hijos van al grupo de su padre.
//   - No hay CPU de hijos ya terminados (cutime): lo que un proceso gastó entre la
//     última lectura y su muerte no se cuenta.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// hasChildTimes: macOS no da la CPU de los hijos ya recogidos.
const hasChildTimes = false

// Valores de p_stat (sys/proc.h).
const (
	sstop = 4
	szomb = 5
)

var (
	bootOnce sync.Once
	bootAt   time.Time

	cpuMu  sync.Mutex
	psCPU  float64 // suma de %cpu de la última lectura de ps (100 = un núcleo)
	cpuAcc struct {
		busy, total float64
		last        time.Time
	}
)

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// startTicks: inicio del proceso en ticks (clkTck) desde el arranque, como en Linux.
func startTicks(tv unix.Timeval, boot time.Time) uint64 {
	st := time.Unix(int64(tv.Sec), int64(tv.Usec)*1000)
	d := st.Sub(boot)
	if d < 0 {
		return 0
	}
	return uint64(d.Seconds() * clkTck)
}

// kinfoProcs: todos los procesos según sysctl, sin CPU ni RAM.
func kinfoProcs() (map[int]*proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	boot := bootTime()
	out := make(map[int]*proc, len(kps))
	for i := range kps {
		k := &kps[i]
		pid := int(k.Proc.P_pid)
		if pid <= 0 {
			continue // kernel_task
		}
		p := &proc{pid: pid, ppid: int(k.Eproc.Ppid), uid: int(k.Eproc.Ucred.Uid), comm: cstr(k.Proc.P_comm[:])}
		p.start = startTicks(k.Proc.P_starttime, boot)
		p.paused = k.Proc.P_stat == sstop
		p.zombie = k.Proc.P_stat == szomb
		out[pid] = p
	}
	return out, nil
}

// runPS: una lectura de ps para todos los procesos.
func runPS() map[int]psRow {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, tool("/bin/ps", "ps"), "-axww", "-o", psFormat)
	c.Env = append(os.Environ(), "LC_ALL=C")
	b, err := c.Output()
	if err != nil && len(b) == 0 {
		return nil
	}
	return parsePS(string(b))
}

// tool: la ruta fija de una herramienta del sistema o, si no está, la del PATH.
func tool(path, name string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return path
}

// readProcs: todos los procesos, con CPU y RAM.
func readProcs() map[int]*proc {
	rows := runPS()
	out, err := kinfoProcs()
	if err != nil {
		// Sin sysctl (no debería pasar): todo de ps, sin inicio exacto.
		out = map[int]*proc{}
		for pid, r := range rows {
			p := &proc{pid: pid, ppid: r.ppid, uid: r.uid, psArgs: r.args}
			if len(r.args) > 0 {
				p.comm = filepath.Base(r.args[0])
			}
			p.paused = strings.HasPrefix(r.state, "T")
			p.zombie = strings.HasPrefix(r.state, "Z")
			out[pid] = p
		}
	}
	sum := 0.0
	for pid, r := range rows {
		sum += r.pcpu
		if p := out[pid]; p != nil {
			p.own = uint64(r.cpuSecs * clkTck)
			p.cpu = p.own
			p.rssKB = r.rssKB
			p.psArgs = r.args
		}
	}
	cpuMu.Lock()
	psCPU = sum
	cpuMu.Unlock()
	pruneCwd(out)
	return out
}

// readProcsLite: sin CPU ni RAM (para la pausa, que mira 5 veces por segundo).
func readProcsLite() map[int]*proc {
	if out, err := kinfoProcs(); err == nil {
		return out
	}
	return readProcs()
}

// readArgs: argv exacto (kern.procargs2); si no se puede (otro usuario), el de ps.
func readArgs(p *proc) []string {
	if b, err := unix.SysctlRaw("kern.procargs2", p.pid); err == nil {
		if a := parseProcArgs2(b); len(a) > 0 {
			return a
		}
	}
	if len(p.psArgs) > 0 {
		return p.psArgs
	}
	return []string{""}
}

// ---------- cwd (lsof, con caché) ----------

type cwdEntry struct {
	start uint64
	dir   string
	at    time.Time
}

var (
	cwdMu    sync.Mutex
	cwdCache = map[int]cwdEntry{}
)

const cwdTTL = 30 * time.Second

// procCwd: el directorio de trabajo. Solo para raíces (root=true): lsof tarda
// decenas de milisegundos por llamada. Los demás devuelven "" y quedan en el grupo
// de su padre.
func procCwd(p *proc, root bool) string {
	if !root {
		return ""
	}
	cwdMu.Lock()
	e, ok := cwdCache[p.pid]
	cwdMu.Unlock()
	if ok && e.start == p.start && time.Since(e.at) < cwdTTL {
		return e.dir
	}
	dir := lsofCwd(p.pid)
	cwdMu.Lock()
	cwdCache[p.pid] = cwdEntry{start: p.start, dir: dir, at: time.Now()}
	cwdMu.Unlock()
	return dir
}

func lsofCwd(pid int) string {
	if pid == os.Getpid() {
		d, _ := os.Getwd()
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, tool("/usr/sbin/lsof", "lsof"), "-a", "-d", "cwd", "-Fn", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C")
	b, _ := c.Output() // sale con 1 si no encuentra el proceso o no tiene permiso
	return parseLsofCwd(string(b))[pid]
}

// pruneCwd olvida los pids que ya no existen (o que se reciclaron).
func pruneCwd(procs map[int]*proc) {
	cwdMu.Lock()
	defer cwdMu.Unlock()
	for pid, e := range cwdCache {
		if p := procs[pid]; p == nil || p.start != e.start {
			delete(cwdCache, pid)
		}
	}
}

// ---------- la máquina ----------

func bootTime() time.Time {
	bootOnce.Do(func() {
		bootAt = time.Now()
		if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil {
			bootAt = time.Unix(int64(tv.Sec), int64(tv.Usec)*1000)
		}
	})
	return bootAt
}

func loadAvg() float64 {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0
	}
	return parseLoadAvg(b)
}

// cpuTimes: (ocupado, total) en ticks de toda la máquina. macOS no los da sin cgo
// (host_statistics), así que se acumulan con la %CPU de ps de la última lectura: el
// panel solo usa la diferencia entre dos llamadas.
func cpuTimes() (uint64, uint64) {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	now := time.Now()
	n := float64(numCPU())
	if !cpuAcc.last.IsZero() {
		dt := now.Sub(cpuAcc.last).Seconds()
		cpuAcc.busy += min(psCPU/100, n) * dt * clkTck
		cpuAcc.total += n * dt * clkTck
	}
	cpuAcc.last = now
	return uint64(cpuAcc.busy), uint64(cpuAcc.total)
}

func memInfo() (used, total int) {
	tot, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, tool("/usr/bin/vm_stat", "vm_stat")).Output()
	if err != nil {
		return 0, int(tot >> 20)
	}
	u, ok := parseVMStat(string(b))
	if !ok {
		return 0, int(tot >> 20)
	}
	return int(min(u, tot) >> 20), int(tot >> 20)
}

func numCPU() int { return runtime.NumCPU() }

// startOf: inicio del proceso en ticks (o 0 si ya no existe).
func startOf(pid int) uint64 {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return 0
	}
	return startTicks(k.Proc.P_starttime, bootTime())
}

// sample anota los rustc vivos bajo cada build (cada 100 ms, con sysctl: sin ps).
func sample(builds map[string]*Build) {
	if len(builds) == 0 {
		return
	}
	procs, err := kinfoProcs()
	if err != nil {
		return
	}
	children := map[int][]*proc{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	for _, b := range builds {
		stack := []int{b.rootPid}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, c := range children[pid] {
				stack = append(stack, c.pid)
				if c.comm != "rustc" {
					continue
				}
				args := c.loadArgs()
				if cr := argValue(args, "--crate-name"); cr != "" {
					if cr == "build_script_build" {
						cr = "build.rs"
					}
					b.mark(cr + "@" + argValue(args, "--target"))
				}
			}
		}
		b.Done = len(b.seen)
	}
}

// notifyDesktop: aviso del Centro de notificaciones. El texto va como argumento
// del script, nunca dentro del código de AppleScript.
func notifyDesktop(title, body string) {
	_ = exec.Command(tool("/usr/bin/osascript", "osascript"),
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run", title, body).Run()
}
