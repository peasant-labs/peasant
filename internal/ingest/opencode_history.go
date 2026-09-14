package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// OpenCodeHistoryRow is one settled-or-running native message row in a
// read-only OpenCode history snapshot. Rows come from the session_message
// table (current shapes) or the legacy message/part tables; the snapshot
// reader fills them without writing to the source.
type OpenCodeHistoryRow struct {
	MessageID  string
	Shape      OpenCodeProvenanceShape
	Seq        int64
	HasSeq     bool
	NativeType string
	CreatedMs  int64
	UpdatedMs  int64
	// CompletedMs carries the native completion clock when the row proves one.
	CompletedMs int64
	// Settled is true only when the row decoder proved the row complete: no
	// running shell, no running or pending tool part, no running compaction.
	// Unsettled rows are omitted from copy proof and from emission.
	Settled bool
	// SourceMessageID and SourceSessionID name the captured source row a copied
	// row was materialized from. They are empty for the session's own rows and
	// record ownership only: a copied child block keeps its own child identity
	// and never merges references across the two sessions.
	SourceMessageID string
	SourceSessionID string
	// Message is the decoded payload the classifier consumes.
	Message OpenCodeProvenanceMessage
}

// OpenCodeForkProof is the typed fork evidence for one child session: the
// source session plus the resolved before/through source-message sequence
// anchor. A nil anchor pair means the boundary is unproven and copies stay
// uncertain; it never licenses inference by timestamp or text equality.
type OpenCodeForkProof struct {
	SourceSessionID  string
	BeforeSeq        *int64
	ThroughSeq       *int64
	ThroughCompleted bool
}

// OpenCodeRevertState is the typed revert evidence for one session. Staged and
// cleared reverts change metadata only and keep every row; a committed revert
// removes materialized rows at or after the target while later appends stay
// valid. The target names the session's own sequence space.
type OpenCodeRevertState struct {
	TargetSeq int64
	Committed bool
}

// OpenCodeHistorySnapshot is one read-only OpenCode snapshot: the child's own
// current rows, the settled parent payloads captured at copy time, and the
// fork, revert, and delivery evidence the materializer decides on. Copied rows
// carry their new child message IDs with their source sequence positions; the
// materializer records that alias for ownership only and never merges refs
// across the two sessions.
type OpenCodeHistorySnapshot struct {
	SessionID         string
	ParentID          string
	HasParent         bool
	ParentNullProven  bool
	Messages          []OpenCodeHistoryRow
	Copied            []OpenCodeHistoryRow
	AgentDeliveredIDs map[string]bool
	Fork              *OpenCodeForkProof
	Revert            *OpenCodeRevertState
	// SourceEvidenceDigest is the SHA-256 hex of the canonical snapshot fields
	// the reader captured. The capture builder carries it into the generation
	// so activation can verify what the candidate proves.
	SourceEvidenceDigest string
}

// OpenCodeMaterializedMessage is one snapshot row with its decided ownership:
// emitted main content, or retained uncertain evidence for a declared earlier
// section.
type OpenCodeMaterializedMessage struct {
	Row         OpenCodeHistoryRow
	Attribution OpenCodeMessageAttribution
	Emit        bool
	// RetainedSegment names the ordered captured-context segment that owns this
	// message's blocks when it is proven inherited context. It is set only for
	// an emitted inherited copy; the builder retains those blocks as local
	// evidence under the segment instead of emitting them as child chat.
	RetainedSegment *int
	EarlierIndex    int // 1-based earlier section when Emit is false but retained
}

// OpenCodeMaterializedHistory is the settled copy proof plus the emission
// plan: ordered context segments, per-message attribution, and the
// completeness the candidate may claim.
type OpenCodeMaterializedHistory struct {
	SessionID    string
	Messages     []OpenCodeMaterializedMessage
	Segments     []indexformat.ContextSegment
	Completeness indexformat.GenerationCompleteness
	Diagnostics  []string
}

// OpenCodeSnapshotError is an actionable materialization refusal naming the
// session, the failed step, and the safe recovery. It never carries raw native
// content, paths, or payload bytes.
type OpenCodeSnapshotError struct {
	SessionID string
	Step      string
	Reason    string
	Recovery  string
}

func (e *OpenCodeSnapshotError) Error() string {
	return fmt.Sprintf("materialize OpenCode history for session %q failed at %s: %s; recovery: %s", e.SessionID, e.Step, e.Reason, e.Recovery)
}

// MaterializeOpenCodeHistory resolves one read-only snapshot into settled copy
// proof and an emission plan following section 5.3. Settled rows at or below
// the proven boundary copy with their sequence gaps and times preserved;
// unfinished rows are omitted; a parent without fork proof copies nothing; a
// committed revert drops rows at or after its target while staged reverts keep
// them; and a missing parent payload after a valid copy retains uncertain
// evidence instead of blanking the child. The snapshot is never mutated and
// the source is never written.
func MaterializeOpenCodeHistory(snapshot OpenCodeHistorySnapshot) (OpenCodeMaterializedHistory, error) {
	if strings.TrimSpace(snapshot.SessionID) == "" {
		return OpenCodeMaterializedHistory{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "validate snapshot identity", Reason: "session identity is empty", Recovery: "snapshot the session row before materializing"}
	}
	out := OpenCodeMaterializedHistory{
		SessionID:    snapshot.SessionID,
		Completeness: indexformat.GenerationCompletenessComplete,
	}
	if snapshot.Fork == nil && len(snapshot.Copied) > 0 {
		return OpenCodeMaterializedHistory{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "prove fork copy", Reason: "the snapshot carries copied rows with no fork proof", Recovery: "supply the fork session and resolved before/through anchor, or snapshot without copied rows"}
	}
	if snapshot.Fork != nil {
		if strings.TrimSpace(snapshot.Fork.SourceSessionID) == "" {
			return OpenCodeMaterializedHistory{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "prove fork copy", Reason: "the fork proof names no source session", Recovery: "record the native fork source session before materializing"}
		}
		if snapshot.Fork.ThroughSeq != nil && !snapshot.Fork.ThroughCompleted {
			out.Completeness = indexformat.GenerationCompletenessIncompleteNew
			out.Diagnostics = append(out.Diagnostics, "fork through anchor targets an unfinished row; the copy boundary is unproven and no inherited row is admitted")
		}
	}
	own, droppedByRevert := applyOpenCodeRevert(snapshot)
	segments, copied, boundaryIncomplete := resolveOpenCodeCopies(snapshot)
	out.Segments = segments
	if boundaryIncomplete {
		out.Completeness = indexformat.GenerationCompletenessIncompleteNew
	}
	for _, row := range droppedByRevert {
		out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("committed revert removed materialized row seq %d from the active capture; later appends remain valid", row.Seq))
	}
	for _, row := range copied {
		out.Messages = append(out.Messages, row)
	}
	earlierIndex := 0
	if needsOpenCodeUncertainSection(out.Messages) {
		earlierIndex = 1
	}
	for i := range out.Messages {
		if out.Messages[i].Attribution.UncertainCopy {
			out.Messages[i].EarlierIndex = earlierIndex
		}
	}
	for _, row := range own {
		if !row.Settled {
			out.Completeness = indexformat.GenerationCompletenessIncompleteNew
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("unfinished own row %q is omitted from the capture; retry after it settles", row.MessageID))
			continue
		}
		if snapshot.Revert != nil && snapshot.Revert.Committed && !row.HasSeq {
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("committed revert cannot place row %q without a sequence position, so the row is retained; verify the revert target against settled rows", row.MessageID))
		}
		out.Messages = append(out.Messages, OpenCodeMaterializedMessage{Row: row, Attribution: LocalOpenCodeAttribution(), Emit: true})
	}
	sort.SliceStable(out.Messages, func(i, j int) bool {
		return openCodeEmissionOrder(out.Messages[i]) < openCodeEmissionOrder(out.Messages[j])
	})
	return out, nil
}

// openCodeEmissionOrder keeps inherited copies (by source sequence) ahead of
// the child's own rows (by child sequence): the captured prefix reads first.
func openCodeEmissionOrder(msg OpenCodeMaterializedMessage) int64 {
	if msg.Attribution.Inherited {
		return msg.Row.Seq - (1 << 62)
	}
	return msg.Row.Seq
}

// applyOpenCodeRevert drops own rows a committed revert removed. Staged and
// cleared reverts keep every row: they change metadata only.
func applyOpenCodeRevert(snapshot OpenCodeHistorySnapshot) (kept []OpenCodeHistoryRow, dropped []OpenCodeHistoryRow) {
	if snapshot.Revert == nil || !snapshot.Revert.Committed {
		return snapshot.Messages, nil
	}
	for _, row := range snapshot.Messages {
		if row.HasSeq && row.Seq >= snapshot.Revert.TargetSeq {
			dropped = append(dropped, row)
			continue
		}
		kept = append(kept, row)
	}
	return kept, dropped
}

// resolveOpenCodeCopies admits settled copied rows at or below the proven
// boundary with gaps preserved, and builds the ordered context segment. A copy
// without a sequence position cannot prove it sits below the boundary, so it
// stays uncertain; conflicting dual anchors keep the disputed range uncertain
// instead of silently dropping it. It reports whether the boundary left the
// capture incomplete.
func resolveOpenCodeCopies(snapshot OpenCodeHistorySnapshot) ([]indexformat.ContextSegment, []OpenCodeMaterializedMessage, bool) {
	if snapshot.Fork == nil {
		return nil, nil, false
	}
	if snapshot.Fork.ThroughSeq != nil && !snapshot.Fork.ThroughCompleted {
		uncertain := make([]OpenCodeMaterializedMessage, 0, len(snapshot.Copied))
		for _, row := range snapshot.Copied {
			uncertain = append(uncertain, OpenCodeMaterializedMessage{
				Row:         row,
				Attribution: OpenCodeMessageAttribution{Ownership: schema.ContentOwnershipUncertain, UncertainCopy: true},
			})
		}
		return nil, uncertain, true
	}
	admitBelow, disputeBelow, hasBound := openCodeCopyBounds(snapshot.Fork)
	var admitted []OpenCodeHistoryRow
	var uncertain []OpenCodeHistoryRow
	for _, row := range snapshot.Copied {
		if !row.Settled {
			continue
		}
		if !hasBound || !row.HasSeq {
			uncertain = append(uncertain, row)
			continue
		}
		switch {
		case row.Seq < admitBelow:
			admitted = append(admitted, row)
		case row.Seq < disputeBelow:
			uncertain = append(uncertain, row)
		default:
			continue
		}
	}
	var messages []OpenCodeMaterializedMessage
	// OpenCode materialization records one inherited range at most; when any
	// row is admitted, that segment is the single segment built below.
	segmentIndex := -1
	if len(admitted) > 0 {
		segmentIndex = 0
	}
	for _, row := range admitted {
		message := OpenCodeMaterializedMessage{
			Row:         row,
			Attribution: OpenCodeMessageAttribution{Ownership: schema.ContentOwnershipInherited, Inherited: true},
			Emit:        true,
		}
		if segmentIndex >= 0 {
			index := segmentIndex
			message.RetainedSegment = &index
		}
		messages = append(messages, message)
	}
	incomplete := false
	for _, row := range uncertain {
		incomplete = true
		messages = append(messages, OpenCodeMaterializedMessage{
			Row:         row,
			Attribution: OpenCodeMessageAttribution{Ownership: schema.ContentOwnershipUncertain, UncertainCopy: true},
		})
	}
	var segments []indexformat.ContextSegment
	if len(admitted) > 0 {
		min, max := admitted[0].Seq, admitted[0].Seq
		for _, row := range admitted[1:] {
			if row.Seq < min {
				min = row.Seq
			}
			if row.Seq > max {
				max = row.Seq
			}
		}
		end := max + 1
		if hasBound && admitBelow < end {
			end = admitBelow
		}
		if end < min {
			end = min
		}
		source := schema.SessionID(snapshot.Fork.SourceSessionID)
		segments = append(segments, indexformat.ContextSegment{
			Ordinal:          0,
			LogicalSessionID: &source,
			PhysicalSourceID: "opencode-session:" + snapshot.SessionID,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:         indexformat.CoordinateKindOpenCodeSequenceRange,
				Start:        &min,
				EndExclusive: &end,
			},
			Inclusion: indexformat.SegmentInclusionInherited,
		})
	}
	return segments, messages, incomplete
}

// openCodeCopyBounds converts the fork anchors to exclusive bounds. Rows below
// every anchor are admitted; rows inside a disputed dual-anchor range stay
// uncertain; rows at or above every anchor are outside the proof.
func openCodeCopyBounds(fork *OpenCodeForkProof) (admitBelow, disputeBelow int64, hasBound bool) {
	admitBelow, disputeBelow = 0, 0
	if fork.ThroughSeq != nil && fork.ThroughCompleted {
		admitBelow, disputeBelow, hasBound = *fork.ThroughSeq+1, *fork.ThroughSeq+1, true
	}
	if fork.BeforeSeq != nil {
		if !hasBound {
			admitBelow, disputeBelow, hasBound = *fork.BeforeSeq, *fork.BeforeSeq, true
		} else if *fork.BeforeSeq < admitBelow {
			disputeBelow, admitBelow = admitBelow, *fork.BeforeSeq
		} else if *fork.BeforeSeq > admitBelow {
			disputeBelow = *fork.BeforeSeq
		}
	}
	return admitBelow, disputeBelow, hasBound
}

// needsOpenCodeUncertainSection reports whether any retained message needs a
// declared earlier-history section.
func needsOpenCodeUncertainSection(messages []OpenCodeMaterializedMessage) bool {
	for _, msg := range messages {
		if msg.Attribution.UncertainCopy {
			return true
		}
	}
	return false
}

// DigestOpenCodeSnapshot computes the canonical source-evidence digest over the
// complete captured snapshot: session and parent identities, every captured
// row's identity, sequence, settled state and decoded payload bytes, the
// per-input delivery correlation, and the fork, revert, and boundary evidence.
// The digest binds the captured content to the capture, so a same-identity row
// whose payload changed can never keep the same proof. It is deterministic,
// length-framed and local-only; parent records outside the checked copy cutoff
// never enter it.
func DigestOpenCodeSnapshot(snapshot OpenCodeHistorySnapshot) string {
	var sb strings.Builder
	writeSnapshotField(&sb, "session", snapshot.SessionID)
	writeSnapshotField(&sb, "parent", snapshot.ParentID)
	if snapshot.HasParent {
		sb.WriteString("has-parent;")
	}
	if snapshot.ParentNullProven {
		sb.WriteString("parent-null-proven;")
	}
	writeSnapshotDelivery(&sb, snapshot.AgentDeliveredIDs)
	rows := append([]OpenCodeHistoryRow(nil), snapshot.Messages...)
	rows = append(rows, snapshot.Copied...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MessageID != rows[j].MessageID {
			return rows[i].MessageID < rows[j].MessageID
		}
		return rows[i].Seq < rows[j].Seq
	})
	for _, row := range rows {
		writeSnapshotField(&sb, "row", row.MessageID)
		fmt.Fprintf(&sb, "seq=%d hasSeq=%t settled=%t shape=%s created=%d updated=%d completed=%d;", row.Seq, row.HasSeq, row.Settled, row.Shape, row.CreatedMs, row.UpdatedMs, row.CompletedMs)
		writeSnapshotField(&sb, "native-type", row.NativeType)
		if row.SourceMessageID != "" || row.SourceSessionID != "" {
			writeSnapshotField(&sb, "source-row", row.SourceMessageID)
			writeSnapshotField(&sb, "source-session", row.SourceSessionID)
		}
		writeSnapshotMessage(&sb, row.Message)
	}
	if snapshot.Fork != nil {
		writeSnapshotField(&sb, "fork", snapshot.Fork.SourceSessionID)
		if snapshot.Fork.BeforeSeq != nil {
			fmt.Fprintf(&sb, "before=%d;", *snapshot.Fork.BeforeSeq)
		}
		if snapshot.Fork.ThroughSeq != nil {
			fmt.Fprintf(&sb, "through=%d completed=%t;", *snapshot.Fork.ThroughSeq, snapshot.Fork.ThroughCompleted)
		}
	}
	if snapshot.Revert != nil {
		fmt.Fprintf(&sb, "revert=%d committed=%t;", snapshot.Revert.TargetSeq, snapshot.Revert.Committed)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// writeSnapshotDelivery frames the ordered per-input delivery correlation so a
// changed delivery correlation changes the proof.
func writeSnapshotDelivery(sb *strings.Builder, delivered map[string]bool) {
	if len(delivered) == 0 {
		return
	}
	keys := make([]string, 0, len(delivered))
	for key, value := range delivered {
		if value {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	fmt.Fprintf(sb, "delivery=%d;", len(keys))
	for _, key := range keys {
		writeSnapshotField(sb, "delivered", key)
	}
}

// writeSnapshotMessage frames every classification-relevant decoded payload
// field of one captured message, so a payload edit changes the digest while the
// canonical encoding stays deterministic across runs.
func writeSnapshotMessage(sb *strings.Builder, msg OpenCodeProvenanceMessage) {
	writeSnapshotField(sb, "m.msgid", msg.MessageID)
	writeSnapshotField(sb, "m.session", msg.SessionID)
	writeSnapshotField(sb, "m.shape", string(msg.Shape))
	writeSnapshotField(sb, "m.type", msg.NativeType)
	fmt.Fprintf(sb, "m.seq=%d hasSeq=%t times=%d,%d;", msg.Seq, msg.HasSeq, msg.TimeCreated, msg.TimeCompleted)
	fmt.Fprintf(sb, "m.flags=%t,%t,%t,%t,%t,%t,%t;", msg.ParentNullProven, msg.HasParent, msg.AgentDelivered, msg.ShellCompleted, msg.HasShellID, msg.CompactionCompleted, msg.AllPartsSynthetic)
	writeSnapshotField(sb, "m.role", string(msg.Role))
	writeSnapshotField(sb, "m.text", msg.Text)
	writeSnapshotField(sb, "m.system", msg.SystemText)
	writeSnapshotField(sb, "m.shellcall", msg.ShellCallID)
	writeSnapshotField(sb, "m.shellcmd", msg.ShellCommand)
	writeSnapshotField(sb, "m.shellout", msg.ShellOutput)
	writeSnapshotField(sb, "m.compaction", msg.CompactionSummary)
	fmt.Fprintf(sb, "m.files=%d;", len(msg.Files))
	for _, file := range msg.Files {
		writeSnapshotField(sb, "m.file.name", file.Name)
		writeSnapshotField(sb, "m.file.uri", file.URI)
		writeSnapshotField(sb, "m.file.desc", file.Description)
	}
	fmt.Fprintf(sb, "m.agents=%d;", len(msg.AgentMentions))
	for _, agent := range msg.AgentMentions {
		writeSnapshotField(sb, "m.agent", agent)
	}
	fmt.Fprintf(sb, "m.skills=%d;", len(msg.SkillTexts))
	for _, skill := range msg.SkillTexts {
		writeSnapshotField(sb, "m.skill", skill)
	}
	fmt.Fprintf(sb, "m.parts=%d;", len(msg.Parts))
	for _, part := range msg.Parts {
		writeSnapshotField(sb, "m.part.id", part.ID)
		writeSnapshotField(sb, "m.part.kind", part.Kind)
		writeSnapshotField(sb, "m.part.text", part.Text)
		writeSnapshotField(sb, "m.part.name", part.ToolName)
		writeSnapshotField(sb, "m.part.input", part.ToolInput)
		writeSnapshotField(sb, "m.part.result", part.ToolResult)
		fmt.Fprintf(sb, "m.part.done=%t;", part.ToolCompleted)
	}
}

func writeSnapshotField(sb *strings.Builder, name, value string) {
	fmt.Fprintf(sb, "%s=%d:%s;", name, len(value), value)
}
