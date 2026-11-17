package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Las unidades terminadas cuentan por sus huellas, también las que vinieron de la
// caché (no hubo rustc que muestrear); las de antes del cargo no.
func TestFingerprintDone(t *testing.T) {
	target := t.TempDir()
	start := time.Now()
	old := start.Add(-time.Hour)
	unit := func(rel, file string, mt time.Time) {
		d := filepath.Join(target, rel)
		_ = os.MkdirAll(d, 0o755)
		p := filepath.Join(d, file)
		_ = os.WriteFile(p, []byte("{}"), 0o644)
		_ = os.Chtimes(p, mt, mt)
	}
	unit("debug/.fingerprint/serde-1111", "lib-serde.json", start.Add(time.Second))
	unit("debug/.fingerprint/tokio-2222", "lib-tokio.json", start.Add(2*time.Second))
	unit("debug/.fingerprint/app-3333", "bin-app.json", old)                                                   // al día: no se tocó
	unit("debug/.fingerprint/build-4444", "invoked.timestamp", start.Add(time.Second))                         // empezó, no terminó
	unit("x86_64-unknown-linux-gnu/release/.fingerprint/libc-5555", "lib-libc.json", start.Add(3*time.Second)) // con --target
	seen := map[string]bool{}
	if n := fingerprintDone(target, start, seen); n != 3 {
		t.Fatalf("%d unidades, quería 3", n)
	}
	unit("debug/.fingerprint/app-3333", "bin-app.json", start.Add(4*time.Second))
	if n := fingerprintDone(target, start, seen); n != 4 {
		t.Fatalf("tras terminar app: %d, quería 4", n)
	}
	if n := fingerprintDone(filepath.Join(target, "no-existe"), start, map[string]bool{}); n != 0 {
		t.Errorf("sin target: %d", n)
	}
}

func TestDoneTakesTheLargerCount(t *testing.T) {
	b := &Build{seen: map[string]bool{"a@": true, "b@": true}}
	b.fpDone = 40
	b.updateDone()
	if b.Done != 40 {
		t.Errorf("Done %d", b.Done)
	}
	b.fpDone = 1
	b.updateDone()
	if b.Done != 2 {
		t.Errorf("Done %d", b.Done)
	}
	if !isRustcWrapper(&proc{comm: "kache"}) || isRustcWrapper(&proc{comm: "rustc"}) {
		t.Error("isRustcWrapper")
	}
}
