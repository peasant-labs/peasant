package ingest

import (
	"bytes"
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

func decodeManagedMetadata(data []byte, path string) (*UnifiedMetadata, error) {
	// Read the version before decoding fields that a future schema may change.
	var header struct {
		SchemaVersion  int             `json:"schemaVersion"`
		AdapterVersion json.RawMessage `json:"adapterVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	if header.SchemaVersion > CurrentSchemaVersion {
		return nil, &UnsupportedMetadataVersionError{Path: path, Version: header.SchemaVersion}
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
	}
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
	meta, err := decodeManagedMetadata(data, path)
	if err != nil {
		var schemaErr *UnsupportedMetadataVersionError
		var adapterErr *AdapterVersionError
		if errors.As(err, &schemaErr) || errors.As(err, &adapterErr) {
			return nil, err
		}
		return nil, nil
	}
	if meta.AdapterVersion != nil {
		target := p.versionTargets()[session.Harness].AdapterVersion
		if *meta.AdapterVersion > target {
			return nil, &AdapterVersionError{Path: path, Version: *meta.AdapterVersion, Target: target}
		}
	}
	return meta, nil
}
