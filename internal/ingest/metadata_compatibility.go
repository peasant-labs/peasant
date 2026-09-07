package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

// nativeRefreshMetadataVersion is the last metadata change that required
// re-extracting native data. Version 10 adds optional producer evidence; reading
// version 9 does not require source access, a rewrite, or a guessed adapter stamp.
const nativeRefreshMetadataVersion = 9

func metadataNeedsNativeRefresh(version int) bool {
	return version < nativeRefreshMetadataVersion
}

func managedInputIOError(path string, err error) error {
	return fmt.Errorf("inspect managed input %q before refresh or indexing: %w; its compatibility could not be checked, so this session's artifact replacement and native fallback were refused; restore read access or retry after the I/O failure is resolved", path, err)
}

// checkStoredMetadataVersion reads actual stored state before refresh or index
// work. Discovery's cache may be incomplete after a failed prefetch, and retained
// maintenance also processes sessions absent from native discovery.
func (p *Pipeline) checkStoredMetadataVersion(ctx context.Context, sid SessionID) error {
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
	return nil
}

func isMetadataCompatibilityError(err error) bool {
	var schemaErr *UnsupportedMetadataVersionError
	var adapterErr *AdapterVersionError
	var headerErr *MetadataHeaderError
	return errors.As(err, &schemaErr) || errors.As(err, &adapterErr) || errors.As(err, &headerErr)
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
func (p *Pipeline) metadataForRewrite(session DiscoveredSession) (*UnifiedMetadata, error) {
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.SchemaVersion > CurrentSchemaVersion {
		return nil, &UnsupportedMetadataVersionError{Path: string(session.SessionID) + " (stored metadata)", Version: loc.SchemaVersion}
	}
	path, err := p.findMetadataPath(session)
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
		return nil, managedInputIOError(path, err)
	}
	header, err := decodeManagedMetadataHeader(data, path)
	if header != nil && header.AdapterVersion != nil {
		target := p.versionTargets()[session.Harness].AdapterVersion
		if *header.AdapterVersion > target {
			return nil, &AdapterVersionError{Path: path, Version: *header.AdapterVersion, Target: target}
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
