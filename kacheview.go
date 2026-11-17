package main

// Cómo se ve kache en el panel y en `turn_watch once`: una línea con el porcentaje de
// aciertos de las últimas 24 h, el tiempo ahorrado, el almacén y el daemon, y debajo
// (si hay sitio) los totales, lo que dice `kache doctor` y el último error. Si no hay
// datos se dice por qué; nunca un 0 % inventado.

import (
	"fmt"
	"strings"
	"time"
)

// Gravedad de una línea.
const (
	sevOK = iota
	sevInfo
	sevWarn
	sevError
)

func bytesText(n uint64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MB", n>>20)
}

func msText(ms uint64) string { return dur(time.Duration(ms) * time.Millisecond) }

// kacheLine: el resumen de una línea y su gravedad.
func kacheLine(k *KacheStatus) (string, int) {
	if k == nil {
		return "kache: sin datos (el servicio es de una versión anterior o acaba de arrancar)", sevInfo
	}
	switch k.State {
	case kacheDisabled:
		return "kache: " + k.Detail, sevInfo
	case kacheOK:
	case kacheDaemonDown, kacheUnconfig, kacheConflict:
		return "kache: " + k.Detail, sevWarn
	default:
		return "kache: " + k.Detail, sevError
	}
	s := k.Stats
	var parts []string
	if pct, ok := s.Ratio(); ok {
		parts = append(parts, fmt.Sprintf("%.0f %% aciertos (%d/%d en %s)", pct, s.Hits(), s.Total(), s.Window))
	} else {
		parts = append(parts, fmt.Sprintf("sin compilaciones en %s", s.Window))
	}
	parts = append(parts, "ahorró "+msText(s.TimeSavedMs))
	store := "almacén " + bytesText(s.StoreBytes)
	if s.StoreLimitBytes > 0 {
		store += " de " + bytesText(s.StoreLimitBytes)
	}
	parts = append(parts, store, "daemon ● "+k.DaemonVersion)
	return "kache " + strings.Join(parts, " · "), sevOK
}

// kacheDetail: las líneas de debajo, con su gravedad.
func kacheDetail(k *KacheStatus, now time.Time) (lines []string, sev []int) {
	if k == nil || k.State == kacheDisabled {
		return nil, nil
	}
	add := func(s string, v int) { lines, sev = append(lines, s), append(sev, v) }
	if s := k.Stats; s != nil {
		d := fmt.Sprintf("locales %d · prefetch %d · remotos %d · duplicados %d · fallos %d · %d entradas", s.LocalHits, s.PrefetchHits, s.RemoteHits, s.Dups, s.Misses, s.Entries)
		add(d, sevInfo)
		if l := s.Lifetime; l != nil {
			add(fmt.Sprintf("desde %s: %d aciertos, ahorró %s", l.Since.Local().Format("2006-01-02"), l.Hits, msText(l.TimeSavedMs)), sevInfo)
		}
	} else if k.StatsErr != "" && k.State == kacheOK {
		add("estadísticas de kache no disponibles: "+k.StatsErr, sevWarn)
	}
	switch {
	case k.Doctor != nil && k.Doctor.Issues > 0:
		add(fmt.Sprintf("kache doctor: %d problema(s): %s", k.Doctor.Issues, strings.Join(k.Doctor.Failed, "; ")), sevWarn)
	case k.DoctorErr != "":
		add("kache doctor no disponible: "+k.DoctorErr, sevWarn)
	}
	if k.LastError != "" && !k.LastErrorAt.IsZero() {
		add(fmt.Sprintf("último error (hace %s): %s", dur(now.Sub(k.LastErrorAt)), k.LastError), sevInfo)
	}
	return lines, sev
}

func sevStyle(v int) string {
	switch v {
	case sevInfo:
		return dim
	case sevWarn:
		return yellow
	case sevError:
		return bold + red
	}
	return ""
}
