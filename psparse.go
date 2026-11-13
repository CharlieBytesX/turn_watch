package main

// Lectores de la salida de herramientas de macOS (ps, lsof, vm_stat, sysctl). Son
// funciones puras, sin build tags, para probarlas en cualquier sistema con salidas de
// ejemplo (psparse_test.go). Las usa proc_darwin.go.

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// psFormat: las columnas que se le piden a ps. args va al final porque es la única que
// puede llevar espacios; todo lo anterior es una palabra.
const psFormat = "pid=,ppid=,uid=,rss=,%cpu=,time=,state=,args="

// psRow: una fila de `ps -axww -o psFormat`.
type psRow struct {
	pid, ppid, uid int
	rssKB          int64
	pcpu           float64 // %cpu (100 = un núcleo entero)
	cpuSecs        float64 // tiempo de CPU acumulado (usuario + sistema)
	state          string
	args           []string // partidos por espacios: aproximados (ver readArgs)
}

// parsePS lee la salida de ps (con LC_ALL=C). Las filas que no se entienden se
// saltan.
func parsePS(out string) map[int]psRow {
	rows := map[int]psRow{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		uid, err3 := strconv.Atoi(f[2])
		rss, err4 := strconv.ParseInt(f[3], 10, 64)
		pcpu, err5 := strconv.ParseFloat(strings.ReplaceAll(f[4], ",", "."), 64)
		secs, ok := parseCPUTime(f[5])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || !ok {
			continue
		}
		r := psRow{pid: pid, ppid: ppid, uid: uid, rssKB: rss, pcpu: pcpu, cpuSecs: secs, state: f[6]}
		args := f[7:]
		// De otro usuario, ps no puede leer los argumentos y muestra "(nombre)".
		if len(args) == 1 && strings.HasPrefix(args[0], "(") && strings.HasSuffix(args[0], ")") {
			args = nil
		}
		r.args = args
		rows[pid] = r
	}
	return rows
}

// parseCPUTime lee el tiempo de CPU de ps: "0:00.03", "12:34.56", "1:02:03.45",
// "2-01:02:03" (días-horas:minutos:segundos). Devuelve segundos.
func parseCPUTime(s string) (float64, bool) {
	s = strings.ReplaceAll(s, ",", ".")
	days := 0.0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = float64(d), s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 {
			return 0, false
		}
		if i < len(parts)-1 && strings.Contains(p, ".") {
			return 0, false
		}
		total = total*60 + v
	}
	return days*86400 + total, true
}

// parseLsofCwd lee `lsof -a -d cwd -Fn -p PID[,PID…]`: líneas "p<pid>", "fcwd",
// "n<ruta>". Devuelve pid → cwd.
func parseLsofCwd(out string) map[int]string {
	res := map[int]string{}
	pid := -1
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			if err != nil {
				pid = -1
				continue
			}
			pid = n
		case 'n':
			if pid >= 0 {
				if _, seen := res[pid]; !seen {
					res[pid] = line[1:]
				}
			}
		}
	}
	return res
}

// parseProcArgs2 lee el resultado de sysctl kern.procargs2.<pid>: argc (int32), la
// ruta del ejecutable terminada en NUL, relleno de NULs y argc argumentos separados
// por NUL (después vienen las variables de entorno, que no se leen).
func parseProcArgs2(b []byte) []string {
	if len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	// La ruta del ejecutable.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil
	}
	rest = rest[i:]
	// El relleno.
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	if argc <= 0 || argc > 1<<16 {
		return nil
	}
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			args = append(args, string(rest))
			break
		}
		args = append(args, string(rest[:j]))
		rest = rest[j+1:]
	}
	return args
}

// parseVMStat lee `vm_stat`: el tamaño de página y las páginas por concepto.
// Devuelve la memoria en uso en bytes como la cuenta el Monitor de Actividad:
// memoria de apps (anónima menos purgable) + fija (wired) + comprimida. En
// versiones sin "Anonymous pages", activa en lugar de la de apps.
func parseVMStat(out string) (used uint64, ok bool) {
	page := uint64(0)
	vals := map[string]uint64{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "page size of") {
			f := strings.Fields(line)
			for i, w := range f {
				if w == "of" && i+1 < len(f) {
					page, _ = strconv.ParseUint(f[i+1], 10, 64)
				}
			}
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil {
			continue
		}
		vals[strings.TrimSpace(k)] = n
	}
	if page == 0 {
		return 0, false
	}
	app := vals["Pages active"]
	if anon, ok := vals["Anonymous pages"]; ok {
		app = anon - min(vals["Pages purgeable"], anon)
	}
	pages := app + vals["Pages wired down"] + vals["Pages occupied by compressor"]
	return pages * page, true
}

// parseLoadAvg lee sysctl vm.loadavg (struct loadavg: fixpt_t ldavg[3]; long
// fscale) y devuelve la carga del último minuto.
func parseLoadAvg(b []byte) float64 {
	if len(b) < 24 {
		return 0
	}
	l1 := binary.LittleEndian.Uint32(b[0:4])
	scale := binary.LittleEndian.Uint64(b[16:24])
	if scale == 0 {
		return 0
	}
	return float64(l1) / float64(scale)
}
