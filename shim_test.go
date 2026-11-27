package main

import (
	"reflect"
	"testing"
)

func TestDropJobFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		sin  []string
		qued []string
	}{
		{name: "sin banderas", in: []string{"build", "--release"}, sin: []string{"build", "--release"}},
		{name: "j separada", in: []string{"build", "-j", "24"}, sin: []string{"build"}, qued: []string{"-j 24"}},
		{name: "j pegada", in: []string{"build", "-j24"}, sin: []string{"build"}, qued: []string{"-j24"}},
		{name: "jobs separada", in: []string{"test", "--jobs", "8"}, sin: []string{"test"}, qued: []string{"--jobs 8"}},
		{name: "jobs con igual", in: []string{"test", "--jobs=8"}, sin: []string{"test"}, qued: []string{"--jobs=8"}},
		{name: "nproc de shell", in: []string{"build", "-j", "24", "-p", "motor"}, sin: []string{"build", "-p", "motor"}, qued: []string{"-j 24"}},
		{name: "bandera al final", in: []string{"build", "-p", "motor", "-j", "24"}, sin: []string{"build", "-p", "motor"}, qued: []string{"-j 24"}},
		{name: "dos banderas", in: []string{"build", "-j", "24", "--jobs=4"}, sin: []string{"build"}, qued: []string{"-j 24", "--jobs=4"}},
		{name: "j sin valor", in: []string{"build", "-j"}, sin: []string{"build"}, qued: []string{"-j"}},
		{name: "j seguida de otra bandera", in: []string{"build", "-j", "--release"}, sin: []string{"build", "--release"}, qued: []string{"-j"}},
		{name: "argumentos del programa intactos", in: []string{"run", "--bin", "motor", "--", "-j", "4"}, sin: []string{"run", "--bin", "motor", "--", "-j", "4"}},
		{name: "bandera antes de la separacion", in: []string{"run", "-j", "4", "--", "-j", "9"}, sin: []string{"run", "--", "-j", "9"}, qued: []string{"-j 4"}},
		{name: "vacio", in: []string{}, sin: []string{}, qued: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sin, qued := dropJobFlags(tc.in)
			if !reflect.DeepEqual(sin, tc.sin) {
				t.Fatalf("argumentos: quería %v, quedó %v", tc.sin, sin)
			}
			if !reflect.DeepEqual(qued, tc.qued) {
				t.Fatalf("quitadas: quería %v, quedó %v", tc.qued, qued)
			}
		})
	}
}
