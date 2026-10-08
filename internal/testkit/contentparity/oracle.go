// Package contentparity drives deterministic captures through production readers and writers.
// Expected output is never computed here: it is captured separately from a pinned older build.
package contentparity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// Case declares the entire source scenario, including deliberately unusual
// producer projections that ordinary parsers do not currently emit.
type Case struct {
	Name        string            `yaml:"name"`
	Harness     string            `yaml:"harness"`
	SessionID   string            `yaml:"session_id"`
	Source      string            `yaml:"source"`
	SourceKind  string            `yaml:"source_kind"`
	Entries     []string          `yaml:"entries"`
	Earlier     []string          `yaml:"earlier"`
	NonEmitted  map[string]string `yaml:"non_emitted"`
	PreviewOnly bool              `yaml:"preview_only"`
	FileBacked  bool              `yaml:"file_backed"`
	Why         string            `yaml:"why"`
	TextPrefix  string            `yaml:"text_prefix"`
	TextRepeats int               `yaml:"text_repeats"`
	TextSuffix  string            `yaml:"text_suffix"`
	Omission    bool              `yaml:"omission"`
}

type Family struct {
	Cases []Case `yaml:"cases"`
}

func Load(data, manifest []byte) ([]Case, error) {
	var family Family
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&family); err != nil {
		return nil, err
	}
	var required struct {
		Names []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(manifest, &required); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, c := range family.Cases {
		invalidName := c.Name == "" || seen[c.Name]
		missingContext := c.SessionID == "" || c.Why == ""
		missingSource := len(c.Entries) == 0 && c.Source == ""
		if invalidName || missingContext || missingSource {
			return nil, fmt.Errorf("load parity sources: case %q is duplicate or incomplete; restore its pinned source and explanation", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range required.Names {
		if !seen[name] {
			return nil, fmt.Errorf("load parity sources: required case %q missing; restore the scenario", name)
		}
		delete(seen, name)
	}
	if len(seen) != 0 {
		return nil, fmt.Errorf("load parity sources: unmanifested cases %v; update the required-name manifest deliberately", seen)
	}
	return family.Cases, nil
}

// Outcome preserves serialization bytes, or the exact refusal and its stable
// errors.Is identity. Absence is represented by JSON null, never an empty hash.
type Outcome struct {
	Bytes []byte   `json:"bytes,omitempty"`
	Error *Refusal `json:"error,omitempty"`
}
type Refusal struct {
	Identity string `json:"identity"`
	Message  string `json:"message"`
}
type Corpus struct {
	Detail            Outcome `json:"detail"`
	Export            Outcome `json:"export"`
	Publish           Outcome `json:"publish"`
	PublishEntries    Outcome `json:"publishEntries"`
	Preview           Outcome `json:"preview"`
	ListEntries       Outcome `json:"listEntries"`
	Range             Outcome `json:"listEntriesRange"`
	MaxIndex          Outcome `json:"maxEntryIndex"`
	FirstEntry        Outcome `json:"firstEntry"`
	FirstUser         Outcome `json:"firstUserMessage"`
	FirstUsers        Outcome `json:"firstUserMessageBulk"`
	LeadingUsers      Outcome `json:"leadingUserMessagesBulk"`
	MetricHash        Outcome `json:"metricInputHash"`
	MetricInput       Outcome `json:"metricInput"`
	GenerationEntries Outcome `json:"generationEntries"`
	SessionHash       *string `json:"sessionEntriesHash"`
	FullHash          *string `json:"fullCaptureHash"`
}

// LoadCorpus refuses an incomplete or unknown surface inventory, including a
// missing hash key that would otherwise decode indistinguishably from NULL.
func LoadCorpus(raw []byte) (Corpus, error) {
	var corpus Corpus
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return corpus, err
	}
	typ := reflect.TypeOf(corpus)
	for i := 0; i < typ.NumField(); i++ {
		key := surfaceName(typ.Field(i))
		if _, ok := fields[key]; !ok {
			return corpus, fmt.Errorf("frozen corpus missing surface %s; restore the pre-change capture", key)
		}
		delete(fields, key)
	}
	if len(fields) != 0 {
		return corpus, fmt.Errorf("frozen corpus has unknown surfaces %v; review its source before comparison", fields)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		return corpus, err
	}
	value := reflect.ValueOf(corpus)
	for i := 0; i < value.NumField(); i++ {
		if surface, ok := value.Field(i).Interface().(Outcome); ok {
			if (surface.Bytes == nil) == (surface.Error == nil) {
				return corpus, fmt.Errorf("surface %s must contain exactly bytes or an explicit refusal", surfaceName(typ.Field(i)))
			}
		}
	}
	return corpus, nil
}

// Compare checks every declared surface, including explicit refusals and SQL
// NULLs. It does not parse payload JSON or canonicalize its bytes.
func Compare(want, got Corpus) error {
	w, g := reflect.ValueOf(want), reflect.ValueOf(got)
	var differences []error
	for i := 0; i < w.NumField(); i++ {
		if !reflect.DeepEqual(w.Field(i).Interface(), g.Field(i).Interface()) {
			old, new := w.Field(i).Interface(), g.Field(i).Interface()
			if a, ok := old.(*string); ok {
				if a != nil {
					old = *a
				}
				if b, ok := new.(*string); ok && b != nil {
					new = *b
				}
			}
			if a, ok := old.(Outcome); ok {
				if b, ok := new.(Outcome); ok && a.Error == nil && b.Error == nil {
					old, new = string(a.Bytes), string(b.Bytes)
				}
			}
			differences = append(differences, fmt.Errorf("frozen content parity: surface %s differs from the pre-change capture: want %.350v; got %.350v; inspect the producer/reader drift before changing the golden", surfaceName(w.Type().Field(i)), old, new))
		}
	}
	return errors.Join(differences...)
}

func surfaceName(field reflect.StructField) string {
	// This is the Go encoding tag namespace, not a transcript source format.
	// ast-grep-ignore: no-bare-format-literals
	return field.Tag.Get("json")
}

func outcome(value any, err error) Outcome {
	if err != nil {
		identity := "error"
		switch {
		case errors.Is(err, transcript.ErrSnapshotIncomplete):
			identity = "transcript.ErrSnapshotIncomplete"
		case errors.Is(err, transcript.ErrLegacySnapshot):
			identity = "transcript.ErrLegacySnapshot"
		case errors.Is(err, store.ErrContentCaptureIncomplete):
			identity = "store.ErrContentCaptureIncomplete"
		case errors.Is(err, push.ErrMetadataMissing):
			identity = "push.ErrMetadataMissing"
		}
		return Outcome{Error: &Refusal{identity, err.Error()}}
	}
	if raw, ok := value.([]byte); ok {
		return Outcome{Bytes: append([]byte{}, raw...)}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return Outcome{Bytes: raw}
}

// Run owns a fresh sandbox and removes it on every ordinary exit. It never
// resolves XDG paths, discovery roots, or a user's store.
func Run(ctx context.Context, c Case) (Corpus, error) {
	var result Corpus
	dir, err := os.MkdirTemp("", "peasant-content-parity-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		return result, err
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		return result, err
	}
	dbPath := filepath.Join(dir, "capture.db")
	db, err := store.Open(dbPath, store.WithPoolSize(2), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		return result, err
	}
	defer db.Close()
	sid, err := schema.NewSessionID(c.SessionID)
	if err != nil {
		return result, err
	}
	var harness schema.Harness
	err = harness.UnmarshalText([]byte(c.Harness))
	if err != nil {
		return result, err
	}
	entries, err := sourceEntries(ctx, c, sid)
	if err != nil {
		return result, err
	}
	if c.TextRepeats > 0 {
		text := strings.Repeat(c.TextPrefix, c.TextRepeats) + c.TextSuffix
		entries[0].ContentPreview = &text
	}
	if c.Omission {
		record, e := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, 1, 300<<20, 256<<20)
		if e != nil {
			return result, e
		}
		extra, e := record.Extra()
		if e != nil {
			return result, e
		}
		note := ingest.OmissionPlaceholderNote(record)
		entries[0].Extra = &extra
		entries[0].ContentPreview = &note
	}
	earlier, err := decodeEntries(c.Earlier, sid, harness)
	if err != nil {
		return result, err
	}
	metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: harness, Model: "fixture-model", Version: "fixture-version", CWD: "/synthetic/parity"}
	metadata.Timestamp.Start = 1700000000000
	metadata.Timestamp.End = 1700000005000
	ingested := int64(1700000006000)
	metadata.Timestamp.Ingested = &ingested
	metadata.Source.FilePath = "/synthetic/parity/source"
	metadata.Source.Format = schema.SourceFormatJSONL
	metadata.Project.Hash = schema.ProjectHash(strings.Repeat("a", 64))
	metadata.Project.Name = "fixture-project"
	metadata.Project.FilePath = "/synthetic/parity"
	metadata.HostSlug = "fixture-host"
	metadata.Stats.TurnCount = len(entries)
	count := int64(1)
	metadata.Stats.InputSubmissionCount = &count
	if c.PreviewOnly {
		metadata.Stats.InputSubmissionCount = nil
	}
	assessment, err := ingest.AssessCapture(ingest.V1CaptureFacts(harness, indexformat.V1{Entries: entries}, !c.PreviewOnly, false))
	if err != nil {
		return result, err
	}
	assessed, err := assessment.ContentCapture(ingest.ContentSourceProviderSource, ingest.TranscriptOriginFile, 1700000006000)
	if err != nil {
		return result, err
	}
	if assessed.Status == ingest.ContentCaptureIncomplete {
		partial := true
		metadata.Diagnostics.Partial = &partial
	}
	metadata.ContentHash = strings.Repeat("b", 64)
	metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
	revisions, err := db.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{{Metadata: &metadata, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact}})
	if err != nil {
		return result, fmt.Errorf("seed %s: %w", c.Name, err)
	}
	capture := ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceProviderSource, TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1700000006000}
	capture = assessed
	if c.PreviewOnly {
		capture.Status = ingest.ContentCaptureIncomplete
		capture.SourceAuthority = ingest.ContentSourceNone
		capture.CaptureFormat = ingest.ContentCaptureFormatPreviewOnly
	}
	if c.Harness == "pi" {
		writes := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: 1, IndexedAtMs: 1700000006000, RequireFullContent: !c.PreviewOnly, CaptureRevision: revisions[sid], ContentCapture: capture}})
		if len(writes) != 1 || writes[0].Err != nil {
			return result, fmt.Errorf("index %s: %v", c.Name, writes)
		}
	} else {
		generation := indexformat.Generation{ID: "g_" + strings.ReplaceAll(c.Name, "-", "_"), Metadata: metadata, Completeness: indexformat.GenerationCompletenessComplete, Main: indexformat.Partition{Entries: entries}, SourceEvidenceDigest: strings.Repeat("c", 64)}
		if c.PreviewOnly {
			generation.Completeness = indexformat.GenerationCompletenessIncompleteNew
		}
		if len(earlier) > 0 {
			generation.Earlier = []indexformat.EarlierPartition{{State: schema.EarlierHistoryUncertainMigrated, Content: indexformat.Partition{Entries: earlier}}}
		}
		blobs := map[schema.SourceEntryRef][]byte{}
		add := func(e schema.SessionEntry) {
			if e.SourceEntryRef == "" {
				return
			}
			var text *string
			switch e.EntryType {
			case "tool_use":
				text = e.ToolInput
			case "tool_result":
				text = e.ToolOutput
			default:
				text = e.ContentPreview
			}
			if text != nil {
				blobs[e.SourceEntryRef] = []byte(*text)
			} else {
				blobs[e.SourceEntryRef] = []byte{}
			}
		}
		for _, e := range entries {
			add(e)
		}
		for _, e := range earlier {
			add(e)
		}
		for ref, text := range c.NonEmitted {
			blobs[schema.SourceEntryRef(ref)] = []byte(text)
		}
		if len(c.NonEmitted) > 0 {
			retained := map[schema.SourceEntryRef][]byte{}
			for ref := range c.NonEmitted {
				retained[schema.SourceEntryRef(ref)] = nil
			}
			generation.Segments = []indexformat.ContextSegment{{Ordinal: 0, PhysicalSourceID: "fixture-retained", Coordinates: indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly}, Inclusion: indexformat.SegmentInclusionInherited, CapturedRefs: sortedRefs(retained)}}
		}
		// Sort references before constructing the catalog; map iteration never
		// determines captured ordering.
		refs := sortedRefs(blobs)
		for _, ref := range refs {
			b := blobs[ref]
			sum := sha256.Sum256(b)
			digest := hex.EncodeToString(sum[:])
			generation.Content = append(generation.Content, indexformat.ContentRecord{Ref: ref, RelativeBlob: "c_" + digest + ".blob", ByteLength: int64(len(b)), Digest: digest})
		}
		nativeAssessment, e := ingest.AssessCapture(ingest.CaptureFacts{Harness: harness, Result: indexformat.V2{Generation: generation}, Policy: ingest.CaptureFreshCandidate, Authoritative: !c.PreviewOnly, SourceOmitted: c.Omission})
		if e != nil {
			return result, e
		}
		capture, e = nativeAssessment.ContentCapture(ingest.ContentSourceProviderSource, ingest.TranscriptOriginFile, 1700000006000)
		if e != nil {
			return result, e
		}
		if c.FileBacked {
			err = seedFileBacked(ctx, db, dbPath, root, generation, blobs, capture, revisions[sid])
		} else {
			_, err = db.ActivateGeneration(ctx, store.GenerationActivation{Generation: indexformat.V2{Generation: generation}, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1700000006000, ContentCapture: capture, CaptureRevision: revisions[sid]})
		}
		if err != nil {
			return result, fmt.Errorf("activate %s: %w", c.Name, err)
		}
	}
	detailBytes, detail, err := transcript.BuildSnapshotDetailBytes(ctx, db, db, sid)
	result.Detail = outcome(detail, err)
	if err == nil {
		result.Detail.Bytes = detailBytes
	}
	if c.Harness == "pi" {
		provider := api.NewStoreDataProvider(db, sessionvisibility.All())
		session, e := provider.SessionByID(ctx, string(sid))
		if e == nil {
			detail, e = transcript.SessionToDetailValidated(session)
		}
		result.Detail = outcome(detail, e)
	}
	exported, err := export.ExportSession(ctx, db, &ingest.OSFileSystem{}, string(sid))
	result.Export = outcome(exported, err)
	input, publishDetail, err := push.LoadPublicationInput(ctx, db, string(sid))
	if err == nil {
		err = push.ValidatePublicationInput(input)
	}
	result.PublishEntries = outcome(nil, err)
	if err == nil {
		raw, mapErr := push.MapMetadata(push.MapOptions{Meta: &input.Metadata, Entries: input.Entries, Fields: config.PushFieldVisibility{}.Resolve()})
		if mapErr != nil {
			result.PublishEntries = outcome(nil, mapErr)
		} else {
			var request struct {
				Entries json.RawMessage `json:"entries"`
			}
			if e := json.Unmarshal(raw, &request); e != nil {
				return result, e
			}
			result.PublishEntries = Outcome{Bytes: request.Entries}
		}
	}
	if err == nil {
		content, e := push.BuildPublishTranscriptContent(publishDetail, &input.Metadata, input.Entries, schema.PushContractVersion("fixture-contract"), config.PushFieldVisibility{}, sessionorigin.User)
		result.Publish = outcome(content, e)
	} else {
		result.Publish = outcome(nil, err)
	}
	previewBytes, _, e := transcript.BuildSnapshotPreviewBytes(ctx, db, sid)
	result.Preview = outcome(previewBytes, e)
	if c.Harness == "pi" {
		result.Preview = result.Detail
	}
	listed, e := db.ListEntries(ctx, sid)
	result.ListEntries = outcome(listed, e)
	ranged, e := db.ListEntriesRange(ctx, sid, 0, 2)
	result.Range = outcome(ranged, e)
	max, e := db.MaxEntryIndex(ctx, sid)
	result.MaxIndex = outcome(max, e)
	first, e := db.FirstEntry(ctx, sid)
	result.FirstEntry = outcome(first, e)
	user, e := db.FirstUserMessage(ctx, string(sid))
	result.FirstUser = outcome(user, e)
	users, e := db.FirstUserMessageBulk(ctx, []string{string(sid)})
	result.FirstUsers = outcome(users, e)
	leading, e := db.LeadingUserMessagesBulk(ctx, []string{string(sid)}, 2)
	result.LeadingUsers = outcome(leading, e)
	metric, e := db.ReadMetricInput(ctx, sid, false)
	result.MetricInput = outcome(metric, e)
	if e == nil {
		hash, hErr := metric.Hash()
		result.MetricHash = outcome(hash, hErr)
	} else {
		result.MetricHash = outcome(nil, e)
	}
	conn, e := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	var captured []indexformat.Partition
	snapshotErr := db.WithSessionSnapshot(ctx, sid, func(snapshot indexformat.ReadSnapshot) error {
		captured = append(captured, snapshot.Main)
		for _, earlier := range snapshot.Earlier {
			captured = append(captured, earlier.Content)
		}
		return nil
	})
	result.GenerationEntries = outcome(captured, snapshotErr)
	if e != nil {
		return result, e
	}
	defer conn.Close()
	err = sqlitex.Execute(conn, `SELECT s.session_entries_hash,c.full_capture_sha256 FROM sessions s LEFT JOIN session_content_captures c USING(session_id) WHERE s.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(st *sqlite.Stmt) error {
		if st.ColumnType(0) != sqlite.TypeNull {
			v := st.ColumnText(0)
			result.SessionHash = &v
		}
		if st.ColumnType(1) != sqlite.TypeNull {
			v := st.ColumnText(1)
			result.FullHash = &v
		}
		return nil
	}})
	return result, err
}

func decodeEntries(raw []string, sid schema.SessionID, harness schema.Harness) ([]schema.SessionEntry, error) {
	var entries []schema.SessionEntry
	for _, data := range raw {
		var entry schema.SessionEntry
		if err := json.Unmarshal([]byte(data), &entry); err != nil {
			return nil, err
		}
		entry.SessionID = sid
		entry.Harness = harness
		entries = append(entries, entry)
	}
	return entries, nil
}

func sourceEntries(ctx context.Context, c Case, sid schema.SessionID) ([]schema.SessionEntry, error) {
	var harness schema.Harness
	err := harness.UnmarshalText([]byte(c.Harness))
	if err != nil {
		return nil, err
	}
	if c.Source == "" {
		return decodeEntries(c.Entries, sid, harness)
	}
	session := ingest.DiscoveredSession{SessionID: sid, Harness: harness, SourcePath: ingest.ResolvedPath("/synthetic/parity/source")}
	var entries []schema.SessionEntry
	switch c.SourceKind {
	case "codex-jsonl":
		entries, err = ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexFullContent(true)).IndexTranscriptBytes(ctx, session, []byte(c.Source))
	case "pi-jsonl":
		entries, err = ingest.NewPiIndexer(&ingest.OSFileSystem{}, ingest.WithPiFullContent(true)).IndexTranscriptBytes(ctx, session, []byte(c.Source))
	case "opencode-projection":
		session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeCurrentSQLite
		entries, err = ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true)).IndexTranscriptBytes(ctx, session, []byte(c.Source))
	default:
		return nil, fmt.Errorf("case %s: unsupported source kind %q", c.Name, c.SourceKind)
	}
	for i := range entries {
		if entries[i].SourceEntryRef == "" {
			entries[i].SourceEntryRef = schema.SourceEntryRef(fmt.Sprintf("e_%d", i))
		}
	}
	return entries, err
}

func sortedRefs(blobs map[schema.SourceEntryRef][]byte) []schema.SourceEntryRef {
	refs := make([]schema.SourceEntryRef, 0, len(blobs))
	for ref := range blobs {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i] < refs[j]
	})
	return refs
}

// seedFileBacked is deliberately limited to the dual-read case. It models
// old persisted rows, not a second implementation of the harmonized writer.
func seedFileBacked(ctx context.Context, db *store.Store, path, root string, g indexformat.Generation, blobs map[schema.SourceEntryRef][]byte, capture ingest.SessionContentCaptureWrite, revision int64) error {
	writes := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: g.Metadata.SessionID, Result: indexformat.V1{Entries: g.Main.Entries}, IndexVersion: 1, IndexerVersion: 1, IndexedAtMs: 1700000006000, RequireFullContent: true, ContentCapture: capture, CaptureRevision: revision}})
	if len(writes) != 1 || writes[0].Err != nil {
		return fmt.Errorf("seed file-backed mirror: %v", writes)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		return err
	}
	defer conn.Close()
	exec := func(query string, args ...any) error {
		return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args})
	}
	meta, err := json.Marshal(g.Metadata)
	if err != nil {
		return err
	}
	titles, err := json.Marshal(g.TitleRefs)
	if err != nil {
		return err
	}
	sid := string(g.Metadata.SessionID)
	if err = exec(`INSERT INTO session_projection_generations(session_id,generation_id,metadata_json,title_refs_json,input_submission_count,source_evidence_digest,completeness,index_format_version,installed_at_ms,activated_at_ms) VALUES(?,?,?,?,?,?,?,2,1700000006000,1700000006000)`, sid, g.ID, string(meta), string(titles), 1, g.SourceEvidenceDigest, string(g.Completeness)); err != nil {
		return err
	}
	for _, e := range g.Main.Entries {
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err = exec(`INSERT INTO session_projection_entries(session_id,generation_id,partition_id,entry_index,source_entry_ref,entry_json) VALUES(?,?,0,?,?,?)`, sid, g.ID, e.EntryIndex, string(e.SourceEntryRef), string(raw)); err != nil {
			return err
		}
	}
	genDir := filepath.Join(root, sid, "generations", g.ID)
	if err = os.MkdirAll(genDir, 0755); err != nil {
		return err
	}
	manifest, err := json.Marshal(g)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(genDir, "manifest.json"), manifest, 0600); err != nil {
		return err
	}
	for _, r := range g.Content {
		if err = exec(`INSERT INTO session_projection_content(session_id,generation_id,source_entry_ref,relative_blob,byte_length,integrity_digest) VALUES(?,?,?,?,?,?)`, sid, g.ID, string(r.Ref), r.RelativeBlob, r.ByteLength, r.Digest); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(genDir, r.RelativeBlob), blobs[r.Ref], 0600); err != nil {
			return err
		}
	}
	if err = exec(`INSERT INTO session_projection_sections(session_id,generation_id,partition_id,earlier_state) VALUES(?,?,0,NULL)`, sid, g.ID); err != nil {
		return err
	}
	return exec(`UPDATE sessions SET active_generation_id=?,index_version=1,index_format_version=2 WHERE session_id=?`, g.ID, sid)
}
