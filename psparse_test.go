package main

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

// Salida real de `LC_ALL=C ps -axww -o pid=,ppid=,uid=,rss=,%cpu=,time=,state=,args=`
// en macOS (con rutas genéricas).
const psSample = `    1     0     0  13456   0.0  12:34.56 Ss   /sbin/launchd
  321     1     0   2048   0.1   0:01.02 Ss   (logd)
  812     1   501  51200   1.5   1:02:03.45 S    /Applications/Some App.app/Contents/MacOS/Some App --flag
 4242   812   501 204800 312.4   0:45.10 R+   /Users/dev/.rustup/toolchains/stable-aarch64-apple-darwin/bin/rustc --crate-name serde_json --edition=2021 src/lib.rs
 4243  4242   501    512   0.0   0:00.00 T    /Users/dev/proj/target/debug/build/ring-0123456789abcdef/build-script-build
 4300     1   501      0   0.0   0:00.00 Z    <defunct>
 4400     1   501   9000   0,5   2-01:00:00 S    claude
garbage line
`

func TestParsePS(t *testing.T) {
	rows := parsePS(psSample)
	if len(rows) != 7 {
		t.Fatalf("%d filas, quería 7: %+v", len(rows), rows)
	}
	r := rows[812]
	if r.ppid != 1 || r.uid != 501 || r.rssKB != 51200 || r.state != "S" {
		t.Errorf("812: %+v", r)
	}
	if math.Abs(r.cpuSecs-3723.45) > 1e-6 {
		t.Errorf("812 cpu: %v", r.cpuSecs)
	}
	if want := []string{"/Applications/Some", "App.app/Contents/MacOS/Some", "App", "--flag"}; !reflect.DeepEqual(r.args, want) {
		t.Errorf("812 args: %q", r.args)
	}
	if rows[321].args != nil {
		t.Errorf("(logd) debería quedar sin argumentos: %q", rows[321].args)
	}
	rc := rows[4242]
	if rc.pcpu != 312.4 || rc.cpuSecs != 45.1 || argValue(rc.args, "--crate-name") != "serde_json" {
		t.Errorf("rustc: %+v", rc)
	}
	if rows[4243].state != "T" || rows[4300].state != "Z" {
		t.Errorf("estados: %q %q", rows[4243].state, rows[4300].state)
	}
	if c := rows[4400]; c.pcpu != 0.5 || c.cpuSecs != 2*86400+3600 {
		t.Errorf("coma decimal / días: %+v", c)
	}
	if rows[1].cpuSecs != 754.56 {
		t.Errorf("launchd: %v", rows[1].cpuSecs)
	}
}

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]float64{
		"0:00.03": 0.03, "12:34.56": 754.56, "1:02:03.45": 3723.45, "2-01:02:03": 2*86400 + 3723, "0:01,50": 1.5,
	} {
		if got, ok := parseCPUTime(in); !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("%q: %v %v, quería %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "1", "1.5:00", "x-1:00", "1:2:3:4"} {
		if _, ok := parseCPUTime(bad); ok {
			t.Errorf("%q debería fallar", bad)
		}
	}
}

func TestParseLsofCwd(t *testing.T) {
	out := "p4242\nfcwd\nn/Users/dev/proyecto con espacios\np4300\nfcwd\nn/\n"
	got := parseLsofCwd(out)
	want := map[int]string{4242: "/Users/dev/proyecto con espacios", 4300: "/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
	if len(parseLsofCwd("")) != 0 {
		t.Error("vacío")
	}
}

func TestParseProcArgs2(t *testing.T) {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = append(b, "/usr/bin/rustc\x00\x00\x00\x00"...)
	b = append(b, "rustc\x00--crate-name\x00mi crate\x00HOME=/Users/dev\x00PATH=/usr/bin\x00"...)
	got := parseProcArgs2(b)
	if want := []string{"rustc", "--crate-name", "mi crate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%q", got)
	}
	if parseProcArgs2(nil) != nil || parseProcArgs2([]byte{1, 0, 0, 0}) != nil {
		t.Error("datos rotos")
	}
}

const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3000.
Pages active:                            200000.
Pages inactive:                          190000.
Pages speculative:                         5000.
Pages throttled:                              0.
Pages wired down:                        100000.
Pages purgeable:                          10000.
"Translation faults":                 123456789.
Pages copy-on-write:                    5555555.
Anonymous pages:                         250000.
Pages occupied by compressor:             50000.
`

func TestParseVMStat(t *testing.T) {
	used, ok := parseVMStat(vmStatSample)
	if want := uint64((250000 - 10000 + 100000 + 50000) * 16384); !ok || used != want {
		t.Errorf("%d %v, quería %d", used, ok, want)
	}
	// Sin "Anonymous pages" (macOS viejo): la activa.
	old := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages active: 10.\nPages wired down: 5.\n"
	if used, ok := parseVMStat(old); !ok || used != 15*4096 {
		t.Errorf("viejo: %d %v", used, ok)
	}
	if _, ok := parseVMStat("nada"); ok {
		t.Error("sin tamaño de página debería fallar")
	}
}

func TestParseLoadAvg(t *testing.T) {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:], 3072) // 1.5 con fscale 2048
	binary.LittleEndian.PutUint64(b[16:], 2048)
	if got := parseLoadAvg(b); got != 1.5 {
		t.Errorf("%v", got)
	}
	if parseLoadAvg(nil) != 0 {
		t.Error("vacío")
	}
}
