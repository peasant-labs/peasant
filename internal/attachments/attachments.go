package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/peasant-labs/schema"
)

// MaxImageBytes is the largest recorded image this build stores. A larger
// png or jpeg keeps metadata and a placeholder, never the bytes.
const MaxImageBytes = 16 << 20

// DirName is the data-directory child that holds content-addressed image files.
const DirName = "attachments"

// Row is one session_attachments record. The key is (SessionID, SourceEntryRef).
// SHA256 is empty when no bytes were stored. PartitionID is recorded so a
// reader can tell which partition owned the source ref; it is not part of the
// key, because a source ref is unique within a session.
type Row struct {
	SessionID      string
	SourceEntryRef string
	PartitionID    string
	SHA256         string
	MediaType      string
	ByteSize       int
	Width          int
	Height         int
	OmittedReason  string
}

// Rows is the database half of the attachment store. The file half lives beside
// it so image bytes never enter SQLite.
type Rows interface {
	UpsertAttachment(ctx context.Context, row Row) error
	AttachmentBySource(ctx context.Context, sessionID, sourceRef string) (Row, bool, error)
	AttachmentsBySHA(ctx context.Context, sha string) ([]Row, error)
	ReferencedSHAs(ctx context.Context) (map[string]struct{}, error)
	DeleteSessionAttachments(ctx context.Context, sessionID string) error
}

// Pending is one image the indexer is ready to store. Bytes are nil when the
// image is omitted. The caller has already classified media and size.
type Pending struct {
	Row   Row
	Bytes []byte
}

// Hooks are the deterministic rendezvous points a test uses to prove GC cannot
// pass a writer that still holds the store lock. Production leaves them nil.
type Hooks struct {
	// GCBeforeLock runs immediately before GC acquires the shared lock.
	GCBeforeLock func()
	// GCAfterLock runs immediately after GC acquires the shared lock.
	GCAfterLock func()
	// PutAfterFsync runs after the temp file is fsynced and before rename and
	// the row insert, while Put still holds the shared lock.
	PutAfterFsync func()
}

// Store is the content-addressed image store. Put and GC share one lock so a
// garbage collection cannot delete a blob whose row is about to be inserted.
type Store struct {
	dir   string
	rows  Rows
	hooks Hooks
	mu    sync.Mutex
}

// New returns a store whose files live in dir and whose rows go through rows.
func New(dir string, rows Rows, hooks Hooks) *Store {
	return &Store{dir: dir, rows: rows, hooks: hooks}
}

// Dir returns the content-addressed file directory.
func (s *Store) Dir() string { return s.dir }

// ObjectPath is the file path of one content address. It rejects anything that
// is not a lowercase 64-hex digest so a caller cannot escape the directory.
func (s *Store) ObjectPath(sha string) (string, error) {
	if !validSHA(sha) {
		return "", fmt.Errorf("attachment path rejected at attachments.Store.ObjectPath while resolving %q: the value is not a lowercase 64-hex content address, so it cannot name a file under the attachment directory; pass the sha256 hex digest of the stored bytes", sha)
	}
	return filepath.Join(s.dir, sha), nil
}

// Put stores one image. Accepted bytes are written temp, fsynced, and renamed
// before the row is inserted, all under the lock shared with GC. An image that
// is omitted writes no file. A sha256 that already has a file is not rewritten.
func (s *Store) Put(ctx context.Context, pending Pending) (Row, error) {
	if pending.Row.SessionID == "" || pending.Row.SourceEntryRef == "" {
		return Row{}, fmt.Errorf("attachment put failed at attachments.Store.Put while recording session %q ref %q: the row has no session or source ref, so it cannot be keyed; supply both before storing an image", pending.Row.SessionID, pending.Row.SourceEntryRef)
	}
	if pending.Row.OmittedReason == "" && len(pending.Bytes) == 0 {
		return Row{}, fmt.Errorf("attachment put failed at attachments.Store.Put while recording session %q ref %q: an accepted image has no bytes, so there is nothing to address; set an omission reason or pass the decoded bytes", pending.Row.SessionID, pending.Row.SourceEntryRef)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := pending.Row
	if row.OmittedReason == "" {
		sum := sha256.Sum256(pending.Bytes)
		row.SHA256 = hex.EncodeToString(sum[:])
		row.ByteSize = len(pending.Bytes)
		if err := s.writeBlobLocked(row.SHA256, pending.Bytes); err != nil {
			return Row{}, err
		}
	} else {
		row.SHA256 = ""
	}
	if err := s.rows.UpsertAttachment(ctx, row); err != nil {
		return Row{}, fmt.Errorf("attachment put failed at attachments.Store.Put while inserting the row for session %q ref %q: %w; the file, if any, was written but the row was not, so a later GC treats an unreferenced file as garbage; retry the put", row.SessionID, row.SourceEntryRef, err)
	}
	return row, nil
}

func (s *Store) writeBlobLocked(sha string, payload []byte) error {
	path, err := s.ObjectPath(sha)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while creating %s: %w; the image was not stored; check the data directory is writable and retry", s.dir, err)
	}
	temp, err := os.CreateTemp(s.dir, ".attachment-*")
	if err != nil {
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while creating a temp file in %s: %w; the image was not stored; check the data directory is writable and retry", s.dir, err)
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while writing %s: %w; the image was not stored; retry the put", tempName, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while syncing %s: %w; the image was not stored; retry the put", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while closing %s: %w; the image was not stored; retry the put", tempName, err)
	}
	if s.hooks.PutAfterFsync != nil {
		s.hooks.PutAfterFsync()
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("attachment put failed at attachments.Store.writeBlobLocked while renaming %s to %s: %w; the image was not stored; retry the put", tempName, path, err)
	}
	cleanup = false
	return nil
}

// Open reads one stored image by its content address. A row without a file is
// reported as missing. A content address with no row is not found.
func (s *Store) Open(ctx context.Context, sha string) (Row, []byte, error) {
	if !validSHA(sha) {
		return Row{}, nil, fmt.Errorf("attachment open failed at attachments.Store.Open while reading %q: the value is not a lowercase 64-hex content address, so no file was opened; pass the sha256 hex digest", sha)
	}
	rows, err := s.rows.AttachmentsBySHA(ctx, sha)
	if err != nil {
		return Row{}, nil, fmt.Errorf("attachment open failed at attachments.Store.Open while reading the row for %s: %w; no file was served; retry the read", sha, err)
	}
	if len(rows) == 0 {
		return Row{}, nil, os.ErrNotExist
	}
	path, err := s.ObjectPath(sha)
	if err != nil {
		return Row{}, nil, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		row := rows[0]
		row.OmittedReason = string(schema.OmittedReasonMissing)
		row.ByteSize = 0
		return row, nil, fmt.Errorf("attachment open failed at attachments.Store.Open while reading %s: the row exists but the file does not (%w); the image is missing and was not served; re-index the session to restore the file or accept the missing placeholder", path, err)
	}
	return rows[0], payload, nil
}

// Integrity reports the read outcome for one source ref. A missing file is
// returned as a row whose omission reason is missing, not as a hard error,
// because the caller still has the metadata.
func (s *Store) Integrity(ctx context.Context, sessionID, sourceRef string) (Row, error) {
	row, ok, err := s.rows.AttachmentBySource(ctx, sessionID, sourceRef)
	if err != nil {
		return Row{}, fmt.Errorf("attachment integrity read failed at attachments.Store.Integrity for session %q ref %q: %w; the row was not read; retry", sessionID, sourceRef, err)
	}
	if !ok {
		return Row{}, fmt.Errorf("attachment integrity read failed at attachments.Store.Integrity for session %q ref %q: no row exists, so the image was never stored; re-index the session", sessionID, sourceRef)
	}
	if row.OmittedReason != "" || row.SHA256 == "" {
		return row, nil
	}
	path, err := s.ObjectPath(row.SHA256)
	if err != nil {
		return Row{}, err
	}
	if _, err := os.Stat(path); err != nil {
		row.OmittedReason = string(schema.OmittedReasonMissing)
		row.ByteSize = 0
		return row, nil
	}
	return row, nil
}

// GCResult is what one garbage collection did.
type GCResult struct {
	Removed []string
	Kept    []string
}

// GC removes files no row references. It signals the before-lock hook before
// acquiring the lock shared with Put, so a test can observe that GC is pending
// while a writer still holds the lock.
func (s *Store) GC(ctx context.Context) (GCResult, error) {
	if s.hooks.GCBeforeLock != nil {
		s.hooks.GCBeforeLock()
	}
	s.mu.Lock()
	if s.hooks.GCAfterLock != nil {
		s.hooks.GCAfterLock()
	}
	defer s.mu.Unlock()
	referenced, err := s.rows.ReferencedSHAs(ctx)
	if err != nil {
		return GCResult{}, fmt.Errorf("attachment GC failed at attachments.Store.GC while listing referenced digests: %w; no file was removed; retry GC", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return GCResult{}, nil
		}
		return GCResult{}, fmt.Errorf("attachment GC failed at attachments.Store.GC while reading %s: %w; no file was removed; retry GC", s.dir, err)
	}
	var result GCResult
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !validSHA(name) {
			continue
		}
		if _, ok := referenced[name]; ok {
			result.Kept = append(result.Kept, name)
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("attachment GC failed at attachments.Store.GC while removing unreferenced %s: %w; referenced files were kept; retry GC", name, err)
		}
		result.Removed = append(result.Removed, name)
	}
	return result, nil
}

// PruneSession deletes one session's rows and then garbage-collects files no
// remaining row references.
func (s *Store) PruneSession(ctx context.Context, sessionID string) (GCResult, error) {
	if err := s.rows.DeleteSessionAttachments(ctx, sessionID); err != nil {
		return GCResult{}, fmt.Errorf("attachment prune failed at attachments.Store.PruneSession for session %q: %w; rows were not deleted and files were not collected; retry the prune", sessionID, err)
	}
	return s.GC(ctx)
}

// Classify decides whether decoded image bytes are stored. Unsupported wins
// over too-large. Width and height are filled when the header decodes.
func Classify(mediaType string, payload []byte) (Row, []byte) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	row := Row{ByteSize: len(payload)}
	if mediaType != string(schema.ImageMediaTypePNG) && mediaType != string(schema.ImageMediaTypeJPEG) {
		row.OmittedReason = string(schema.OmittedReasonUnsupported)
		row.ByteSize = 0
		return row, nil
	}
	row.MediaType = mediaType
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(payload)); err == nil {
		row.Width = cfg.Width
		row.Height = cfg.Height
	}
	if len(payload) > MaxImageBytes {
		row.OmittedReason = string(schema.OmittedReasonTooLarge)
		row.ByteSize = len(payload)
		return row, nil
	}
	row.ByteSize = len(payload)
	return row, payload
}

func validSHA(sha string) bool {
	if len(sha) != 64 || sha != strings.ToLower(sha) {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}
