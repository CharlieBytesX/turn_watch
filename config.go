package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config: ~/.config/turn_watch/config.json. Se escribe al cambiar el límite.
type Config struct {
	Slots int `json:"slots"` // núcleos para compilar mientras el TUI está abierto
	// Todo el sistema (root, Docker, otros usuarios) con sudo. nil: preguntar.
	System *bool `json:"system,omitempty"`
	// Límite activo; lo recuerda el servicio entre reinicios.
	Active bool `json:"active"`
	// Agentes de código: nombres de proceso (comm o argv[0]). Vacío: defaultAgents.
	Agents []string `json:"agents,omitempty"`
	// Con el límite activo, meter a los agentes (y todo lo que lancen) en
	// turn_watch.slice. nil: sí.
	AgentsCap *bool `json:"agents_cap,omitempty"`
	// Caché kache. nil: sí. false: no se usa y el panel no avisa por ella
	// (`TURN_WATCH_SIN_KACHE=1 turn_watch install` lo deja así).
	Kache *bool `json:"kache,omitempty"`
	// Linux: meter el daemon de kache (kache.service) en turn_watch.slice, con el
	// mismo tope de CPU. nil o false: fuera (lo aplica `turn_watch install`).
	KacheInSlice *bool `json:"kache_in_slice,omitempty"`
}

var defaultAgents = []string{"claude", "codex", "opencode", "pi", "aider", "gemini", "cursor-agent", "goose"}

func (c Config) agentNames() []string {
	if len(c.Agents) == 0 {
		return defaultAgents
	}
	return c.Agents
}

func (c Config) agentsCap() bool { return c.AgentsCap == nil || *c.AgentsCap }

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "turn_watch", "config.json")
}

// defaultSlots: todos los núcleos menos 2 (para el escritorio), al menos 1.
func defaultSlots() int { return max(numCPU()-2, 1) }

func loadConfig() Config {
	cfg, _ := loadConfigOK()
	return cfg
}

// loadConfigOK: ok=false si el archivo existe pero no se entiende (a medias mientras
// alguien lo guarda con el editor): el servicio sigue con lo que tenía.
func loadConfigOK() (Config, bool) {
	ok := true
	cfg := Config{Slots: defaultSlots()}
	if b, err := os.ReadFile(configPath()); err == nil {
		ok = json.Unmarshal(b, &cfg) == nil
	}
	if cfg.Slots < 1 {
		cfg.Slots = defaultSlots()
	}
	cfg.Slots = min(cfg.Slots, numCPU())
	return cfg, ok
}

func saveConfig(cfg Config) {
	_ = os.MkdirAll(filepath.Dir(configPath()), 0o755)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// Atómico: el servicio lo lee en cada vuelta y no debe ver un archivo a medias.
	tmp := configPath() + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		_ = os.Rename(tmp, configPath())
	}
}

// setSlotsConfig guarda el límite sin tocar el resto de la configuración.
func setSlotsConfig(n int) {
	cfg := loadConfig()
	cfg.Slots = n
	saveConfig(cfg)
}

func setSystemConfig(on bool) {
	cfg := loadConfig()
	cfg.System = &on
	saveConfig(cfg)
}

func setActiveConfig(on bool) {
	cfg := loadConfig()
	cfg.Active = on
	saveConfig(cfg)
}

func setAgentsCapConfig(on bool) {
	cfg := loadConfig()
	cfg.AgentsCap = &on
	saveConfig(cfg)
}

// envVar lee TURN_WATCH_<name>; si no está, acepta la vieja RUSTWATCH_<name>.
func envVar(name string) string {
	if v := os.Getenv("TURN_WATCH_" + name); v != "" {
		return v
	}
	return os.Getenv("RUSTWATCH_" + name)
}
