package harnesslayout

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sort"
)

// RootReport states what a capture found at one root.
type RootReport struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Sessions int    `json:"sessions"`
	Error    string `json:"error,omitempty"`
}

// SessionError records a session whose capture failed.
type SessionError struct {
	Session string `json:"session"`
	Error   string `json:"error"`
}

// Report is the result of capturing one tool on one machine.
type Report struct {
	Tool     Tool           `json:"tool"`
	Roots    []RootReport   `json:"roots"`
	Captures []Capture      `json:"captures"`
	Failures []SessionError `json:"failures,omitempty"`
}

// ShapeOnly returns the report with every identifying value removed: root
// paths, session identifiers, titles, project paths, and timestamps.
func (r Report) ShapeOnly() Report {
	roots := make([]RootReport, len(r.Roots))
	for i, root := range r.Roots {
		root.Path = ""
		roots[i] = root
	}
	captures := make([]Capture, len(r.Captures))
	for i, capture := range r.Captures {
		captures[i] = capture.WithoutMetadataValues()
	}
	failures := make([]SessionError, len(r.Failures))
	for i, failure := range r.Failures {
		failure.Session = ""
		failures[i] = failure
	}
	r.Roots, r.Captures, r.Failures = roots, captures, failures
	return r
}

// OSSource opens an operating system directory as a Source.
func OSSource(dir string) Source {
	return Source{FS: os.DirFS(dir), Dir: dir}
}

// Run discovers the sessions of every path and captures up to limit of them,
// ordered by root and then by session identifier. A limit of zero or less
// captures every session. A missing root is reported, not an error; a failed
// session is recorded in Failures and the run continues.
func Run(ctx context.Context, layout Layout, paths []string, open func(string) Source, limit int) (Report, error) {
	report := Report{Tool: layout.Tool, Captures: []Capture{}}
	type pending struct {
		src Source
		ref SessionRef
	}
	var queue []pending
	for _, path := range paths {
		root := RootReport{Path: path}
		src := open(path)
		if _, err := fs.Stat(src.FS, "."); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				root.Error = err.Error()
			}
			report.Roots = append(report.Roots, root)
			continue
		}
		root.Present = true
		refs, err := layout.Probe.Discover(ctx, src)
		if err != nil {
			root.Error = err.Error()
		}
		root.Sessions = len(refs)
		report.Roots = append(report.Roots, root)
		sort.SliceStable(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
		for _, ref := range refs {
			queue = append(queue, pending{src: src, ref: ref})
		}
	}
	for _, item := range queue {
		if limit > 0 && len(report.Captures) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		capture, err := layout.Probe.Capture(ctx, item.src, item.ref)
		if err != nil {
			report.Failures = append(report.Failures, SessionError{Session: item.ref.ID, Error: err.Error()})
			continue
		}
		capture.Tool = layout.Tool
		if capture.Session == "" {
			capture.Session = item.ref.ID
		}
		report.Captures = append(report.Captures, capture)
	}
	return report, nil
}
