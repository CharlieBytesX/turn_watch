//go:build linux

package main

// Lectura de procesos y de la máquina en Linux: todo sale de /proc.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func readProcs() map[int]*proc {
	out := map[int]*proc{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		s := string(stat)
		l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if l < 0 || r < l || r+2 > len(s) {
			continue
		}
		f := strings.Fields(s[r+2:])
		if len(f) < 22 {
			continue
		}
		p := &proc{pid: pid, comm: s[l+1 : r], uid: -1}
		if len(f) > 0 && f[0] == "T" {
			p.paused = true
		}
		p.zombie = f[0] == "Z"
		p.ppid, _ = strconv.Atoi(f[1])
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		// cutime/cstime: CPU de los hijos ya terminados y recogidos. Sin esto,
		// cada rustc que termina entre dos lecturas se pierde de la cuenta.
		cut, _ := strconv.ParseUint(f[13], 10, 64)
		cst, _ := strconv.ParseUint(f[14], 10, 64)
		p.own, p.kids = ut+st, cut+cst
		p.cpu = p.own + p.kids
		p.start, _ = strconv.ParseUint(f[19], 10, 64)
		pages, _ := strconv.ParseInt(f[21], 10, 64)
		p.rssKB = pages * int64(os.Getpagesize()) / 1024
		if fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				p.uid = int(st.Uid)
			}
		}
		out[pid] = p
	}
	return out
}

// readArgs: argv del proceso (/proc/<pid>/cmdline).
func readArgs(p *proc) []string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.pid))
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

// procCwd: el directorio de trabajo del proceso ("" si no se puede leer, p. ej. de
// otro usuario). En Linux es barato para cualquier proceso, raíz o no.
func procCwd(p *proc, root bool) string {
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", p.pid))
	return cwd
}

func bootTime() time.Time {
	b, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			n, _ := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
			return time.Unix(n, 0)
		}
	}
	return time.Now()
}

func loadAvg() float64 {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// cpuTimes: (ocupado, total) en ticks de toda la máquina.
func cpuTimes() (uint64, uint64) {
	b, _ := os.ReadFile("/proc/stat")
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	var total, idle uint64
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total - idle, total
}

func memInfo() (used, total int) {
	b, _ := os.ReadFile("/proc/meminfo")
	var tot, avail int
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			tot = n / 1024
		case "MemAvailable:":
			avail = n / 1024
		}
	}
	return tot - avail, tot
}

// sample anota los rustc vivos bajo cada build sin recorrer todo /proc.
func sample(builds map[string]*Build) {
	for _, b := range builds {
		stack := []int{b.rootPid}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
			for _, t := range tasks {
				raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, t.Name()))
				for _, f := range strings.Fields(string(raw)) {
					child, err := strconv.Atoi(f)
					if err != nil {
						continue
					}
					stack = append(stack, child)
					comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", child))
					if strings.TrimSpace(string(comm)) != "rustc" {
						continue
					}
					p := &proc{pid: child}
					args := p.loadArgs()
					if c := argValue(args, "--crate-name"); c != "" {
						if c == "build_script_build" {
							c = "build.rs"
						}
						b.mark(c + "@" + argValue(args, "--target"))
					}
				}
			}
		}
		b.Done = len(b.seen)
	}
}

func numCPU() int {
	b, _ := os.ReadFile("/proc/cpuinfo")
	return strings.Count(string(b), "\nprocessor") + 1
}

// startOf: inicio del proceso en ticks (o 0 si ya no existe).
func startOf(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	r := strings.LastIndexByte(s, ')')
	if r < 0 || r+2 > len(s) {
		return 0
	}
	f := strings.Fields(s[r+2:])
	if len(f) < 20 {
		return 0
	}
	n, _ := strconv.ParseUint(f[19], 10, 64)
	return n
}

func notifyDesktop(title, body string) {
	_ = exec.Command("notify-send", "-a", "turn_watch", "-i", "applications-development", title, body).Run()
}

// hasChildTimes: /proc da cutime/cstime (CPU de los hijos ya recogidos).
const hasChildTimes = true

// readProcsLite: en Linux leer /proc ya es barato: lo mismo que readProcs.
func readProcsLite() map[int]*proc { return readProcs() }
