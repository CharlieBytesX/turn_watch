package main

// Cola de compilación: un jobserver de GNU make en una FIFO
// ($XDG_RUNTIME_DIR/turn_watch/jobserver) que existe SOLO mientras el TUI está abierto.
//
// - Cargo entiende el protocolo: con CARGO_MAKEFLAGS="--jobserver-auth=fifo:<ruta>" (lo
//   pone el envoltorio `cargo` de turn_watch) cada rustc toma un byte de la FIFO antes de
//   arrancar y lo devuelve al terminar. Con todas las compilaciones leyendo de la misma
//   FIFO, entre todas no pasan del límite; la que llega cuando no hay turnos, espera.
// - El envoltorio, además, mete cada cargo en turn_watch.slice, al que el TUI le pone
//   CPUQuota = límite × 100 %.
// - Al cerrar el TUI se quita todo: se vacía la cola con turnos de sobra (lo que esperaba
//   sigue), se borra la FIFO y el envoltorio deja de limitar.
// - El límite se cambia en caliente: teclas +/- en el TUI o `turn_watch limit N`.
// - `turn_watch hold N -- cmd` reserva turnos para algo que no habla el protocolo (un
//   `docker build`, que compila dentro de BuildKit) y los devuelve al terminar.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	tokenByte = '+'
)

func runtimeDir() string       { return filepath.Dir(statePath()) }
func jobserverPath() string    { return filepath.Join(runtimeDir(), "jobserver") }
func queueInfoPath() string    { return filepath.Join(runtimeDir(), "queue.json") }
func limitRequestPath() string { return filepath.Join(runtimeDir(), "limit.request") }

// queueInfo: lo que el dueño de la cola publica para la barra y los demás.
type queueInfo struct {
	Slots int `json:"slots"`
	PID   int `json:"pid"`
}

// Queue: la cola mientras el TUI está abierto (su dueño).
type Queue struct {
	f *os.File // FIFO abierta en lectura y escritura: no se cierra mientras viva
	// peek: la misma FIFO solo en lectura, para contar turnos. En macOS, FIONREAD
	// sobre un descriptor de lectura y escritura cuenta el lado de escritura (0).
	peek      *os.File
	lock      *os.File
	slots     int
	idleSince time.Time

	mu         sync.Mutex
	debt       int  // turnos a retirar cuando vuelvan (al bajar el límite)
	collecting bool // hay un lector retirándolos
}

var errQueueOwned = errors.New("otra ventana de turn_watch ya tiene la cola")

// pool: el primer trabajo de cada cargo no usa turno (el "implícito" del protocolo).
func pool(slots int) int { return max(slots-1, 1) }

func startQueue(slots int) (*Queue, error) {
	dir := runtimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, errQueueOwned
	}
	path := jobserverPath()
	_ = os.Remove(path) // de un TUI que murió sin limpiar
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		lock.Close()
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		lock.Close()
		return nil, err
	}
	peek, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		f.Close()
		lock.Close()
		return nil, err
	}
	q := &Queue{f: f, peek: peek, lock: lock, slots: slots, idleSince: time.Now()}
	q.write(pool(slots))
	q.publish()
	setSliceQuota(slots)
	return q, nil
}

func (q *Queue) write(n int) {
	if n <= 0 {
		return
	}
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = tokenByte
	}
	_, _ = q.f.Write(buf)
}

func (q *Queue) Slots() int { return q.slots }

// Free: turnos libres ahora (bytes en la FIFO).
func (q *Queue) Free() int { return fifoAvailable(q.peek) }

// SetSlots cambia el límite en caliente.
func (q *Queue) SetSlots(n int) {
	n = min(max(n, 1), numCPU())
	if n == q.slots {
		return
	}
	delta := pool(n) - pool(q.slots)
	q.mu.Lock()
	if delta > 0 {
		// Primero se cancela lo que se debía retirar.
		pay := min(delta, q.debt)
		q.debt -= pay
		q.write(delta - pay)
	} else {
		q.debt += -delta
		q.collect()
	}
	q.mu.Unlock()
	q.slots = n
	q.publish()
	setSliceQuota(n)
	setSlotsConfig(n)
}

// collect retira los turnos debidos a medida que vuelven. Lee de la FIFO compitiendo
// con los cargo que esperan: si no, un turno devuelto lo toma otro antes que el TUI y
// el límite nuevo no llega nunca. Llamar con q.mu tomado.
func (q *Queue) collect() {
	if q.collecting || q.debt <= 0 {
		return
	}
	r, err := os.OpenFile(jobserverPath(), os.O_RDWR, 0) // bloqueante
	if err != nil {
		return
	}
	q.collecting = true
	go func() {
		defer r.Close()
		b := make([]byte, 1)
		for {
			if _, err := r.Read(b); err != nil {
				q.mu.Lock()
				q.collecting = false
				q.mu.Unlock()
				return
			}
			q.mu.Lock()
			if q.debt > 0 {
				q.debt--
			} else {
				q.write(1) // ya no hacía falta: se devuelve
			}
			done := q.debt == 0
			if done {
				q.collecting = false
			}
			q.mu.Unlock()
			if done {
				return
			}
		}
	}()
}

// Tick: aplica `turn_watch limit` y, sin nada compilando, rellena la cola (los turnos
// que falten se perdieron: un proceso que murió con turnos en la mano).
func (q *Queue) Tick(busy bool, now time.Time) {
	if b, err := os.ReadFile(limitRequestPath()); err == nil {
		_ = os.Remove(limitRequestPath())
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			q.SetSlots(n)
		}
	}
	if busy {
		q.idleSince = now
		return
	}
	if now.Sub(q.idleSince) < 3*time.Second {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.collecting {
		return
	}
	want, have := pool(q.slots), q.Free()
	switch {
	case have < 0:
	case have < want:
		q.write(want - have)
	case have > want:
		_, _ = q.f.Read(make([]byte, have-want))
	}
	q.debt = 0
}

// Stop quita el límite: suelta turnos (lo que esperaba sigue como un cargo normal),
// borra la FIFO para que los cargo nuevos no la usen y quita la cuota de CPU.
func (q *Queue) Stop() {
	_ = os.Remove(jobserverPath())
	_ = os.Remove(queueInfoPath())
	q.mu.Lock()
	q.debt = 0
	// Lo que corre sigue como un cargo normal sin límite: tantos turnos como núcleos.
	q.write(numCPU())
	q.mu.Unlock()
	q.f.Close()
	q.peek.Close()
	setSliceQuota(0)
	q.lock.Close()
}

func (q *Queue) publish() {
	b, _ := json.Marshal(queueInfo{Slots: q.slots, PID: os.Getpid()})
	tmp := queueInfoPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, queueInfoPath())
	}
}

// fifoAvailable: bytes esperando en la FIFO (ioctl FIONREAD; TIOCINQ en Linux).
func fifoAvailable(f *os.File) int {
	n, err := unix.IoctlGetInt(int(f.Fd()), fionread)
	if err != nil {
		return -1
	}
	return n
}

// queueStatus: para quien no es el dueño (barra, daemon). slots 0 = sin límite.
func queueStatus() (slots, free int) {
	// Sin seguir enlaces y solo si la carpeta es del dueño de la cola: el ayudante de
	// root también lo llama, sobre la carpeta del usuario.
	b, err := readFileSafe(runtimeDir(), helperUID, "queue.json", 4096)
	if err != nil {
		return 0, 0
	}
	var qi queueInfo
	if json.Unmarshal(b, &qi) != nil || qi.Slots == 0 || qi.PID <= 1 || syscall.Kill(qi.PID, 0) != nil {
		return 0, 0
	}
	f, err := openFifoSafe(runtimeDir(), helperUID, "jobserver")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return qi.Slots, fifoAvailable(f)
}

// queueActive: hay un TUI con la cola abierta.
func queueActive() bool {
	s, _ := queueStatus()
	return s > 0
}

// limitCmd: `turn_watch limit N` cambia el límite del TUI abierto (y el guardado).
func limitCmd(args []string) {
	if len(args) != 1 {
		cfg := loadConfig()
		if s, free := queueStatus(); s > 0 {
			fmt.Printf("límite activo: %d núcleos (%d turnos libres)\n", s, free)
		} else {
			fmt.Printf("límite desactivado (turn_watch on para activarlo con %d núcleos)\n", cfg.Slots)
		}
		fmt.Println("cambiar: turn_watch limit N")
		return
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		fmt.Fprintln(os.Stderr, "turn_watch limit: N debe ser un número > 0")
		os.Exit(2)
	}
	n = min(n, numCPU())
	setSlotsConfig(n)
	if queueActive() {
		_ = os.WriteFile(limitRequestPath(), []byte(strconv.Itoa(n)), 0o600)
		fmt.Printf("límite: %d núcleos (aplicado ya)\n", n)
	} else {
		fmt.Printf("límite guardado: %d núcleos (se aplica al activarlo)\n", n)
	}
}

// ---------- turn_watch hold ----------

// hold reserva n turnos (esperando si hace falta), corre el comando y los devuelve.
// Sin cola activa corre el comando sin esperar.
func hold(args []string) {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "uso: turn_watch hold N -- comando [args...]")
		os.Exit(2)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		fmt.Fprintln(os.Stderr, "turn_watch hold: N debe ser un número > 0")
		os.Exit(2)
	}
	cmdArgs := args[2:]
	slots, _ := queueStatus()
	if slots == 0 {
		os.Exit(run(cmdArgs, nil))
	}
	n = min(n, pool(slots))
	f, err := os.OpenFile(jobserverPath(), os.O_RDWR, 0)
	if err != nil {
		os.Exit(run(cmdArgs, nil))
	}
	defer f.Close()

	taken := make([]byte, 0, n)
	give := func() {
		if len(taken) > 0 {
			_, _ = f.Write(taken)
			taken = taken[:0]
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		b := make([]byte, 1)
		for len(taken) < n {
			if _, err := f.Read(b); err != nil {
				break
			}
			taken = append(taken, b[0])
		}
		close(done)
	}()
	waiting := time.NewTimer(500 * time.Millisecond)
	select {
	case <-done:
	case <-waiting.C:
		fmt.Fprintf(os.Stderr, "turn_watch: esperando turno en la cola de compilación…\n")
		select {
		case <-done:
		case s := <-sig:
			give()
			os.Exit(128 + int(s.(syscall.Signal)))
		}
	case s := <-sig:
		give()
		os.Exit(128 + int(s.(syscall.Signal)))
	}
	code := run(cmdArgs, sig)
	give()
	os.Exit(code)
}

// run ejecuta el comando reenviándole las señales; devuelve su código de salida.
func run(args []string, sig chan os.Signal) int {
	c := exec.Command(args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "turn_watch:", err)
		return 127
	}
	if sig != nil {
		go func() {
			for s := range sig {
				_ = c.Process.Signal(s)
			}
		}()
	}
	if err := c.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

// controlPath: `turn_watch on|off` y el TUI le piden al servicio activar o desactivar.
func controlPath() string { return filepath.Join(runtimeDir(), "control") }

const helperPidName = "helper.pid"

// helperAlive: el ayudante de root (`sudo turn_watch enforce`) está corriendo. Su
// helper.pid está en la carpeta de root (rootStateDir); el de un ayudante de una
// versión anterior, en la del usuario, cuenta solo si ese pid es un turn_watch de
// root (para no lanzar otro mientras el viejo siga).
func helperAlive() bool {
	if b, err := readFileSafe(rootStateDir(), rootOwner, helperPidName, 64); err == nil && pidAlive(b) {
		return true
	}
	b, err := readFileSafe(runtimeDir(), helperUID, helperPidName, 64)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || !pidAlive(b) {
		return false
	}
	p := readProcsLite()[pid]
	return p != nil && p.uid == rootOwner && (p.comm == "turn_watch" || p.comm == oldName)
}

func pidAlive(b []byte) bool {
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
