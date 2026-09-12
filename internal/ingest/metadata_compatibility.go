package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// metadataNeedsNativeRefresh reports whether a stored sidecar has to be rebuilt
// from native data before this build can rely on it. It is the one rule, stated
// once in metadata.go: a version whose only difference from the current one is
// optional fields is read as it stands, and anything older is refreshed.
//
// An unreadable version is treated as needing a refresh. That is the safe
// answer: it sends the session down the reporting refresh path, which preserves
// the existing artifact and index, rather than certifying a version this build
// cannot name.
func metadataNeedsNativeRefresh(version int) bool {
	recorded, err := newMetadataSchemaVersion(version)
	if err != nil {
		return true
	}
	return metadataNeedsRefresh(recorded, CurrentSchemaVersion)
}

func managedInputIOError(path string, err error) error {
	return fmt.Errorf("inspect managed input %q before refresh or indexing: %w; its compatibility could not be checked, so this session's artifact replacement and native fallback were refused; restore read access or retry after the I/O failure is resolved", path, err)
}

// checkStoredMetadataVersion reads actual stored state before refresh or index
// work. Discovery's cache may be incomplete after a failed prefetch, and retained
// maintenance also processes sessions absent from native discovery.
func (p *Pipeline) checkStoredMetadataVersion(ctx context.Context, sid SessionID) error {
	return p.checkStoredMetadataCompatibility(ctx, sid, nil)
}

func (p *Pipeline) checkStoredRewriteVersion(ctx context.Context, sid SessionID, harness Harness) error {
	target := p.versionTargets()[harness].AdapterVersion
	return p.checkStoredMetadataCompatibility(ctx, sid, &target)
}

func (p *Pipeline) checkStoredMetadataCompatibility(ctx context.Context, sid SessionID, adapterTarget *int) error {
	backing := p.store
	if backing == nil {
		backing, _ = p.metricsStore.(SessionStore)
	}
	if backing == nil {
		return nil // File-only mode has no stored metadata version.
	}
	locations, err := backing.BulkLookupSessionLocations(ctx, []SessionID{sid})
	if err != nil {
		return fmt.Errorf("read stored metadata compatibility for session %s before refresh or indexing: %w; compatibility could not be verified, so this operation was refused without changing session artifacts or index; restore database access and retry", sid, err)
	}
	if location, ok := locations[sid]; ok && location.SchemaVersion > CurrentSchemaVersion {
		return &UnsupportedMetadataVersionError{Path: string(sid) + " (stored metadata)", Version: location.SchemaVersion}
	}
	if location, ok := locations[sid]; ok && adapterTarget != nil && location.AdapterVersion != nil && *location.AdapterVersion > *adapterTarget {
		return &AdapterVersionError{Path: string(sid) + " (stored metadata)", Version: *location.AdapterVersion, Target: *adapterTarget}
	}
	return nil
}

// managedRelativePath is the one spelling a managed file gets in anything the
// user reads: its path under the managed output directory, which is what the
// index selection reports and what a user can locate without knowing where the
// directory itself lives. A path outside the managed tree is left alone.
func (p *Pipeline) managedRelativePath(path string) string {
	relative, err := filepath.Rel(string(p.config.OutputDir), path)
	if err != nil || relative == "" || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}

// metadataCompatibilityCause unwraps err to the compatibility refusal inside
// it, if there is one.
//
// This is the ONE place the closed list of compatibility refusals is written.
// Callers that only ask whether err is one use isMetadataCompatibilityError;
// callers that REPORT it want the cause rather than whatever operation met it,
// because diagnostics collapse by whole-value equality and a wrapper is one
// more spelling of one cause. Returning the cause from the same list they test
// against means a type added here cannot be recognised by one and missed by
// the other.
func metadataCompatibilityCause(err error) (error, bool) {
	var schemaErr *UnsupportedMetadataVersionError
	if errors.As(err, &schemaErr) {
		return schemaErr, true
	}
	var adapterErr *AdapterVersionError
	if errors.As(err, &adapterErr) {
		return adapterErr, true
	}
	var headerErr *MetadataHeaderError
	if errors.As(err, &headerErr) {
		return headerErr, true
	}
	return nil, false
}

func isMetadataCompatibilityError(err error) bool {
	_, ok := metadataCompatibilityCause(err)
	return ok
}

// MetadataHeaderError distinguishes an unreadable compatibility field from
// historic body corruption. Invalid version evidence never authorizes recovery.
type MetadataHeaderError struct {
	Path  string
	Cause error
}

func (e *MetadataHeaderError) Error() string {
	return fmt.Sprintf("read managed metadata %s: compatibility header is invalid (%v); its schema cannot be verified, so artifacts and index were preserved; restore valid version metadata or use a compatible Peasant build before retrying", e.Path, e.Cause)
}

// UnsupportedMetadataVersionError distinguishes a newer artifact from a missing
// one. Callers must not fall back to native source reconstruction on this error.
type UnsupportedMetadataVersionError struct {
	Path    string
	Version int
}

func (e *UnsupportedMetadataVersionError) Error() string {
	return fmt.Sprintf("read managed metadata %s: schema version %d is newer than supported version %d; this session's artifact and index were not changed; upgrade Peasant before refreshing or indexing it", e.Path, e.Version, CurrentSchemaVersion)
}

// AdapterVersionError refuses invalid producer evidence or an adapter rewrite
// that would erase a newer producer's work. A future positive adapter revision
// does not by itself prevent reading supported metadata for retained indexing.
type AdapterVersionError struct {
	Path    string
	Version int
	Target  int
	Cause   error
}

func (e *AdapterVersionError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("read managed metadata %s: adapterVersion is invalid (%v); producer evidence was not overwritten; restore a positive integer revision or omit the field for unknown provenance", e.Path, e.Cause)
	}
	if e.Version <= 0 {
		return fmt.Sprintf("read managed metadata %s: adapterVersion %d is not positive; producer evidence is invalid and this session was not changed; restore valid metadata or omit unknown historical provenance", e.Path, e.Version)
	}
	return fmt.Sprintf("refresh managed metadata %s: recorded adapter revision %d is newer than this harness's adapter revision %d; native extraction and artifact replacement were refused to preserve producer evidence; upgrade Peasant before refreshing this session", e.Path, e.Version, e.Target)
}

type managedMetadataHeader struct {
	SchemaVersion  int
	AdapterVersion *int
}

// decodeManagedMetadataHeader validates producer evidence without depending on
// the metadata body's readability. An unrelated malformed field cannot discard
// a known schema or adapter revision before a caller decides whether to rewrite.
func decodeManagedMetadataHeader(data []byte, path string) (*managedMetadataHeader, error) {
	var header struct {
		SchemaVersion  json.RawMessage `json:"schemaVersion"`
		AdapterVersion json.RawMessage `json:"adapterVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	decoded := &managedMetadataHeader{}
	var schemaErr error
	if len(header.SchemaVersion) > 0 {
		schemaErr = json.Unmarshal(header.SchemaVersion, &decoded.SchemaVersion)
	}
	if schemaErr == nil && decoded.SchemaVersion > CurrentSchemaVersion {
		return nil, &UnsupportedMetadataVersionError{Path: path, Version: decoded.SchemaVersion}
	}
	if len(header.AdapterVersion) > 0 {
		if bytes.Equal(bytes.TrimSpace(header.AdapterVersion), []byte("null")) {
			return nil, &AdapterVersionError{Path: path, Cause: errors.New("null is not a recorded producer revision")}
		}
		var adapterVersion int
		if err := json.Unmarshal(header.AdapterVersion, &adapterVersion); err != nil {
			return nil, &AdapterVersionError{Path: path, Cause: err}
		}
		if adapterVersion <= 0 {
			return nil, &AdapterVersionError{Path: path, Version: adapterVersion}
		}
		decoded.AdapterVersion = &adapterVersion
	}
	// Keep a valid producer revision even when the other header field is
	// malformed; callers must evaluate that evidence before corruption recovery.
	if schemaErr != nil {
		return decoded, &MetadataHeaderError{Path: path, Cause: schemaErr}
	}
	return decoded, nil
}

func decodeManagedMetadata(data []byte, path string) (*UnifiedMetadata, error) {
	if _, err := decodeManagedMetadataHeader(data, path); err != nil {
		return nil, err
	}
	return decodeManagedMetadataBody(data)
}

func decodeManagedMetadataBody(data []byte) (*UnifiedMetadata, error) {
	var meta UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// metadataForRewrite runs before --force or any adapter/source access. Corrupt
// older metadata retains the existing re-extraction policy; known incompatible
// metadata is not corruption and must never be overwritten by that recovery path.
//
// ctx is the run's context, never a fresh background one: the metadata lookup
// walks the managed output directory, so a cancelled run must stop reading the
// filesystem here as it does everywhere else.
func (p *Pipeline) metadataForRewrite(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.SchemaVersion > CurrentSchemaVersion {
		return nil, &UnsupportedMetadataVersionError{Path: string(session.SessionID) + " (stored metadata)", Version: loc.SchemaVersion}
	}
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.AdapterVersion != nil {
		target := p.versionTargets()[session.Harness].AdapterVersion
		if *loc.AdapterVersion > target {
			return nil, &AdapterVersionError{Path: string(session.SessionID) + " (stored metadata)", Version: *loc.AdapterVersion, Target: target}
		}
	}
	// Database-first: when the row already reports a supported schema, an
	// adapter revision this build accepts, and the ingested clock the freshness
	// decision needs, the metadata file would only re-derive what the row
	// already carries. Skip reading it and let the caller's DB-first freshness
	// branch classify from the row. A version skew between the file and its row
	// is a torn or mixed pair, which is detected on an actual pair read
	// (harvest index, the content stage, redact), not on this classify. The
	// file read below stays as the fallback for a legacy row the database
	// cannot answer for.
	if loc, ok := p.locationCache[session.SessionID]; ok &&
		loc.SchemaVersion >= 1 && loc.SchemaVersion <= CurrentSchemaVersion &&
		loc.AdapterVersion != nil && *loc.AdapterVersion <= p.versionTargets()[session.Harness].AdapterVersion &&
		loc.IngestedMs != nil && *loc.IngestedMs > 0 {
		return nil, nil
	}
	path, err := p.findMetadataPath(ctx, session)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	data, err := p.fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, managedInputIOError(p.managedRelativePath(path), err)
	}
	// Name the file the way the index selection names it. Both checks refuse
	// the same file for the same reason with the same remedy, and diagnostics
	// collapse by whole-value equality, so a second spelling of the location is
	// the one difference that turns one refusal into two warnings.
	reported := p.managedRelativePath(path)
	header, err := decodeManagedMetadataHeader(data, reported)
	if header != nil && header.AdapterVersion != nil {
		target := p.versionTargets()[session.Harness].AdapterVersion
		if *header.AdapterVersion > target {
			return nil, &AdapterVersionError{Path: reported, Version: *header.AdapterVersion, Target: target}
		}
	}
	if err != nil {
		if isMetadataCompatibilityError(err) {
			return nil, err
		}
		return nil, nil
	}
	meta, err := decodeManagedMetadataBody(data)
	if err != nil {
		// Historic corruption remains recoverable only after the independently
		// decoded header has ruled out an incompatible producer.
		return nil, nil
	}
	return meta, nil
}
