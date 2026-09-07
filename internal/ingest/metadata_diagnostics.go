package ingest

import (
	"errors"
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
