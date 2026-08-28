package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// BuildTranscriptContent builds the versioned, structured push wire body (D2):
// a TranscriptContent envelope wrapping the SessionDetailPayload produced by the
// SAME builder the local dashboard uses (internal/transcript). Raw session-level
// identity is privacy-normalized first, so the shape matches the local viewer
// without bypassing publication field consent.
//
// Content comes from indexed entries rather than raw provider files. Stored
// entries are not redacted during ingest, so the production pipeline must pass
// the output of RedactEntries here before publication. Any user-facing summary
// must describe this as best-effort detection of known patterns; see
// config.RedactionScopeSentence.
//
// emit is the negotiated push-contract version: normally
// defaults.PublishSchemaVersion, but the village's current version when the CLI
// downgrade-emits (CLI-ahead). The envelope's ContractVersion and the embedded
// SessionDetailPayload.SchemaVersion are both stamped from emit in lockstep
// (envelope wins on any future disagreement; see schema.TranscriptContent).
func BuildTranscriptContent(meta *ingest.UnifiedMetadata, entries []schema.SessionEntry, emit schema.PushContractVersion, fields config.PushFieldVisibility, origin sessionorigin.Origin) schema.TranscriptContent {
	content, _ := BuildTranscriptContentValidated(meta, entries, emit, fields, origin)
	return content
}

// BuildTranscriptContentValidated builds content through the producer trust
// boundary and returns attribution failures to outward-facing callers.
//
// origin is the session's stored origin, a push CALL OPTION read from
// sessions.session_origin rather than from the metadata document, which is an
// alias to the external contract module and is not Peasant's to extend.
func BuildTranscriptContentValidated(meta *ingest.UnifiedMetadata, entries []schema.SessionEntry, emit schema.PushContractVersion, fields config.PushFieldVisibility, origin sessionorigin.Origin) (schema.TranscriptContent, error) {
	session := metadataToSession(meta, fields)
	turns, err := transcript.EntriesToTurnsValidated(entries)
	if err != nil {
		return schema.TranscriptContent{}, err
	}
	session.Turns = turns

	payload, err := transcript.SessionToDetailValidated(session)
	if err != nil {
		return schema.TranscriptContent{}, err
	}
	payload.TurnCount = len(payload.Turns)
	payload.SchemaVersion = emit
	// Peasant ALWAYS declares, for all three stored values, and never refuses a
	// session on account of its origin. The stored menu and the wire menu are
	// the same three tokens, so there is no mapping step and no case where the
	// field is deliberately dropped; a published document carrying no origin can
	// only come from a build older than this one.
	//
	// It is NOT gated by push field visibility: origin names no person, path,
	// host, or repository, so gating it would defeat grouping at the server for
	// no privacy gain. It carries no per-field redaction handling either — the
	// assembled document is redacted whole (marshalBuiltTranscriptContent), which
	// is what covers a field nobody remembered to add a rule for.
	payload.SessionOrigin = declaredOrigin(origin)

	return schema.TranscriptContent{
		ContractVersion: emit,
		Kind:            schema.ContentKindSessionDetail,
		SessionDetail:   payload,
	}, nil
}

// RedactEntries returns the stored entries with every string value redacted at
// the level the push applies.
//
// A publish is multipart: both the transcript envelope and metadata entries can
// carry transcript text. Redacting entries once at the pipeline read boundary
// gives both parts the same bytes and ensures newly added string fields pass
// through the same RedactJSON rules as `peasant redact`.
//
// Applying it twice is a no-op AT THE OFFERED LEVELS, so feeding both consumers
// from one redaction is safe: the canonical placeholders match no rule at
// minimal or standard. It is NOT idempotent at maximum, where the identifier
// anonymiser renames what the first pass already renamed (id1 becomes id2), so
// the two parts of one publish would carry different names for the same symbol.
// Maximum is refused by a single static row in internal/config, so no push runs
// there today - but this function takes a redact.JSONRedactor at any level, and
// the qualification is the difference between a safe assumption and a lucky one.
//
// It fails closed: redact.RedactJSONDocBytes
// returns its input UNCHANGED when the round trip fails, so routing through it
// alone would hand back the unredacted originals with no error and no way for a
// caller to tell. redactJSONDocument repeats the round trip keeping the errors,
// and both outward seams use it.
//
// A nil redactor leaves the entries as recorded, the same explicit-choice
// convention marshalTranscriptContent uses; every production push builds one.
func RedactEntries(redactor redact.JSONRedactor, entries []schema.SessionEntry) ([]schema.SessionEntry, error) {
	if redactor == nil || len(entries) == 0 {
		return entries, nil
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf(
			"redact transcript entries for publication: marshalling %d stored entr(ies) failed: %w. "+
				"This ran in internal/push.RedactEntries, after the entries were read from the local store and before "+
				"anything was uploaded, so nothing was published and nothing local was changed. Publishing the entries "+
				"unredacted is not an option here: they carry the recorded transcript text, and the village does not "+
				"scan the metadata part for secrets. Re-run the push; if it recurs, run 'peasant harvest --json' to "+
				"check whether this session's indexed entries are readable",
			len(entries), err)
	}
	redactedRaw, err := redactJSONDocument(redactor, raw, "transcript entries")
	if err != nil {
		return nil, err
	}
	var redactedEntries []schema.SessionEntry
	if err := json.Unmarshal(redactedRaw, &redactedEntries); err != nil {
		return nil, fmt.Errorf(
			"redact transcript entries for publication: the redacted entries could not be read back as entries: %w. "+
				"This ran in internal/push.RedactEntries, between redaction and upload, so nothing was published. It "+
				"means the redactor returned something that is no longer a valid entry list, which would otherwise be "+
				"published as the transcript's content layer. Report this with the session id from the line above; the "+
				"push can be retried, but it will fail the same way until the rule that reshaped the document is fixed",
			err)
	}
	if len(redactedEntries) != len(entries) {
		return nil, fmt.Errorf("redact transcript entries for publication: entry count changed from %d to %d during redaction; schema-owned evidence cannot be matched safely, so nothing was uploaded; fix the custom redaction rule so it rewrites values without reshaping the entry list, then retry", len(entries), len(redactedEntries))
	}
	for index := range entries {
		modelID, present, err := observedModelFromExtra(entries[index].Extra)
		if err != nil {
			return nil, err
		}
		if present {
			restored, err := restoreObservedModelExtra(redactedEntries[index].Extra, modelID)
			if err != nil {
				return nil, err
			}
			redactedEntries[index].Extra = restored
		}
	}
	return redactedEntries, nil
}

func observedModelFromExtra(extra *string) (string, bool, error) {
	if extra == nil {
		return "", false, nil
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*extra), &document); err != nil {
		return "", false, nil
	}
	raw, present := document["model_id"]
	if !present {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fmt.Errorf("preserve observed-model evidence during publication redaction: model_id is not a string: %w; nothing was uploaded; repair or re-index the entry, then retry", err)
	}
	return value, true, nil
}

func restoreObservedModelExtra(extra *string, value string) (*string, error) {
	if extra == nil {
		return nil, fmt.Errorf("preserve observed-model evidence during publication redaction: redaction removed the entry Extra document; nothing was uploaded; fix the custom rule so it does not reshape entries, then retry")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*extra), &document); err != nil {
		return nil, fmt.Errorf("preserve observed-model evidence during publication redaction: redacted Extra is unreadable: %w; nothing was uploaded; fix the custom rule, then retry", err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	document["model_id"] = raw
	restored, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	result := string(restored)
	return &result, nil
}

// redactJSONDocument redacts a marshalled document and PROVES it did.
//
// redact.RedactJSONDocBytes fails OPEN: if the decode or the re-marshal fails it
// returns its INPUT UNCHANGED, with no error. For a viewer that is survivable.
// On this path it is not - a silent pass-through publishes the document exactly
// as assembled, which is the recorded transcript text - and it is invisible,
// because the caller receives well-formed bytes either way. So the round trip is
// repeated here with the errors kept.
//
// ALL THREE outward seams go through this: the entries at the point they are
// read, the assembled metadata document, and the transcript content part. One
// implementation, so a fail-open cannot be closed on one path and left open on
// another - which is what had happened, with this comment claiming two seams
// while the third and largest one called the fail-open primitive directly.
func redactJSONDocument(redactor redact.JSONRedactor, document []byte, what string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf(
			"redact the %s for publication: it could not be decoded for redaction: %w. This ran in "+
				"internal/push.redactJSONDocument, after the document was assembled and before anything was uploaded, so "+
				"nothing was published and nothing local changed. Publishing it unredacted is not an option: it carries "+
				"the session's own recorded text, and the village does not scan this part for secrets. Retry the push; if "+
				"it recurs the document is malformed and the session should be re-ingested",
			what, err)
	}
	encoded, err := json.Marshal(redactor.RedactJSON(decoded))
	if err != nil {
		return nil, fmt.Errorf(
			"redact the %s for publication: the redacted document could not be re-encoded: %w. This ran in "+
				"internal/push.redactJSONDocument, between redaction and upload, so nothing was published. It means a "+
				"redaction rule produced a value that is not representable on the wire, and the UNREDACTED document "+
				"would otherwise have been sent in its place. Report this with the session id printed above; retrying "+
				"will fail the same way until the rule is corrected",
			what, err)
	}
	return encoded, nil
}

// marshalTranscriptContent builds and serializes the structured push body at the
// negotiated emit contract version, REDACTED at the level the push applies.
//
// The redaction runs through the shared fail-closed redactJSONDocument helper.
// It invokes the same RedactJSON rules as `peasant redact` while retaining
// decode and encode failures instead of returning the original bytes. The two
// entrypoints therefore share rule behavior without copying the unsafe
// fail-open byte wrapper.
//
// It redacts every string value in the document, which is what makes it the same
// behaviour rather than a similar one. IDENTIFIERS SURVIVE because no rule
// matches their shapes, not because they are excluded - a session UUID, a project
// hash and a contract version have nothing a secret or PII rule recognises - and
// TestPushContent_RedactsContentWithoutBreakingIdentifiers pins that, since a
// rule added later that did match one of them would corrupt the key the village
// stores against.
//
// A nil redactor leaves the body unredacted. That is the caller's decision to
// make explicit rather than this function's to guess at; every production push
// path constructs one.
func marshalTranscriptContent(
	meta *ingest.UnifiedMetadata,
	entries []schema.SessionEntry,
	emit schema.PushContractVersion,
	fields config.PushFieldVisibility,
	origin sessionorigin.Origin,
	redactor redact.JSONRedactor,
) ([]byte, error) {
	content, err := BuildTranscriptContentValidated(meta, entries, emit, fields, origin)
	if err != nil {
		return nil, err
	}
	return marshalBuiltTranscriptContent(content, redactor)
}

func marshalBuiltTranscriptContent(content schema.TranscriptContent, redactor redact.JSONRedactor) ([]byte, error) {
	b, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal transcript content: %w", err)
	}
	if redactor == nil {
		return b, nil
	}
	// Through the SAME fail-closed round trip as the other two seams. This one
	// called redact.RedactJSONDocBytes directly and returned its value, so a
	// re-marshal failure published the assembled document exactly as built, with
	// no error - the failure the shared helper exists to convert into a refusal,
	// on the seam that carries the published transcript. Two comments here
	// asserted there were two outward seams and that one implementation prevented
	// exactly this; there are three, and this was the one outside it.
	redacted, err := redactJSONDocument(redactor, b, "transcript content")
	if err != nil {
		return nil, err
	}
	// And READ BACK, so a redaction that returns a DIFFERENT SHAPE is a refusal
	// rather than a corrupt publish.
	//
	// The other two seams get this incidentally: the entries seam unmarshals into
	// []SessionEntry, and the assembled request is schema-validated after
	// redaction. This one had neither, so a redactor returning valid JSON that is
	// not a transcript produced a published body that is not a transcript, with no
	// error - found by driving the fail-closed test per seam instead of once.
	// Nothing leaks in that case, which is why it is a shape check and not a
	// second redaction; a body the village stores as a transcript should still be
	// one.
	var check schema.TranscriptContent
	if err := json.Unmarshal(redacted, &check); err != nil || check.Kind != content.Kind {
		return nil, fmt.Errorf(
			"redact the transcript content for publication: the redacted document is no longer a transcript envelope "+
				"(kind %q, unmarshal error %v). This ran in internal/push.marshalTranscriptContent, between redaction and "+
				"upload, so nothing was published. It means a redaction rule reshaped the document rather than rewriting "+
				"values inside it, and the village would otherwise have stored that as this session's transcript. Report "+
				"this with the session id printed above; retrying will fail the same way until the rule is corrected",
			check.Kind, err)
	}
	if (content.SessionDetail == nil) != (check.SessionDetail == nil) {
		return nil, transcriptShapeRedactionError("sessionDetail presence changed")
	}
	if content.SessionDetail != nil {
		if err := restoreObservedModels(content.SessionDetail.Turns, check.SessionDetail.Turns); err != nil {
			return nil, err
		}
	}
	return json.Marshal(check)
}

func restoreObservedModels(source, destination []schema.TurnDetail) error {
	if len(source) != len(destination) {
		return transcriptShapeRedactionError(fmt.Sprintf("turn count changed from %d to %d", len(source), len(destination)))
	}
	for index := range source {
		if source[index].Index != destination[index].Index || source[index].Role != destination[index].Role || source[index].Depth != destination[index].Depth {
			return transcriptShapeRedactionError(fmt.Sprintf("turn identity changed at position %d", index))
		}
		destination[index].ObservedModel = source[index].ObservedModel
	}
	return nil
}

func transcriptShapeRedactionError(reason string) error {
	return fmt.Errorf("transcript redaction changed evidence-bearing structure\n  what: %s\n  why: observedModel evidence can only be restored onto the exact validated turn sequence\n  where: push.marshalBuiltTranscriptContent\n  when: after local content validation and redaction, before serialization or upload\n  meaning: nothing was uploaded because the redacted payload could diverge from the capability-gated payload\n  fix: correct the custom redaction rule so it rewrites string values without removing, reordering, or truncating transcript structure, then retry", reason)
}

// metadataToSession projects on-disk metadata onto the SessionToDetail input
// while applying the same raw-identity consent gates as the authoritative part.
func metadataToSession(meta *ingest.UnifiedMetadata, fields config.PushFieldVisibility) *ingest.Session {
	resolved := fields.Resolve()
	sid, _ := ingest.NewSessionID(string(meta.SessionID))
	// The displayed project identity follows the SAME label-vs-hash decision
	// as the authoritative publish request (projectWireLabel in mapper.go):
	// a recognizable git remote with GitRemote and ProjectName both visible
	// sends the repository label; otherwise this falls back to the
	// privacy-safe salted-hash label. The raw project name/basename is never
	// sent on this path either — D10 applies uniformly across both published
	// documents, not only the authoritative one.
	project := privacySafeProjectLabel(string(meta.Project.Hash))
	label, sentLabel := projectWireLabel(meta, resolved)
	if sentLabel {
		project = label
	}
	session := &ingest.Session{
		ID:        sid,
		Harness:   meta.ModelHarness,
		Model:     string(meta.Model),
		Project:   project,
		StartTime: time.UnixMilli(meta.Timestamp.Start),
		EndTime:   time.UnixMilli(meta.Timestamp.End),
	}
	// The local path is withheld whenever the label went out instead (D10):
	// a label already identifies the project, so pairing it with the
	// filesystem path would leak directory structure for no benefit.
	if !sentLabel && resolved.ProjectPath {
		session.ProjectPath = meta.CWD
		if meta.CWD == "" {
			session.ProjectPath = meta.Project.FilePath
		}
	}
	session.Metadata.TokensIn = meta.Stats.TokensIn
	session.Metadata.TokensOut = meta.Stats.TokensOut
	session.Metadata.TotalTokens = meta.Stats.TokensIn + meta.Stats.TokensOut
	session.Metadata.TurnCount = meta.Stats.TurnCount
	session.Metadata.ToolCallCount = meta.Stats.ToolCallCount
	session.Metadata.Duration = time.Duration(meta.Stats.DurationMs) * time.Millisecond
	if resolved.GitBranch && meta.Git.Branch != nil {
		session.GitBranch = *meta.Git.Branch
	}
	if resolved.GitRemote && meta.Git.Remote != nil {
		session.GitRemote = *meta.Git.Remote
	}
	return session
}

// declaredOrigin converts a stored origin into the wire declaration.
//
// A push NEVER fails on account of origin, so a value the stored menu cannot
// produce — which the sessions.session_origin CHECK makes unreachable from any
// row this build wrote — is declared as unknown rather than refused. Under the
// contract that is the honest answer for a value this producer cannot vouch
// for: it returns the decision to the consumer's own rule instead of asserting
// something false, and the session is still published.
func declaredOrigin(origin sessionorigin.Origin) schema.SessionOrigin {
	if !origin.Valid() {
		return schema.SessionOriginUnknown
	}
	return schema.SessionOrigin(origin)
}
