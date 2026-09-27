package testgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cases cover the anchor locator's fail-closed branches that a committed
// tree cannot express: a file that declares a name twice, a function with more
// than one exec call carrying the fragment, and a function with no exec call.
// The registry fixtures cover the branches that a real file can express.

func writeSiteFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "site.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolveExecSite_LocatesCallInsideNamedFunction proves the locator picks
// the call inside the named function by its fragment, not by a line offset.
func TestResolveExecSite_LocatesCallInsideNamedFunction(t *testing.T) {
	dir := writeSiteFile(t, `package p

import "os/exec"

func first() {
	exec.Command("go", "vet", "./...")
}

func second() {
	exec.Command("go", "build", "-race=false", "-o", "out", "./cmd/app")
}
`)
	anchor := ExecAnchor{File: "site.go", Func: "second", Fragment: `exec.Command("go", "build"`}
	site, err := ResolveExecSite(dir, anchor)
	if err != nil {
		t.Fatalf("ResolveExecSite: %v", err)
	}
	if !site.FlagsKnown {
		t.Fatal("flags should resolve")
	}
	if site.RaceEnabled {
		t.Fatalf("flags %v should not enable the race detector", site.Flags)
	}
	if !contains(site.Flags, "-race=false") {
		t.Fatalf("flags = %v, want -race=false", site.Flags)
	}
}

// TestResolveExecSite_AmbiguousFunctionNameFails proves a name declared twice
// in the file is an error, not a first match.
func TestResolveExecSite_AmbiguousFunctionNameFails(t *testing.T) {
	dir := writeSiteFile(t, `package p

import "os/exec"

type a struct{}
type b struct{}

func (a) run() { exec.Command("go", "build") }
func (b) run() { exec.Command("go", "build") }
`)
	_, err := ResolveExecSite(dir, ExecAnchor{File: "site.go", Func: "run", Fragment: "exec.Command"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
}

// TestResolveExecSite_AmbiguousFragmentFails proves that when more than one call
// in the function carries the fragment, the anchor is an error, not a first
// match.
func TestResolveExecSite_AmbiguousFragmentFails(t *testing.T) {
	dir := writeSiteFile(t, `package p

import "os/exec"

func build() {
	exec.Command("go", "build")
	exec.Command("go", "build")
}
`)
	_, err := ResolveExecSite(dir, ExecAnchor{File: "site.go", Func: "build", Fragment: `exec.Command("go", "build"`})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
}

// TestResolveExecSite_MissingFragmentFails proves the fragment pins the call:
// a present function whose call does not carry the fragment fails closed.
func TestResolveExecSite_MissingFragmentFails(t *testing.T) {
	dir := writeSiteFile(t, `package p

import "os/exec"

func build() {
	exec.Command("go", "build")
}
`)
	_, err := ResolveExecSite(dir, ExecAnchor{File: "site.go", Func: "build", Fragment: "exec.CommandContext"})
	if err == nil || !strings.Contains(err.Error(), "required fragment") {
		t.Fatalf("err = %v, want a missing-fragment error", err)
	}
}

// TestResolveExecSite_NoExecCallFails proves an anchored function with no exec
// call fails closed.
func TestResolveExecSite_NoExecCallFails(t *testing.T) {
	dir := writeSiteFile(t, `package p

func build() {}
`)
	_, err := ResolveExecSite(dir, ExecAnchor{File: "site.go", Func: "build", Fragment: "exec.Command"})
	if err == nil || !strings.Contains(err.Error(), "no exec.Command") {
		t.Fatalf("err = %v, want a no-exec-call error", err)
	}
}
