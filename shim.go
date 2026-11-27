package main

// Envoltorio de cargo: `turn_watch install` deja ~/.local/bin/cargo como enlace a este
// mismo ejecutable. Llamado como `cargo`, busca el cargo de verdad y:
//   - con el límite activo (cola activa): le pasa el jobserver de turn_watch, le
//     quita el paralelismo explícito (`-j 24`, `--jobs=8`: un `-j` a mano le gana
//     al jobserver y se saltaría la cola); en Linux, además, lo corre dentro de
//     turn_watch.slice (systemd-run --scope), con el tope de CPU; en macOS, con
//     prioridad baja (nice 10);
//   - sin límite: lo corre tal cual.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func isShim() bool { return filepath.Base(os.Args[0]) == "cargo" }

// realCargo: el primer `cargo` del PATH que no sea este ejecutable; si no, el de rustup.
func realCargo() string {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		cand := filepath.Join(dir, "cargo")
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(cand); err == nil && resolved == self {
			continue
		}
		return cand
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cargo", "bin", "cargo")
}

func shim() {
	real := realCargo()
	args := append([]string{real}, os.Args[1:]...)
	env := os.Environ()
	if envVar("IN_QUEUE") == "" {
		if slots, _ := queueStatus(); slots > 0 {
			if sin, quitadas := dropJobFlags(args[1:]); len(quitadas) > 0 {
				args = append([]string{real}, sin...)
				fmt.Fprintln(os.Stderr, "turn_watch (cargo): "+strings.Join(quitadas, " ")+" sobra: la cola reparte los turnos")
			}
			env = append(env,
				"TURN_WATCH_IN_QUEUE=1",
				"CARGO_MAKEFLAGS=-j"+strconv.Itoa(slots)+" --jobserver-auth=fifo:"+jobserverPath(),
			)
			env = without(env, "CARGO_BUILD_JOBS")
			shimLimited(args, env) // Linux: dentro de turn_watch.slice (no vuelve si lo logra)
		}
	}
	must(syscall.Exec(real, args, env))
}

// dropJobFlags: quita de los argumentos de cargo el paralelismo explícito (-j N,
// -jN, --jobs N, --jobs=N) mientras haya cola, para que reparta ella. Un -j puesto
// a mano le gana al jobserver de CARGO_MAKEFLAGS: cada cargo abriría su propio
// paralelismo y entre todos pasarían del límite, que es justo lo que la cola evita.
// Lo que viene después de `--` es del programa que cargo ejecuta, no de cargo, y se
// queda como está. Devuelve los argumentos sin las banderas y lo que se quitó, para
// avisarlo en el log.
func dropJobFlags(args []string) (sin []string, quitadas []string) {
	sin = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(sin, args[i:]...), quitadas
		}
		switch {
		case a == "-j" || a == "--jobs":
			quitada := a
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				quitada += " " + args[i+1]
				i++
			}
			quitadas = append(quitadas, quitada)
		case strings.HasPrefix(a, "-j") && len(a) > 2:
			quitadas = append(quitadas, a)
		case strings.HasPrefix(a, "--jobs="):
			quitadas = append(quitadas, a)
		default:
			sin = append(sin, a)
		}
	}
	return sin, quitadas
}

func without(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if len(kv) > len(key) && kv[:len(key)+1] == key+"=" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "turn_watch (cargo):", err)
		os.Exit(127)
	}
}
