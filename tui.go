package main

// El panel (TUI): terminal en modo raw, lectura de teclas con un parser de
// secuencias de escape, desplazamiento y dibujo línea a línea.
//
// Antes se leían bytes sueltos y cada uno costaba un redibujo más 150 ms de
// espera: la rueda del ratón (que foot manda como flechas en la pantalla
// alternativa, 3 bytes por línea) dejaba cientos de bytes en cola y la `q`
// tardaba segundos o minutos en llegar. Además la `A` de la flecha arriba
// activaba el límite y la `D` de la flecha izquierda lo desactivaba.

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ---------- terminal ----------

// Los ioctl de la terminal cambian de nombre según el sistema (tty_linux.go,
// tty_darwin.go): ioctlGetTermios, ioctlSetTermios y ioctlSetTermiosFlush (como
// el anterior, pero antes descarta la entrada pendiente).

// tty: /dev/tty abierto por nosotros. Así Go lo registra en su poller y se le
// pueden poner plazos de lectura (para pausar la lectura mientras sudo pide la
// contraseña) y de escritura (para que una terminal que no lee no nos bloquee).
type tty struct {
	f     *os.File
	rc    syscall.RawConn // para los ioctl sin pasar por f.Fd() (que lo vuelve bloqueante)
	saved unix.Termios
	raw   bool // la terminal está en modo raw y en la pantalla alternativa

	mu      sync.Mutex
	paused  bool
	resume  chan struct{}
	in      chan []byte
	readErr chan error
}

// control corre f con el descriptor de la terminal (sin f.Fd()).
func (t *tty) control(f func(fd int) error) error {
	var ferr error
	if err := t.rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func (t *tty) getTermios() (*unix.Termios, error) {
	var tio *unix.Termios
	err := t.control(func(fd int) (err error) {
		tio, err = unix.IoctlGetTermios(fd, ioctlGetTermios)
		return err
	})
	return tio, err
}

func (t *tty) setTermios(req uint, tio *unix.Termios) error {
	return t.control(func(fd int) error { return unix.IoctlSetTermios(fd, req, tio) })
}

func openTTY() (*tty, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &tty{f: f, rc: rc, resume: make(chan struct{}), in: make(chan []byte, 64), readErr: make(chan error, 1)}
	tio, err := t.getTermios()
	if err != nil {
		f.Close()
		return nil, err
	}
	t.saved = *tio
	go t.reader()
	return t, nil
}

// size: filas y columnas (ioctl TIOCGWINSZ).
func (t *tty) size() (rows, cols int) {
	var ws *unix.Winsize
	err := t.control(func(fd int) (err error) {
		ws, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		return err
	})
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 100
	}
	return int(ws.Row), int(ws.Col)
}

// Al entrar: pantalla alternativa, cursor oculto, sin salto de línea automático
// (lo que no cabe a lo ancho se corta en vez de bajar de línea), ratón solo para
// la rueda (1000 + formato SGR 1006) y pegado delimitado (2004) para descartarlo.
const (
	enterSeq = "\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[H\x1b[2J"
	leaveSeq = "\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?7h\x1b[0m\x1b[?25h\x1b[?1049l"
)

func (t *tty) enter() {
	raw := t.saved
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	// OPOST se queda: "\n" sigue siendo "\r\n".
	_ = t.setTermios(ioctlSetTermios, &raw)
	t.raw = true
	t.write(enterSeq, time.Second)
}

// leave deja la terminal como estaba. Se puede llamar varias veces.
func (t *tty) leave() {
	if !t.raw {
		return
	}
	t.raw = false
	t.write(leaveSeq, time.Second)
	// TCSETSF descarta lo que quedó sin leer (p. ej. eventos de la rueda) para
	// que no aparezca en la shell.
	_ = t.setTermios(ioctlSetTermiosFlush, &t.saved)
}

// write escribe con plazo: si la terminal no lee, se rinde en vez de colgarse.
func (t *tty) write(s string, d time.Duration) bool {
	_ = t.f.SetWriteDeadline(time.Now().Add(d))
	_, err := t.f.WriteString(s)
	return err == nil
}

// reader manda lo que llega de la terminal en trozos; nunca se bloquea en el
// canal más que lo que tarda el bucle principal en vaciarlo.
func (t *tty) reader() {
	buf := make([]byte, 4096)
	for {
		n, err := t.f.Read(buf)
		if n > 0 {
			t.in <- bytes.Clone(buf[:n])
		}
		if err != nil {
			t.mu.Lock()
			paused := t.paused
			t.mu.Unlock()
			if paused {
				<-t.resume
				continue
			}
			if os.IsTimeout(err) {
				continue
			}
			t.readErr <- err
			return
		}
	}
}

// pause deja de leer la terminal (sudo la necesita para la contraseña).
func (t *tty) pause() {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	_ = t.f.SetReadDeadline(time.Now())
}

func (t *tty) unpause() {
	t.mu.Lock()
	was := t.paused
	t.paused = false
	_ = t.f.SetReadDeadline(time.Time{})
	t.mu.Unlock()
	if was {
		select {
		case t.resume <- struct{}{}:
		case <-time.After(100 * time.Millisecond): // el lector no llegó a pausarse
		}
	}
}

// ---------- entrada ----------

type key int

const (
	keyNone key = iota
	keyQuit
	keyUp
	keyDown
	keyPgUp
	keyPgDn
	keyHome
	keyEnd
	keyWheelUp
	keyWheelDown
	keyEnter
	keyRune // tecla normal: ver ev.r
)

type event struct {
	k key
	r byte
}

// parser convierte bytes de la terminal en eventos. Lo que no usamos (foco,
// clics, flechas laterales, Alt+tecla, pegado, respuestas de la terminal…) se
// descarta entero, sin dejar letras sueltas.
type parser struct {
	pend    []byte // secuencia sin terminar
	pasting bool   // dentro de un pegado: se ignora hasta \x1b[201~
}

const pasteEnd = "\x1b[201~"

func (p *parser) feed(b []byte) []event {
	data := append(p.pend, b...)
	p.pend = nil
	var evs []event
	for i := 0; i < len(data); {
		if p.pasting {
			j := bytes.Index(data[i:], []byte(pasteEnd))
			if j < 0 {
				// Guardar solo una posible marca de fin a medias.
				keep := min(len(data)-i, len(pasteEnd)-1)
				p.pend = append([]byte(nil), data[len(data)-keep:]...)
				return evs
			}
			p.pasting = false
			i += j + len(pasteEnd)
			continue
		}
		c := data[i]
		if c != 0x1b {
			i++
			switch c {
			case 'q', 'Q', 3, 4, 0x1c: // q, Ctrl-C, Ctrl-D, Ctrl-\
				evs = append(evs, event{k: keyQuit})
			case 'k':
				evs = append(evs, event{k: keyUp})
			case '\r', '\n': // Enter (y Ctrl-J)
				evs = append(evs, event{k: keyEnter})
			case 'j':
				evs = append(evs, event{k: keyDown})
			case ' ':
				evs = append(evs, event{k: keyPgDn})
			case 'g':
				evs = append(evs, event{k: keyHome})
			case 'G':
				evs = append(evs, event{k: keyEnd})
			default:
				if c >= 0x20 && c < 0x7f {
					evs = append(evs, event{k: keyRune, r: c})
				}
			}
			continue
		}
		n, ev, ok := p.escape(data[i:])
		if !ok {
			p.pend = append([]byte(nil), data[i:]...)
			return evs
		}
		if ev.k != keyNone {
			evs = append(evs, ev)
		}
		i += n
	}
	return evs
}

// escape interpreta una secuencia que empieza con ESC. ok=false: falta el resto.
func (p *parser) escape(s []byte) (n int, ev event, ok bool) {
	if len(s) < 2 {
		return 0, ev, false
	}
	switch s[1] {
	case 0x1b: // Esc Esc: el primero es la tecla Esc
		return 1, event{k: keyQuit}, true
	case '[':
		// CSI: parámetros 0x30–0x3f, intermedios 0x20–0x2f, final 0x40–0x7e.
		j := 2
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
			j++
		}
		if j >= len(s) {
			return 0, ev, len(s) > 64 // algo raro y larguísimo: se descarta
		}
		final := s[j]
		if final < 0x40 || final > 0x7e {
			return j, ev, true // secuencia rota: se descarta lo leído
		}
		params := string(s[2:j])
		n = j + 1
		if final == 'M' && params == "" { // ratón X10: 3 bytes más
			if len(s) < n+3 {
				return 0, ev, false
			}
			return n + 3, ev, true
		}
		return n, p.csi(params, final), true
	case 'O': // SS3: flechas en modo aplicación
		if len(s) < 3 {
			return 0, ev, false
		}
		switch s[2] {
		case 'A':
			ev.k = keyUp
		case 'B':
			ev.k = keyDown
		case 'H':
			ev.k = keyHome
		case 'F':
			ev.k = keyEnd
		case 'M': // Enter del teclado numérico
			ev.k = keyEnter
		}
		return 3, ev, true
	case ']', 'P', '_', '^', 'X': // cadenas (OSC, DCS…): hasta BEL o ESC \
		for j := 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, ev, true
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, ev, true
			}
		}
		return 0, ev, false
	}
	return 2, ev, true // Alt+tecla: se ignora
}

func (p *parser) csi(params string, final byte) event {
	if strings.HasPrefix(params, "<") { // ratón SGR: <botón;x;y M|m
		b, _ := strconv.Atoi(strings.SplitN(params[1:], ";", 2)[0])
		if final == 'M' && b&64 != 0 {
			switch b & 3 {
			case 0:
				return event{k: keyWheelUp}
			case 1:
				return event{k: keyWheelDown}
			}
		}
		return event{}
	}
	switch final {
	case 'A':
		return event{k: keyUp}
	case 'B':
		return event{k: keyDown}
	case 'H':
		return event{k: keyHome}
	case 'F':
		return event{k: keyEnd}
	case '~':
		switch strings.SplitN(params, ";", 2)[0] {
		case "5":
			return event{k: keyPgUp}
		case "6":
			return event{k: keyPgDn}
		case "1", "7":
			return event{k: keyHome}
		case "4", "8":
			return event{k: keyEnd}
		case "200":
			p.pasting = true
		}
	}
	return event{} // foco (I/O), flechas laterales, F1…, etc.
}

// pending: hay una secuencia a medias; flush la resuelve si no llega el resto.
func (p *parser) pending() bool { return len(p.pend) > 0 || p.pasting }

func (p *parser) flush() []event {
	wasEsc := !p.pasting && len(p.pend) == 1 && p.pend[0] == 0x1b
	p.pend, p.pasting = nil, false
	if wasEsc {
		return []event{{k: keyQuit}} // la tecla Esc sola
	}
	return nil
}

// ---------- pantalla ----------

// screen dibuja reescribiendo solo las líneas que cambian.
type screen struct {
	t    *tty
	prev []string
}

// draw escribe las filas que cambiaron, dentro de una actualización sincronizada
// (2026) para que la terminal no muestre el cuadro a medias.
func (s *screen) draw(lines []string, full bool) {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	if full || len(s.prev) != len(lines) {
		b.WriteString("\x1b[H\x1b[2J")
		s.prev = nil
	}
	changed := false
	for i, l := range lines {
		if i < len(s.prev) && s.prev[i] == l {
			continue
		}
		changed = true
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s\x1b[K", i+1, reset, l)
	}
	if !changed && s.prev != nil {
		return
	}
	b.WriteString(reset + "\x1b[?2026l")
	if s.t.write(b.String(), 200*time.Millisecond) {
		s.prev = lines
	} else {
		s.prev = nil // quedó a medias: la próxima vez, todo
	}
}

// ---------- el panel ----------

// watch: el TUI. Es solo un panel: lo que muestra y el límite son del servicio
// (turn_watch.service), así que cerrarlo no cambia nada. Al abrirlo activa el límite
// si estaba desactivado; `d` lo desactiva, `a` lo activa, +/- lo cambian.
func watch() {
	st := readState()
	if !daemonUp(st) {
		fmt.Fprintln(os.Stderr, "turn_watch: el servicio no está corriendo. Arráncalo con:\n  "+startHint()+"\n(o vuelve a correr `turn_watch install`)")
		os.Exit(1)
	}
	if !st.LimitOn {
		request("on")
	}
	ensureHelper()

	t, err := openTTY()
	if err != nil {
		fmt.Fprintln(os.Stderr, "turn_watch: el panel necesita una terminal:", err)
		os.Exit(1)
	}
	// Restaurar la terminal pase lo que pase en esta goroutine (también con pánico).
	defer t.leave()
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(sig)
	defer signal.Stop(winch)
	t.enter()

	scr := &screen{t: t}
	var p parser
	escTimer := time.NewTimer(time.Hour)
	escTimer.Stop()
	refresh := time.NewTicker(500 * time.Millisecond)
	defer refresh.Stop()
	soon := time.NewTimer(time.Hour) // redibujo poco después de una acción
	soon.Stop()

	view := newUI()
	view.warns = localWarnings(st)
	draw := func(full bool) {
		st = readState()
		rows, cols := t.size()
		scr.draw(panel(st, view, rows, cols), full)
	}
	// handle aplica los eventos; true: salir.
	handle := func(evs []event) bool {
		moved, acted := false, false
		for _, ev := range evs {
			switch ev.k {
			case keyQuit:
				return true
			case keyUp, keyWheelUp:
				view.move(-1)
			case keyDown, keyWheelDown:
				view.move(1)
			case keyPgUp:
				view.move(-max(view.page-1, 1))
			case keyPgDn:
				view.move(max(view.page-1, 1))
			case keyHome:
				view.move(-len(view.keys))
			case keyEnd:
				view.move(len(view.keys))
			case keyEnter:
				view.toggle()
			case keyRune:
				switch ev.r {
				case '+', '=', '-', '_':
					n := loadConfig().Slots
					if st != nil && st.LimitOn {
						n = st.Slots
					}
					if ev.r == '+' || ev.r == '=' {
						n++
					} else {
						n--
					}
					setLimit(n)
					acted = true
				case 'd', 'D':
					request("off")
					acted = true
				case 'a', 'A':
					request("on")
					acted = true
				case 't', 'T':
					// Tope de los agentes: lo aplica el servicio en su siguiente vuelta.
					setAgentsCapConfig(!loadConfig().agentsCap())
					acted = true
				case 's', 'S':
					// sudo necesita la terminal normal para pedir la contraseña, y
					// que nadie más la esté leyendo.
					t.pause()
					t.leave()
					setSystemConfig(true)
					ensureHelper()
					t.enter()
					t.unpause()
					p = parser{}
					draw(true)
				}
			}
			moved = moved || ev.k != keyRune
		}
		if acted {
			// Que el servicio alcance a aplicarlo, sin dejar de leer teclas.
			soon.Reset(150 * time.Millisecond)
		}
		if moved || acted {
			draw(false)
		}
		return false
	}

	draw(true)
	for {
		select {
		case <-sig:
			return
		case <-t.readErr:
			return // la terminal se cerró
		case b := <-t.in:
			if handle(p.feed(b)) {
				return
			}
			if p.pending() {
				d := 50 * time.Millisecond // Esc solo o secuencia cortada
				if p.pasting {
					d = time.Second
				}
				escTimer.Reset(d)
			} else {
				escTimer.Stop()
			}
		case <-escTimer.C:
			if handle(p.flush()) {
				return
			}
		case <-winch:
			draw(true)
		case <-soon.C:
			draw(false)
		case <-refresh.C:
			draw(false)
		}
	}
}

func siNo(b bool) string {
	if b {
		return "sí"
	}
	return "no"
}
