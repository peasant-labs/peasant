package ingest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// reportMetadataRefusal retains nonfatal warnings independently from slog,
// which interactive callers may suppress while rendering progress.
func (p *Pipeline) reportMetadataRefusal(location string, err error) {
	remedy := "Restore readable, compatible managed metadata and retry."
	var schemaErr *UnsupportedMetadataVersionError
	var adapterErr *AdapterVersionError
	if errors.As(err, &schemaErr) || errors.As(err, &adapterErr) && adapterErr.Version > 0 {
		remedy = "Upgrade Peasant to a build compatible with the recorded producer/schema version."
	}
	diagnostic := DiagnosticEntry{ErrorType: "metadata_refused", Location: location, Message: err.Error(), Remediation: remedy}
	p.reportDiagnostic(diagnostic)
}

// reportDiagnostic reports one entry to the user, once.
//
// Identical entries collapse: "exactly one warning for one cause" rests on
// EQUALITY BY VALUE of the whole entry, so several paths that meet the same
// cause must build the same entry: same type, same location, same message,
// same remedy. Two reporters that describe one refusal in different words, or
// that name the same file by different paths, produce two warnings for the
// user however alike they read. Route a shared cause through its own reporter
// (reportMetadataRefusal for stored-metadata compatibility) rather than
// restating it at each site.
func (p *Pipeline) reportDiagnostic(diagnostic DiagnosticEntry) {
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	if p.diagnosticSet == nil {
		p.diagnosticSet = make(map[DiagnosticEntry]struct{})
	}
	if _, seen := p.diagnosticSet[diagnostic]; seen {
		return
	}
	p.diagnosticSet[diagnostic] = struct{}{}
	p.diagnostics = append(p.diagnostics, diagnostic)
}

func (p *Pipeline) reportIndexRefusal(sessionID SessionID, err error) {
	p.reportDiagnostic(DiagnosticEntry{
		ErrorType: "index_refused", Location: string(sessionID),
		Message:     fmt.Sprintf("index session %s: %v; this attempt preserved the last successful index", sessionID, err),
		Remediation: "Use a compatible Peasant build or fix the reported parser/input/store problem, then retry harvest.",
	})
}

func (p *Pipeline) resetDiagnostics() {
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	p.diagnostics = nil
	p.diagnosticSet = nil
}

func (p *Pipeline) snapshotDiagnostics() []DiagnosticEntry {
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	diagnostics := slices.Clone(p.diagnostics)
	slices.SortFunc(diagnostics, func(a, b DiagnosticEntry) int {
		if order := strings.Compare(a.Location, b.Location); order != 0 {
			return order
		}
		return strings.Compare(a.Message, b.Message)
	})
	return diagnostics
}
