package main

// Salud y estadísticas de kache, siempre a la vista (state.json → panel, `once`).
//
// Antes, si kache no se instalaba o no se configuraba, `turn_watch install` lo decía
// una vez y después todo seguía sin caché sin que nadie se enterara. Ahora el
// servicio lo comprueba al arrancar y cada kacheEvery, con la salida para máquinas
// de kache (`kache --json …`, un documento versionado: schema_version 1,
// command, success, error y los campos del comando; ver docs/commands/reference.mdx
// y src/machine.rs en kunobi-ninja/kache):
//
//   - `kache --json daemon status`: si el daemon corre (no lo arranca);
//   - `kache --json stats --since 24h`, solo con el daemon vivo, porque si no lo
//     encuentra `kache stats` arranca uno con el entorno de quien pregunta: aciertos
//     (local + prefetch + remoto), duplicados, fallos, tiempo ahorrado, tamaño del
//     almacén y su límite, y los totales desde que kache cuenta (lifetime);
//   - `kache --json doctor` al arrancar y cada doctorEvery (solo informa: sin --fix
//     no cambia nada).
//
// Si algo falla o falta un campo, el estado lo dice ("estadísticas de kache no
// disponibles: <motivo>"); nunca se muestra un 0 % inventado.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	kacheEvery     = 30 * time.Second
	doctorEvery    = 30 * time.Minute
	kacheSchema    = 1
	kacheWindow    = "24h"
	kacheSetupFile = "kache-setup-error.txt"
)

// KacheState: el estado de la caché, de peor a mejor según el orden en kacheHealth.
type KacheState string

const (
	kacheOK         KacheState = "ok"           // configurada, daemon vivo, estadísticas leídas
	kacheDisabled   KacheState = "disabled"     // TURN_WATCH_SIN_KACHE=1 o "kache": false
	kacheMissing    KacheState = "missing"      // no hay binario
	kacheUnconfig   KacheState = "unconfigured" // cargo no la usa
	kacheConflict   KacheState = "conflict"     // cargo usa otro rustc-wrapper
	kacheDaemonDown KacheState = "daemon_down"  // el daemon no corre
	kacheError      KacheState = "error"        // kache no responde o su salida no se entiende
)

// KacheStatus: lo que el servicio sabe de kache (en state.json).
type KacheStatus struct {
	Checked       time.Time    `json:"checked"`
	State         KacheState   `json:"state"`
	Detail        string       `json:"detail,omitempty"` // el motivo, si no es ok
	Path          string       `json:"path,omitempty"`
	Wrapper       WrapperInfo  `json:"wrapper"`
	DaemonUp      *bool        `json:"daemon_up,omitempty"` // nil: no se sabe
	DaemonVersion string       `json:"daemon_version,omitempty"`
	Stats         *KacheStats  `json:"stats,omitempty"`
	StatsErr      string       `json:"stats_error,omitempty"`
	Doctor        *KacheDoctor `json:"doctor,omitempty"`
	DoctorErr     string       `json:"doctor_error,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
	LastErrorAt   time.Time    `json:"last_error_at,omitzero"`
	SetupError    string       `json:"setup_error,omitempty"` // lo que falló en `turn_watch install`

	doctorChecked time.Time // cuándo corrió doctor (también si falló)
}

// KacheStats: `kache stats --json` de la ventana (24 h) y desde siempre.
type KacheStats struct {
	Window          string         `json:"window"`
	LocalHits       int            `json:"local_hits"`
	PrefetchHits    int            `json:"prefetch_hits"`
	RemoteHits      int            `json:"remote_hits"`
	Dups            int            `json:"dups"`
	Misses          int            `json:"misses"`
	TimeSavedMs     uint64         `json:"time_saved_ms"`
	StoreBytes      uint64         `json:"store_bytes"`
	StoreLimitBytes uint64         `json:"store_limit_bytes"`
	Entries         int            `json:"entries"`
	Lifetime        *KacheLifetime `json:"lifetime,omitempty"`
}

// KacheLifetime: totales desde que kache empezó a contar.
type KacheLifetime struct {
	Since       time.Time `json:"since"`
	Hits        uint64    `json:"hits"`
	TimeSavedMs uint64    `json:"time_saved_ms"`
}

// Hits: aciertos (local, prefetch y remoto), como los cuenta kache.
func (s *KacheStats) Hits() int { return s.LocalHits + s.PrefetchHits + s.RemoteHits }

// Total: todas las compilaciones que pasaron por kache en la ventana.
func (s *KacheStats) Total() int { return s.Hits() + s.Dups + s.Misses }

// Ratio: porcentaje de aciertos; ok=false si no hubo compilaciones (no es 0 %).
func (s *KacheStats) Ratio() (pct float64, ok bool) {
	if s.Total() == 0 {
		return 0, false
	}
	return 100 * float64(s.Hits()) / float64(s.Total()), true
}

// KacheDoctor: el resumen de `kache doctor --json`.
type KacheDoctor struct {
	Checked time.Time `json:"checked"`
	Issues  int       `json:"issues"`
	Failed  []string  `json:"failed,omitempty"` // "label: detalle" de lo que falló
}

// ---------- la salida de kache ----------

type kacheEnvelope struct {
	SchemaVersion *int    `json:"schema_version"`
	Command       *string `json:"command"`
	Success       *bool   `json:"success"`
	Error         *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeKache comprueba el sobre de un documento de kache y llena body.
func decodeKache(out []byte, command string, body any) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return errors.New("kache no escribió nada")
	}
	var env kacheEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return fmt.Errorf("la salida de kache no es JSON: %v", err)
	}
	switch {
	case env.SchemaVersion == nil:
		return errors.New("la salida de kache no trae schema_version")
	case *env.SchemaVersion != kacheSchema:
		return fmt.Errorf("versión %d del JSON de kache no soportada (se espera %d)", *env.SchemaVersion, kacheSchema)
	case env.Success == nil:
		return errors.New("la salida de kache no trae success")
	case !*env.Success:
		if env.Error != nil && env.Error.Message != "" {
			return fmt.Errorf("kache: %s (%s)", env.Error.Message, env.Error.Code)
		}
		return errors.New("kache: falló sin decir por qué")
	case env.Command == nil || *env.Command != command:
		got := "nada"
		if env.Command != nil {
			got = *env.Command
		}
		return fmt.Errorf("se esperaba la salida de %q y llegó la de %q", command, got)
	}
	return json.Unmarshal(out, body)
}

// required: nombre del primer campo obligatorio que falta.
func required(fields map[string]bool) error {
	var missing []string
	for name, ok := range fields {
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("a la salida de kache le faltan campos: %s", strings.Join(missing, ", "))
}

// parseKacheStats: `kache --json stats --since 24h`.
func parseKacheStats(out []byte) (*KacheStats, error) {
	var b struct {
		Disk *struct {
			StoreBytes      *uint64 `json:"store_bytes"`
			StoreLimitBytes *uint64 `json:"store_limit_bytes"`
		} `json:"disk"`
		Entries      *int    `json:"entries"`
		TimeSavedMs  *uint64 `json:"time_saved_ms"`
		LocalHits    *int    `json:"local_hits"`
		PrefetchHits *int    `json:"prefetch_hits"`
		RemoteHits   *int    `json:"remote_hits"`
		Dups         *int    `json:"dups"`
		Misses       *int    `json:"misses"`
		Since        *string `json:"since"`
		Lifetime     *struct {
			Since       time.Time `json:"since"`
			Hits        uint64    `json:"hits"`
			TimeSavedMs uint64    `json:"hit_compile_time_ms"`
		} `json:"lifetime"`
	}
	if err := decodeKache(out, "stats", &b); err != nil {
		return nil, err
	}
	if err := required(map[string]bool{
		"disk.store_bytes":       b.Disk != nil && b.Disk.StoreBytes != nil,
		"disk.store_limit_bytes": b.Disk != nil && b.Disk.StoreLimitBytes != nil,
		"entries":                b.Entries != nil,
		"time_saved_ms":          b.TimeSavedMs != nil,
		"local_hits":             b.LocalHits != nil,
		"prefetch_hits":          b.PrefetchHits != nil,
		"remote_hits":            b.RemoteHits != nil,
		"dups":                   b.Dups != nil,
		"misses":                 b.Misses != nil,
		"since":                  b.Since != nil,
	}); err != nil {
		return nil, err
	}
	s := &KacheStats{
		Window: *b.Since, LocalHits: *b.LocalHits, PrefetchHits: *b.PrefetchHits, RemoteHits: *b.RemoteHits,
		Dups: *b.Dups, Misses: *b.Misses, TimeSavedMs: *b.TimeSavedMs, Entries: *b.Entries,
		StoreBytes: *b.Disk.StoreBytes, StoreLimitBytes: *b.Disk.StoreLimitBytes,
	}
	if b.Lifetime != nil {
		s.Lifetime = &KacheLifetime{Since: b.Lifetime.Since, Hits: b.Lifetime.Hits, TimeSavedMs: b.Lifetime.TimeSavedMs}
	}
	return s, nil
}

// parseDaemonStatus: `kache --json daemon status`.
func parseDaemonStatus(out []byte) (running bool, version string, err error) {
	var b struct {
		Running *bool   `json:"daemon_running"`
		Version *string `json:"daemon_version"`
	}
	if err := decodeKache(out, "daemon-status", &b); err != nil {
		return false, "", err
	}
	if err := required(map[string]bool{"daemon_running": b.Running != nil}); err != nil {
		return false, "", err
	}
	if b.Version != nil {
		version = *b.Version
	}
	return *b.Running, version, nil
}

// parseDoctor: `kache --json doctor`.
func parseDoctor(out []byte) (*KacheDoctor, error) {
	var b struct {
		Issues *int `json:"issues"`
		Checks []struct {
			Label    string `json:"label"`
			Pass     bool   `json:"pass"`
			Optional bool   `json:"optional"`
			Detail   string `json:"detail"`
		} `json:"checks"`
	}
	if err := decodeKache(out, "doctor", &b); err != nil {
		return nil, err
	}
	if err := required(map[string]bool{"issues": b.Issues != nil}); err != nil {
		return nil, err
	}
	d := &KacheDoctor{Issues: *b.Issues}
	for _, c := range b.Checks {
		if !c.Pass && !c.Optional {
			d.Failed = append(d.Failed, strings.TrimSpace(c.Label+": "+c.Detail))
		}
	}
	return d, nil
}

// ---------- el vigilante (en el servicio) ----------

// kacheRunner corre kache y devuelve su salida estándar (y un error si no salió 0).
// kache escribe su documento JSON también cuando falla, así que se lee igual.
type kacheRunner func(ctx context.Context, bin string, args ...string) (stdout []byte, err error)

func execKache(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), fmt.Errorf("kache %s: sin respuesta (%v)", strings.Join(args, " "), ctx.Err())
	}
	if err != nil && errb.Len() > 0 {
		err = fmt.Errorf("%v: %s", err, lastLine(errb.String()))
	}
	return out.Bytes(), err
}

type kacheWatcher struct {
	run       kacheRunner
	find      func() string
	lookupEnv func(string) (string, bool)
	home      string
	disabled  func() string // motivo si está desactivada ("" = no)

	mu  sync.Mutex
	cur *KacheStatus
}

func newKacheWatcher() *kacheWatcher {
	return &kacheWatcher{
		run: execKache, find: findKache, lookupEnv: os.LookupEnv, home: home(),
		disabled: func() string {
			if envVar("SIN_KACHE") != "" {
				return "desactivada (TURN_WATCH_SIN_KACHE)"
			}
			if c := loadConfig(); c.Kache != nil && !*c.Kache {
				return `desactivada ("kache": false en config.json)`
			}
			return ""
		},
	}
}

// Snapshot: el último estado (nil antes de la primera comprobación).
func (w *kacheWatcher) Snapshot() *KacheStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	c := *w.cur
	return &c
}

// Loop comprueba al empezar y cada kacheEvery (doctor: cada doctorEvery).
func (w *kacheWatcher) Loop(stop <-chan struct{}) {
	t := time.NewTicker(kacheEvery)
	defer t.Stop()
	for {
		w.Refresh(time.Now())
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// Refresh hace una comprobación completa (bloquea; segundos como mucho).
func (w *kacheWatcher) Refresh(now time.Time) {
	w.mu.Lock()
	prev := w.cur
	w.mu.Unlock()
	next := w.check(now, prev)
	w.mu.Lock()
	w.cur = next
	w.mu.Unlock()
}

func (w *kacheWatcher) check(now time.Time, prev *KacheStatus) *KacheStatus {
	st := &KacheStatus{Checked: now}
	if prev != nil {
		st.LastError, st.LastErrorAt = prev.LastError, prev.LastErrorAt
	}
	fail := func(msg string) {
		st.LastError, st.LastErrorAt = msg, now
	}
	if why := w.disabled(); why != "" {
		st.State, st.Detail = kacheDisabled, why
		return st
	}
	st.SetupError = readSetupError()
	st.Path = w.find()
	if st.Path == "" {
		st.State, st.Detail = kacheMissing, "kache no está instalado: `turn_watch install` lo instala (TURN_WATCH_SIN_KACHE=1 para no usarlo)"
		if st.SetupError != "" {
			st.Detail += "; en la instalación: " + st.SetupError
		}
		return st
	}
	st.Wrapper = detectWrapper(w.lookupEnv, w.home)

	// El daemon: si no corre, no se piden estadísticas (kache stats lo arrancaría).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	out, err := w.run(ctx, st.Path, "--json", "daemon", "status")
	cancel()
	running, version, perr := parseDaemonStatus(out)
	switch {
	case perr != nil:
		msg := "kache daemon status: " + perr.Error()
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		fail(msg)
		st.StatsErr = msg
	default:
		st.DaemonUp, st.DaemonVersion = &running, version
		if !running {
			st.StatsErr = "el daemon de kache no corre"
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			out, err := w.run(ctx, st.Path, "--json", "stats", "--since", kacheWindow)
			cancel()
			if s, perr := parseKacheStats(out); perr != nil {
				st.StatsErr = perr.Error()
				if err != nil {
					st.StatsErr += " (" + err.Error() + ")"
				}
				fail("kache stats: " + st.StatsErr)
			} else {
				st.Stats = s
			}
		}
	}

	// doctor: al arrancar y cada doctorEvery; entre medio se repite el último.
	if prev != nil && (prev.Doctor != nil || prev.DoctorErr != "") && now.Sub(prev.doctorAt()) < doctorEvery {
		st.Doctor, st.DoctorErr = prev.Doctor, prev.DoctorErr
		st.doctorChecked = prev.doctorAt()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		out, err := w.run(ctx, st.Path, "--json", "doctor")
		cancel()
		st.doctorChecked = now
		if d, perr := parseDoctor(out); perr != nil {
			st.DoctorErr = perr.Error()
			if err != nil {
				st.DoctorErr += " (" + err.Error() + ")"
			}
			fail("kache doctor: " + st.DoctorErr)
		} else {
			d.Checked = now
			st.Doctor = d
		}
	}

	st.State, st.Detail = kacheHealth(st)
	return st
}

// kacheHealth: el estado según lo comprobado, de lo más grave a lo menos.
func kacheHealth(st *KacheStatus) (KacheState, string) {
	switch st.Wrapper.Kind {
	case WrapperOther, WrapperOff:
		return kacheConflict, st.Wrapper.String() + ": turn_watch no lo cambia; kache no se usa"
	case WrapperNone:
		d := "cargo no usa kache (falta rustc-wrapper): `kache init -y --no-shell` o `turn_watch install`"
		if st.SetupError != "" {
			d += "; en la instalación: " + st.SetupError
		}
		return kacheUnconfig, d
	case WrapperUnknown:
		return kacheUnconfig, st.Wrapper.String()
	}
	switch {
	case st.DaemonUp == nil:
		return kacheError, "kache no responde: " + st.StatsErr
	case !*st.DaemonUp:
		return kacheDaemonDown, "el daemon de kache no corre: `kache daemon start` (la próxima compilación también lo arranca)"
	case st.Stats == nil:
		return kacheError, "estadísticas de kache no disponibles: " + st.StatsErr
	}
	return kacheOK, ""
}

// doctorChecked no va a state.json: Doctor.Checked ya lo dice.
func (s *KacheStatus) doctorAt() time.Time {
	if s.Doctor != nil {
		return s.Doctor.Checked
	}
	return s.doctorChecked
}

// ---------- lo que falló en `turn_watch install` ----------

func setupErrorPath() string { return filepath.Join(filepath.Dir(configPath()), kacheSetupFile) }

// recordSetupError guarda (o con "" borra) lo que falló al instalar o configurar
// kache, para que el servicio lo siga mostrando.
func recordSetupError(msg string) {
	if msg == "" {
		_ = os.Remove(setupErrorPath())
		return
	}
	_ = os.MkdirAll(filepath.Dir(setupErrorPath()), 0o755)
	_ = os.WriteFile(setupErrorPath(), []byte(time.Now().Format(time.RFC3339)+" "+msg+"\n"), 0o644)
}

func readSetupError() string {
	b, err := os.ReadFile(setupErrorPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
