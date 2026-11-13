package main

import (
	"os"
	"path/filepath"
	"strings"
)

// gitInfo: el proyecto original, el worktree (si la carpeta es un worktree de git) y
// la rama, subiendo desde dir hasta encontrar .git.
//   - repo normal: .git es un directorio → proyecto = esa carpeta, sin worktree.
//   - worktree: .git es un archivo "gitdir: <repo>/.git/worktrees/<nombre>" →
//     proyecto = <repo>, worktree = <nombre>.
func gitInfo(dir string) (project, worktree, branch string) {
	for d := dir; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		dotgit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotgit)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			return filepath.Base(d), "", headBranch(dotgit)
		}
		b, err := os.ReadFile(dotgit)
		if err != nil {
			return filepath.Base(d), "", ""
		}
		gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(d, gitdir)
		}
		// <repo>/.git/worktrees/<nombre>
		if filepath.Base(filepath.Dir(gitdir)) == "worktrees" {
			main := filepath.Dir(filepath.Dir(filepath.Dir(gitdir)))
			return filepath.Base(main), filepath.Base(gitdir), headBranch(gitdir)
		}
		return filepath.Base(d), "", headBranch(gitdir)
	}
	return "", "", ""
}

func headBranch(gitdir string) string {
	b, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "ref: refs/heads/") {
		return strings.TrimPrefix(s, "ref: refs/heads/")
	}
	if len(s) >= 7 {
		return s[:7] // HEAD separado: el commit
	}
	return s
}

// shortWorktree acorta los nombres largos de worktrees de agentes.
func shortWorktree(w string) string {
	if strings.HasPrefix(w, "agent-") && len(w) > 14 {
		return w[:14] + "…"
	}
	return w
}

// gitRoot: la carpeta del repo o worktree (la que tiene .git) que contiene dir.
func gitRoot(dir string) string {
	for d := dir; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}
	return ""
}

// place: dónde, en pocas palabras: la rama; si no tiene nombre útil (HEAD
// separado o una rama generada tipo worktree-agent-…), el worktree recortado.
func place(worktree, branch string) string {
	generated := strings.HasPrefix(branch, "worktree-") || branch == worktree || isHash(branch)
	switch {
	case branch != "" && !generated:
		return branch
	case worktree != "":
		return shortWorktree(worktree)
	}
	return branch
}

func isHash(s string) bool {
	if len(s) != 7 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// placeLabel: "proyecto ⎇ rama" (o solo el proyecto si no es un repo de git).
func placeLabel(project, worktree, branch string) string {
	if p := place(worktree, branch); p != "" {
		return project + " ⎇ " + p
	}
	return project
}
