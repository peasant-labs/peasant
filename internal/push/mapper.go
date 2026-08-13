package push

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/title"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// MapOptions bundles all parameters for MapMetadata.
// Replaces positional parameters for clarity and extensibility.
type MapOptions struct {
	Meta    *ingest.UnifiedMetadata
	Metrics *schema.QualityMetrics
	Entries []schema.SessionEntry
	// Associations is the current durable observed-commit context loaded from
	// the store. The enclosing session identity supplies the session arm, so
	// every published item carries only ID plus observedCommitHash.
	Associations []schema.PublishedAssociation
	License      schema.License
	Fields       config.PushFieldVisibility
	// Redactor redacts the ASSEMBLED document before it is returned.
	//
	// It is here rather than at the call site because this function is where the
	// metadata part is assembled from four independent sources, and redacting the
	// finished document is the only form that covers a source nobody thought
	// about. Before this boundary, per-source redaction covered only Meta and
	// Entries, so a user-derived quality.titleGenerated could be assembled onto
	// the wire beside a separately redacted entries[].contentPreview. Any field
	// added to PublishRequest tomorrow is covered by default instead of by whoever
	// remembers.
	//
	// A nil redactor returns the document as assembled - the same explicit-choice
	// convention marshalTranscriptContent and RedactEntries use.
	//
	// BECAUSE THIS REDACTS THE ASSEMBLED DOCUMENT, a field's SOURCE cannot
	// determine whether it is covered. GetQualityMetrics loads persisted quality
	// metrics; MapMetadata sanitizes quality.titleGenerated with the canonical
	// title pipeline before the runtime document redactor covers it with every
	// other assembled field. Coverage follows WHERE a field is assembled rather
	// than which store query supplied it.
	//
	// The one way to defeat that is to route a value onto the wire by some path
	// other than this function. That is a real possibility rather than a
	// rhetorical one - the transcript part is exactly such a path, and it has its
	// own whole-document redaction for the same reason.
	Redactor      redact.JSONRedactor
	TitlePipeline title.Pipeline
}

// MapMetadata converts a local UnifiedMetadata into the village schema.PublishRequest JSON,
// REDACTED as a whole document at the level the push applies.
//
// All json.Marshal errors are propagated — none are discarded.
// Field visibility is respected: fields with their corresponding Fields flag set to false
// are omitted (zero-valued) in the PublishRequest — they never reach the network.
//
// The redaction is deliberately over the ASSEMBLED document rather than over each
// source, and it is what makes the metadata part symmetric with the transcript
// part, which has always been redacted whole. See MapOptions.Redactor for why the
// per-source form kept leaving new fields uncovered.
//
// It does NOT make RedactEntries redundant. That runs where the entries are READ,
// so every consumer of them - including one added later that does not redact its
// own output - receives redacted entries. This runs where one particular document
// is assembled. The two cover different populations: remove either and a real
// path goes unredacted.
func MapMetadata(opts MapOptions) ([]byte, error) {
	meta := opts.Meta
	req := schema.PublishRequest{
		Identity: schema.SessionIdentity{
			SessionID:     schema.SessionID(meta.SessionID),
			SchemaVersion: meta.SchemaVersion,
		},
		Model: schema.ModelInfo{
			Harness:        schema.Harness(meta.ModelHarness),
			Model:          schema.ModelID(meta.Model),
			HarnessVersion: meta.Version,
			HostSlug:       conditionalHostSlug(opts.Fields.HostSlug, schema.HostSlug(meta.HostSlug)),
		},
		Timestamp: schema.TimestampInfo{
			Start:    meta.Timestamp.Start,
			End:      meta.Timestamp.End,
			Ingested: meta.Timestamp.Ingested,
		},
		Source: schema.SourceInfo{
			FilePath: meta.Source.FilePath,
			Format:   schema.SourceFormat(meta.Source.Format),
		},
		Git: schema.GitContext{
			Branch:       conditionalStringPtr(opts.Fields.GitBranch, meta.Git.Branch),
			Remote:       conditionalStringPtr(opts.Fields.GitRemote, meta.Git.Remote),
			Worktree:     meta.Git.Worktree,
			Tracking:     meta.Git.Tracking,
			Associations: append([]schema.PublishedAssociation(nil), opts.Associations...),
		},
		Project: schema.ProjectContext{
			// project.hash is ALWAYS sent. It is a per-installation salted
			// HMAC-SHA256 (salt.go) — non-correlatable across users — so the
			// village can group a single user's sessions by project without
			// being able to recover the underlying remote. Unlike the raw
			// identity fields (gitRemote/branch/projectPath/projectName/hostSlug,
			// which stay field-visibility-gated), the hash leaks no plaintext.
			Hash:     schema.ProjectHash(meta.Project.Hash),
			FilePath: conditionalString(opts.Fields.ProjectPath, meta.Project.FilePath),
			Name:     conditionalString(opts.Fields.ProjectName, meta.Project.Name),
		},
		Stats: schema.SessionStats{
			TurnCount:     meta.Stats.TurnCount,
			ToolCallCount: meta.Stats.ToolCallCount,
			SubagentCount: meta.Stats.SubagentCount,
			DurationMs:    meta.Stats.DurationMs,
			TokensIn:      meta.Stats.TokensIn,
			TokensOut:     meta.Stats.TokensOut,
		},
		Diagnostics: schema.DiagnosticsInfo{
			Warnings: make([]schema.DiagnosticEntry, len(meta.Diagnostics.Warnings)),
			Partial:  meta.Diagnostics.Partial,
		},
	}

	for i, w := range meta.Diagnostics.Warnings {
		req.Diagnostics.Warnings[i] = schema.DiagnosticEntry{
			ErrorType:   w.ErrorType,
			Location:    w.Location,
			Message:     w.Message,
			Remediation: w.Remediation,
		}
	}

	// ParentUUID — only set when the session has a parent
	if meta.ParentUUID != nil {
		parentID := schema.SessionID(*meta.ParentUUID)
		req.Identity.ParentSessionID = &parentID
	}

	// Subagents
	if len(meta.Subagents) > 0 {
		req.Subagents = make([]schema.SubagentRef, len(meta.Subagents))
		for i, s := range meta.Subagents {
			req.Subagents[i] = schema.SubagentRef{
				SessionID:  schema.SessionID(s.SessionID),
				ParentUUID: schema.SessionID(s.ParentUUID),
			}
		}
	}

	// v2/v3 metrics — included when the caller provides pre-mapped QualityMetrics.
	if opts.Metrics != nil {
		quality := *opts.Metrics
		if quality.TitleGenerated != nil {
			pipeline := opts.TitlePipeline
			if pipeline == nil {
				var err error
				pipeline, err = title.Default()
				if err != nil {
					return nil, fmt.Errorf("initialize canonical title sanitation while mapping publish metadata: %w; no request was created; retry after repairing the Redact installation", err)
				}
			}
			projectPath := meta.Project.FilePath
			if meta.Git.Worktree != nil && *meta.Git.Worktree != "" {
				projectPath = *meta.Git.Worktree
			}
			result, err := pipeline.Sanitize(*quality.TitleGenerated, redact.TitleContext{
				Harness: schema.Harness(meta.ModelHarness), ProjectPath: projectPath,
			})
			if err != nil {
				return nil, fmt.Errorf("sanitize generated title while mapping publish metadata: %w; no request was created and the unchecked title was not used; recompute metrics and retry", err)
			}
			quality.TitleGenerated = nil
			if result.Text != "" {
				quality.TitleGenerated = &result.Text
			}
		}
		req.Quality = &quality
	}

	// Content-layer entries: include when the caller provides mapped entries.
	if len(opts.Entries) > 0 {
		req.Entries = opts.Entries
	}

	// License: the contributor's per-transcript content license (sessions.license_id,
	// V37). Always sent when set — NOT field-visibility-gated (it is a deliberate legal
	// declaration, not an identity leak). Empty ⇒ omitempty drops it ⇒ village stores NULL.
	req.License = opts.License

	result, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal publish request: %w", err)
	}
	if opts.Redactor == nil {
		return result, nil
	}
	redacted, err := redactJSONDocument(opts.Redactor, result, "publish request")
	if err != nil {
		return nil, err
	}
	var redactedRequest schema.PublishRequest
	if err := json.Unmarshal(redacted, &redactedRequest); err != nil {
		return nil, fmt.Errorf("restore schema-owned evidence after publish-request redaction: decode failed: %w; no request was created; fix the redaction rule and retry", err)
	}
	if len(redactedRequest.Entries) != len(req.Entries) {
		return nil, fmt.Errorf("restore schema-owned evidence after publish-request redaction: entry count changed from %d to %d; no request was created because model evidence cannot be matched safely; fix the redaction rule and retry", len(req.Entries), len(redactedRequest.Entries))
	}
	for index := range req.Entries {
		modelID, present, evidenceErr := observedModelFromExtra(req.Entries[index].Extra)
		if evidenceErr != nil {
			return nil, evidenceErr
		}
		if present {
			restored, restoreErr := restoreObservedModelExtra(redactedRequest.Entries[index].Extra, modelID)
			if restoreErr != nil {
				return nil, restoreErr
			}
			redactedRequest.Entries[index].Extra = restored
		}
	}
	return json.Marshal(redactedRequest)
}

// conditionalStringPtr returns nil if include is false or val is nil.
func conditionalStringPtr(include bool, val *string) *string {
	if !include || val == nil {
		return nil
	}
	return val
}

// conditionalString returns empty string if include is false.
func conditionalString(include bool, val string) string {
	if !include {
		return ""
	}
	return val
}

// conditionalHostSlug returns empty HostSlug if include is false.
func conditionalHostSlug(include bool, val schema.HostSlug) schema.HostSlug {
	if !include {
		return ""
	}
	return val
}

func privacySafeProjectLabel(hash string) string {
	if hash == "" {
		return ""
	}
	const labelHashLength = 12
	if len(hash) > labelHashLength {
		hash = hash[:labelHashLength]
	}
	return "project-" + hash
}
