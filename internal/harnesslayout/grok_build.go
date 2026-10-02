package harnesslayout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

const ToolGrokBuild Tool = "grok-build"

const (
	grokSubagentsDir = "subagents"
	grokSubagentMeta = "meta.json"
	grokCwdMarker    = ".cwd"
	grokUserChunk    = "user_message_chunk"
	grokAgentChunk   = "agent_message_chunk"
	grokToolCall     = "tool_call"
)

var (
	grokBuildSummary = Artifact{
		Name:        "summary",
		Pattern:     "sessions/*/*/summary.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "session index entry: info.id, info.cwd, titles, created_at/updated_at, message counts, current_model_id",
	}
	grokBuildUpdates = Artifact{
		Name:        "updates",
		Pattern:     "sessions/*/*/updates.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "authoritative ACP session update stream; one {timestamp, method, params} envelope per line",
	}
	grokBuildChatHistory = Artifact{
		Name:        "chat-history",
		Pattern:     "sessions/*/*/chat_history.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "conversation items sent to the model, tagged by type (system, user, assistant, tool_result, backend_tool_call, reasoning)",
	}
	grokBuildSignals = Artifact{
		Name:        "signals",
		Pattern:     "sessions/*/*/signals.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "session counters: turns, messages, tool calls and failures, token and latency signals",
	}
	grokBuildUsage = Artifact{
		Name:        "usage",
		Pattern:     "sessions/*/*/usage.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "per-session usage totals",
	}
	grokBuildPlan = Artifact{
		Name:        "plan",
		Pattern:     "sessions/*/*/plan.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "TODO and task list state",
	}
	grokBuildRewindPoints = Artifact{
		Name:        "rewind-points",
		Pattern:     "sessions/*/*/rewind_points.jsonl",
		Format:      FormatJSONL,
		Role:        RoleAuxiliary,
		Description: "file snapshots that /rewind restores",
	}
	grokBuildFeedback = Artifact{
		Name:        "feedback",
		Pattern:     "sessions/*/*/feedback.jsonl",
		Format:      FormatJSONL,
		Role:        RoleAuxiliary,
		Description: "user feedback and ratings",
	}
	grokBuildSubagentMeta = Artifact{
		Name:        "subagent-meta",
		Pattern:     "sessions/*/*/subagents/*/meta.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "one file per spawned subagent; the child session lives in the normal sessions tree",
	}
	grokBuildCwdMarker = Artifact{
		Name:        "cwd-marker",
		Pattern:     "sessions/*/.cwd",
		Format:      FormatText,
		Role:        RoleIndex,
		Description: "original working directory, written when the URL-encoded group name would exceed 255 bytes",
	}
	grokBuildPromptHistory = Artifact{
		Name:        "prompt-history",
		Pattern:     "sessions/*/prompt_history.jsonl",
		Format:      FormatJSONL,
		Role:        RoleAuxiliary,
		Description: "per-working-directory prompt recall history, shared by every session in the group",
	}
	grokBuildSearchIndex = Artifact{
		Name:        "search-index",
		Pattern:     "sessions/session_search.sqlite",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "SQLite FTS5 index over session titles and prompts; derived from the JSONL files",
	}
)

// grokBuildSessionArtifacts are the per-session files a capture shapes, in
// report order.
var grokBuildSessionArtifacts = []Artifact{
	grokBuildSummary,
	grokBuildUpdates,
	grokBuildChatHistory,
	grokBuildSignals,
	grokBuildUsage,
	grokBuildPlan,
	grokBuildRewindPoints,
	grokBuildFeedback,
}

var grokBuildShapedArtifacts = append(slices.Clip(grokBuildSessionArtifacts), grokBuildSubagentMeta)

type grokBuildProbe struct{}

var _ Probe = grokBuildProbe{}

func init() {
	Register(Layout{
		Tool:        ToolGrokBuild,
		DisplayName: "Grok Build",
		Sessions:    "One directory per session under sessions/<URL-encoded cwd>/<session id>/, holding summary.json, updates.jsonl, and chat_history.jsonl.",
		Roots: []Root{
			{Path: "{home}/.grok"},
		},
		Artifacts: []Artifact{
			grokBuildSummary,
			grokBuildUpdates,
			grokBuildChatHistory,
			grokBuildSignals,
			grokBuildUsage,
			grokBuildPlan,
			grokBuildRewindPoints,
			grokBuildFeedback,
			grokBuildSubagentMeta,
			grokBuildCwdMarker,
			grokBuildPromptHistory,
			grokBuildSearchIndex,
		},
		Sources: []string{
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/17-sessions.md",
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/session/storage/mod.rs",
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/session/persistence.rs",
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-sampling-types/src/conversation.rs",
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-config/src/paths.rs",
			"https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-dirs/src/lib.rs",
			"https://docs.x.ai/build/settings/reference",
		},
		Probe: grokBuildProbe{},
	})
}

func (grokBuildProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	dirs := map[string]struct{}{}
	for _, marker := range []string{grokBuildSummary.Pattern, grokBuildUpdates.Pattern} {
		matches, err := fs.Glob(src.FS, marker)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			dirs[path.Dir(match)] = struct{}{}
		}
	}
	refs := make([]SessionRef, 0, len(dirs))
	for dir := range dirs {
		var paths []string
		for _, artifact := range grokBuildSessionArtifacts {
			name := path.Join(dir, path.Base(artifact.Pattern))
			if info, err := fs.Stat(src.FS, name); err == nil && !info.IsDir() {
				paths = append(paths, name)
			}
		}
		entries, _ := fs.ReadDir(src.FS, path.Join(dir, grokSubagentsDir))
		for _, entry := range entries {
			name := path.Join(dir, grokSubagentsDir, entry.Name(), grokSubagentMeta)
			if info, err := fs.Stat(src.FS, name); err == nil && !info.IsDir() {
				paths = append(paths, name)
			}
		}
		refs = append(refs, SessionRef{ID: path.Base(dir), Paths: paths})
	}
	return refs, nil
}

// grokBuildCounts accumulates turn counts from both transcripts; the chat
// history is preferred because updates.jsonl stores streamed chunks.
type grokBuildCounts struct {
	chatSeen                            bool
	chatUser, chatAssistant, chatTool   int
	updateUser, updateAgent, updateTool int
	lastUpdateKind                      string
}

func (grokBuildProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	if len(ref.Paths) == 0 {
		return Capture{}, fmt.Errorf("grok-build session %s has no artifacts", ref.ID)
	}
	dir := grokSessionDir(ref.Paths[0])
	meta := Metadata{SessionID: ref.ID}
	var counts grokBuildCounts
	var shapes []ArtifactShape
	for _, name := range ref.Paths {
		artifact, ok := grokBuildArtifactFor(name)
		if !ok {
			continue
		}
		shape, err := grokBuildShape(src, artifact, name, &meta, &counts)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Capture{}, err
		}
		shapes = append(shapes, shape)
	}
	if meta.ProjectPath == "" {
		meta.ProjectPath = grokDecodeCwd(src, path.Dir(dir))
	}
	if counts.chatSeen {
		meta.UserTurns, meta.AssistantMsgs, meta.ToolCalls = counts.chatUser, counts.chatAssistant, counts.chatTool
	} else {
		meta.UserTurns, meta.AssistantMsgs, meta.ToolCalls = counts.updateUser, counts.updateAgent, counts.updateTool
	}
	return Capture{Session: ref.ID, Metadata: meta, Shapes: shapes}, nil
}

func grokBuildShape(src Source, artifact Artifact, name string, meta *Metadata, counts *grokBuildCounts) (ArtifactShape, error) {
	file, err := src.FS.Open(name)
	if err != nil {
		return ArtifactShape{}, err
	}
	defer file.Close()
	rec := NewShapeRecorder(artifact, name)
	switch artifact.Name {
	case grokBuildSummary.Name:
		document, err := ShapeJSON(file, rec, nil, nil)
		if err == nil {
			grokObserveSummary(document, meta)
		}
	case grokBuildUpdates.Name:
		err = ShapeJSONL(file, rec, grokUpdateKind, func(record any) {
			kind := grokUpdateKind(record)
			meta.Observe(TimeField(record, "timestamp"), "")
			switch kind {
			case grokUserChunk:
				if counts.lastUpdateKind != grokUserChunk {
					counts.updateUser++
				}
			case grokAgentChunk:
				if counts.lastUpdateKind != grokAgentChunk {
					counts.updateAgent++
				}
			case grokToolCall:
				counts.updateTool++
			}
			counts.lastUpdateKind = kind
		})
	case grokBuildChatHistory.Name:
		err = ShapeJSONL(file, rec, grokChatKind, func(record any) {
			counts.chatSeen = true
			switch grokChatKind(record) {
			case "user":
				if reason := StringField(record, "synthetic_reason"); reason == "" || reason == "human" {
					counts.chatUser++
				}
			case "assistant":
				counts.chatAssistant++
				counts.chatTool += len(ArrayField(record, "tool_calls"))
				meta.Observe(time.Time{}, StringField(record, "model_id"))
			case "backend_tool_call":
				counts.chatTool++
			}
		})
	case grokBuildSubagentMeta.Name:
		_, err = ShapeJSON(file, rec, nil, KindField("status"))
	case grokBuildRewindPoints.Name, grokBuildFeedback.Name:
		err = ShapeJSONL(file, rec, KindField("type"), nil)
	default:
		_, err = ShapeJSON(file, rec, nil, nil)
	}
	if err != nil && rec.Shape().Malformed == 0 {
		return ArtifactShape{}, fmt.Errorf("%s: %w", name, err)
	}
	return rec.Shape(), nil
}

func grokObserveSummary(document any, meta *Metadata) {
	if id := StringField(document, "info", "id"); id != "" {
		meta.SessionID = id
	}
	meta.ProjectPath = StringField(document, "info", "cwd")
	meta.Title = StringField(document, "generated_title")
	if meta.Title == "" {
		meta.Title = StringField(document, "session_summary")
	}
	model := StringField(document, "current_model_id")
	for _, key := range []string{"created_at", "updated_at", "last_active_at"} {
		meta.Observe(TimeField(document, key), model)
	}
}

func grokUpdateKind(record any) string {
	return StringField(record, "params", "update", "sessionUpdate")
}

// grokChatKind reads the ConversationItem "type" tag, or "role" in the
// legacy chat_format_version 0 messages.
func grokChatKind(record any) string {
	if kind := StringField(record, "type"); kind != "" {
		return kind
	}
	return StringField(record, "role")
}

func grokBuildArtifactFor(name string) (Artifact, bool) {
	for _, artifact := range grokBuildShapedArtifacts {
		if ok, _ := path.Match(artifact.Pattern, name); ok {
			return artifact, true
		}
	}
	return Artifact{}, false
}

// grokSessionDir returns sessions/<cwd>/<id> for any path inside it.
func grokSessionDir(name string) string {
	parts := strings.SplitN(name, "/", 4)
	if len(parts) < 3 {
		return path.Dir(name)
	}
	return path.Join(parts[:3]...)
}

// grokDecodeCwd mirrors decode_cwd_from_dirname: a URL-decoded absolute
// path wins, otherwise the .cwd marker of the slug-hash form.
func grokDecodeCwd(src Source, group string) string {
	if decoded, err := url.PathUnescape(path.Base(group)); err == nil {
		if strings.HasPrefix(decoded, "/") || (len(decoded) > 2 && decoded[1] == ':') {
			return decoded
		}
	}
	raw, err := fs.ReadFile(src.FS, path.Join(group, grokCwdMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
