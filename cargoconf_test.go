package main

import (
	"os"
	"path/filepath"
	"testing"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDetectWrapper(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string // nombre del archivo en CARGO_HOME ("" = ninguno)
		config string
		env    map[string]string
		want   WrapperKind
		value  string
	}{
		{name: "sin config", want: WrapperNone},
		{name: "kache", file: "config.toml", config: "[build]\nrustc-wrapper = \"kache\"\n", want: WrapperKache, value: "kache"},
		{name: "ruta absoluta a kache", file: "config.toml", config: "[build]\nrustc-wrapper = '/home/u/.local/bin/kache'\n", want: WrapperKache, value: "/home/u/.local/bin/kache"},
		{name: "clave con puntos", file: "config.toml", config: "build.rustc-wrapper = \"kache\"\n", want: WrapperKache},
		{name: "comentada no cuenta", file: "config.toml", config: "[build]\n# rustc-wrapper = \"kache\"\njobs = 4\n", want: WrapperNone},
		{name: "en otra tabla no cuenta", file: "config.toml", config: "[target.x86_64-unknown-linux-gnu]\nrustc-wrapper = \"kache\"\n", want: WrapperNone},
		{name: "sccache", file: "config.toml", config: "[build]\nrustc-wrapper = \"sccache\"\n", want: WrapperOther, value: "sccache"},
		{name: "vacío apaga", file: "config.toml", config: "[build]\nrustc-wrapper = \"\"\n", want: WrapperOff},
		{name: "config sin extensión", file: "config", config: "[build]\nrustc-wrapper = \"kache\"\n", want: WrapperKache},
		{name: "TOML roto", file: "config.toml", config: "[build\nrustc-wrapper = kache\n", want: WrapperUnknown},
		{name: "no es texto", file: "config.toml", config: "[build]\nrustc-wrapper = 3\n", want: WrapperUnknown},
		{name: "RUSTC_WRAPPER gana a la config", file: "config.toml", config: "[build]\nrustc-wrapper = \"kache\"\n",
			env: map[string]string{"RUSTC_WRAPPER": "sccache"}, want: WrapperOther, value: "sccache"},
		{name: "RUSTC_WRAPPER vacío apaga", file: "config.toml", config: "[build]\nrustc-wrapper = \"kache\"\n",
			env: map[string]string{"RUSTC_WRAPPER": ""}, want: WrapperOff},
		{name: "CARGO_BUILD_RUSTC_WRAPPER", env: map[string]string{"CARGO_BUILD_RUSTC_WRAPPER": "/opt/kache/bin/kache"}, want: WrapperKache},
		{name: "RUSTC_WRAPPER antes que CARGO_BUILD_RUSTC_WRAPPER",
			env: map[string]string{"RUSTC_WRAPPER": "kache", "CARGO_BUILD_RUSTC_WRAPPER": "sccache"}, want: WrapperKache},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			// $CARGO_HOME propio: ~/.cargo no se mira.
			ch := filepath.Join(t.TempDir(), "cargo-home")
			_ = os.MkdirAll(ch, 0o755)
			env["CARGO_HOME"] = ch
			_ = os.MkdirAll(filepath.Join(home, ".cargo"), 0o755)
			_ = os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[build]\nrustc-wrapper = \"decoy\"\n"), 0o644)
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(ch, tc.file), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			w := detectWrapper(envMap(env), home)
			if w.Kind != tc.want || (tc.value != "" && w.Value != tc.value) {
				t.Fatalf("%+v, quería %s %q", w, tc.want, tc.value)
			}
			if w.String() == "" {
				t.Error("sin descripción")
			}
		})
	}
	// Sin CARGO_HOME: ~/.cargo.
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".cargo"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[build]\nrustc-wrapper = \"kache\"\n"), 0o644)
	if w := detectWrapper(envMap(nil), home); w.Kind != WrapperKache {
		t.Errorf("~/.cargo: %+v", w)
	}
}

func TestCargoBypass(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "local", "bin")
	shims := filepath.Join(root, "mise", "shims")
	rustup := filepath.Join(root, "cargo", "bin")
	for _, d := range []string{bin, shims, rustup} {
		_ = os.MkdirAll(d, 0o755)
	}
	self := filepath.Join(bin, "turn_watch")
	_ = os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Symlink("turn_watch", filepath.Join(bin, "cargo"))
	_ = os.WriteFile(filepath.Join(shims, "cargo"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(rustup, "cargo"), []byte("#!/bin/sh\n"), 0o755)
	join := func(ds ...string) string { return filepath.Join(ds[0]) + listSep(ds[1:]) }
	if first, bypass := cargoBypass(join(bin, shims, rustup), bin, self); bypass || first != filepath.Join(bin, "cargo") {
		t.Errorf("envoltorio primero: %s %v", first, bypass)
	}
	if first, bypass := cargoBypass(join(shims, bin, rustup), bin, self); !bypass || first != filepath.Join(shims, "cargo") {
		t.Errorf("mise primero: %s %v", first, bypass)
	}
	if _, bypass := cargoBypass(join(rustup, bin), bin, self); !bypass {
		t.Error("rustup primero")
	}
	// Un enlace al envoltorio en otra carpeta cuenta como el envoltorio.
	other := filepath.Join(root, "other")
	_ = os.MkdirAll(other, 0o755)
	_ = os.Symlink(self, filepath.Join(other, "cargo"))
	if _, bypass := cargoBypass(join(other, rustup), bin, self); bypass {
		t.Error("enlace al envoltorio")
	}
	if first, bypass := cargoBypass(filepath.Join(root, "vacía"), bin, self); first != "" || bypass {
		t.Error("sin cargo en el PATH")
	}
}

func listSep(ds []string) string {
	s := ""
	for _, d := range ds {
		s += string(os.PathListSeparator) + d
	}
	return s
}

func TestConfigTargetDir(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "crates", "a")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.MkdirAll(filepath.Join(ws, ".cargo"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".cargo", "config.toml"), []byte("[build]\ntarget-dir = \"out\"\n"), 0o644)
	if got := configTargetDir(sub, t.TempDir()); got != filepath.Join(ws, "out") {
		t.Errorf("relativa: %s", got)
	}
	ch := t.TempDir()
	_ = os.WriteFile(filepath.Join(ch, "config.toml"), []byte("[build]\ntarget-dir = \"/abs/target\"\n"), 0o644)
	if got := configTargetDir(t.TempDir(), ch); got != "/abs/target" {
		t.Errorf("CARGO_HOME: %s", got)
	}
	// resolveTargetDir: --target-dir, luego CARGO_TARGET_DIR, luego config, luego workspace.
	_ = os.WriteFile(filepath.Join(ws, "Cargo.lock"), []byte("[[package]]\n"), 0o644)
	none := func(string) string { return "" }
	if got := resolveTargetDir([]string{"cargo", "build", "--target-dir", "t2"}, none, sub, ch); got != filepath.Join(sub, "t2") {
		t.Errorf("--target-dir: %s", got)
	}
	env := func(k string) string { return map[string]string{"CARGO_TARGET_DIR": "/env/t"}[k] }
	if got := resolveTargetDir([]string{"cargo", "build"}, env, sub, ch); got != "/env/t" {
		t.Errorf("CARGO_TARGET_DIR: %s", got)
	}
	if got := resolveTargetDir([]string{"cargo", "build"}, none, sub, ch); got != filepath.Join(ws, "out") {
		t.Errorf("config: %s", got)
	}
	_ = os.Remove(filepath.Join(ws, ".cargo", "config.toml"))
	if got := resolveTargetDir([]string{"cargo", "build"}, none, sub, t.TempDir()); got != filepath.Join(ws, "target") {
		t.Errorf("workspace: %s", got)
	}
}
