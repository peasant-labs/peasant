package harnesslayout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
)

// ToolCline is the Cline coding agent: the VS Code extension
// (saoudrizwan.claude-dev), its JetBrains plugin, and the Cline CLI.
const ToolCline Tool = "cline"

const (
	clineTasksDir       = "tasks"
	clineSessionsDir    = "sessions"
	clineTaskHistory    = "state/taskHistory.json"
	clineMessagesSuffix = ".messages.json"
	clineCompactSuffix  = ".compaction.json"
	clineTitleMaxRunes  = 240
	clineExtensionDir   = "User/globalStorage/saoudrizwan.claude-dev"
)

var (
	clineAPIHistory = Artifact{
		Name:        "api-conversation-history",
		Pattern:     "tasks/*/api_conversation_history.json",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "legacy task: array of Anthropic-style messages (role, content blocks) with optional id, ts, modelInfo, and metrics",
	}
	clineUIMessages = Artifact{
		Name:        "ui-messages",
		Pattern:     "tasks/*/ui_messages.json",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "legacy task: array of rendered chat rows, each a say or ask message with ts",
	}
	clineTaskMetadata = Artifact{
		Name:        "task-metadata",
		Pattern:     "tasks/*/task_metadata.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "legacy task: files_in_context, model_usage, and environment_history",
	}
	clineContextHistory = Artifact{
		Name:        "context-history",
		Pattern:     "tasks/*/context_history.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "legacy task: context-window edits keyed by message index",
	}
	clineTaskSettings = Artifact{
		Name:        "task-settings",
		Pattern:     "tasks/*/settings.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "legacy task: per-task settings overrides",
	}
	clineHistoryIndex = Artifact{
		Name:        "task-history",
		Pattern:     clineTaskHistory,
		Format:      FormatJSON,
		Role:        RoleIndex,
		Description: "legacy: array of history items, one per task, with task text, cwd, model, and token totals",
	}
	clineManifest = Artifact{
		Name:        "session-manifest",
		Pattern:     "sessions/*/*.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "SDK session: <sessionId>.json with source, status, provider, model, cwd, prompt, times, and metadata",
	}
	clineMessages = Artifact{
		Name:        "session-messages",
		Pattern:     "sessions/*/*.messages.json",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "SDK session: <sessionId>.messages.json envelope whose messages array holds role, content blocks, ts, modelInfo, and metrics",
	}
	clineSubagentMessages = Artifact{
		Name:        "subagent-messages",
		Pattern:     "sessions/*/*.messages.json",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "SDK session: <agentId>.messages.json or <agentId>__<teamTaskId>.messages.json for each sub-agent or teammate",
	}
	clineCompaction = Artifact{
		Name:        "session-compaction",
		Pattern:     "sessions/*/*.compaction.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "SDK session: <sessionId>.compaction.json with the compacted message list",
	}
	clineSessionIndex = Artifact{
		Name:        "session-index",
		Pattern:     "db/sessions.db",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "SDK: SQLite index of sessions; not captured, the manifests carry the same fields",
	}
)

// clineTaskFiles lists the per-task files in capture order.
var clineTaskFiles = []struct {
	file     string
	artifact Artifact
}{
	{"api_conversation_history.json", clineAPIHistory},
	{"ui_messages.json", clineUIMessages},
	{"task_metadata.json", clineTaskMetadata},
	{"context_history.json", clineContextHistory},
	{"settings.json", clineTaskSettings},
}

// clineUIToolKinds are the ask and say values that stand for one tool call
// in tasks recorded before native tool_use blocks.
var clineUIToolKinds = []string{"tool", "command", "use_mcp_server", "browser_action_launch"}

var (
	clineModeNotice = regexp.MustCompile(`(?s)<mode_notice>.*?</mode_notice>`)
	clineInputTag   = regexp.MustCompile(`</?user_(?:input|command)\b[^>]*>`)
)

func clineRoot(product string) Root {
	return Root{Path: "{config}/" + product + "/" + clineExtensionDir}
}

func init() {
	Register(Layout{
		Tool:        ToolCline,
		DisplayName: "Cline",
		Sessions:    "One legacy task directory (tasks/<taskId>/) or one SDK session directory (sessions/<sessionId>/) per session.",
		Roots: []Root{
			{Path: "{home}/.cline/data"},
			clineRoot("Code"),
			clineRoot("Code - Insiders"),
			clineRoot("VSCodium"),
			clineRoot("Cursor"),
			clineRoot("Windsurf"),
			clineRoot("Antigravity"),
			{OS: []OS{OSLinux}, Path: "{home}/.vscode-server/data/" + clineExtensionDir},
		},
		Artifacts: []Artifact{
			clineAPIHistory, clineUIMessages, clineTaskMetadata, clineContextHistory, clineTaskSettings,
			clineHistoryIndex, clineManifest, clineMessages, clineSubagentMessages, clineCompaction, clineSessionIndex,
		},
		Sources: []string{
			"https://github.com/cline/cline/blob/v3.89.2/apps/vscode/src/core/storage/disk.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/sdk/legacy-state-reader.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/hosts/vscode/vscode-to-file-migration.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/messages/content.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/ExtensionMessage.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/HistoryItem.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/core/context/context-tracking/ContextTrackerTypes.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/shared/src/storage/paths.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/services/session-artifacts.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/session/models/session-manifest.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/services/session-data.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/shared/src/llms/messages.ts",
			"https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/CHANGELOG.md",
			"https://docs.cline.bot/getting-started/installing-cline",
		},
		Probe: clineProbe{},
	})
}

type clineProbe struct{}

var _ Probe = clineProbe{}

// Discover returns one reference per directory under tasks/ and sessions/.
// The reference ID keeps the family prefix, so a legacy task and an SDK
// session with the same ID stay distinct.
func (clineProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	var refs []SessionRef
	tasks, taskErr := clineSubdirs(src.FS, clineTasksDir)
	historyExists := clineExists(src.FS, clineTaskHistory)
	for _, id := range tasks {
		ref := SessionRef{ID: path.Join(clineTasksDir, id)}
		for _, task := range clineTaskFiles {
			if p := path.Join(clineTasksDir, id, task.file); clineExists(src.FS, p) {
				ref.Paths = append(ref.Paths, p)
			}
		}
		if historyExists {
			ref.Paths = append(ref.Paths, clineTaskHistory)
		}
		refs = append(refs, ref)
	}
	sessions, sessionErr := clineSubdirs(src.FS, clineSessionsDir)
	var readErrs []error
	for _, id := range sessions {
		dir := path.Join(clineSessionsDir, id)
		ref := SessionRef{ID: dir}
		entries, err := fs.ReadDir(src.FS, dir)
		if err != nil {
			readErrs = append(readErrs, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && path.Ext(entry.Name()) == ".json" {
				ref.Paths = append(ref.Paths, path.Join(dir, entry.Name()))
			}
		}
		refs = append(refs, ref)
	}
	return refs, errors.Join(append([]error{taskErr, sessionErr}, readErrs...)...)
}

func (clineProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	family, id, _ := strings.Cut(ref.ID, "/")
	switch family {
	case clineTasksDir:
		return captureClineTask(src.FS, id, ref.Paths)
	case clineSessionsDir:
		return captureClineSession(src.FS, id, ref.Paths)
	}
	return Capture{}, fmt.Errorf("cline: unknown session reference %q", ref.ID)
}

func captureClineTask(fsys fs.FS, id string, paths []string) (Capture, error) {
	meta := Metadata{SessionID: id}
	var (
		shapes            []ArtifactShape
		errs              []error
		decoded           bool
		uiSeen            bool
		uiTitle           string
		uiTurns, uiTools  int
		apiTurns, toolUse int
	)
	for _, task := range clineTaskFiles {
		p := path.Join(clineTasksDir, id, task.file)
		if !slices.Contains(paths, p) {
			continue
		}
		var (
			rec *ShapeRecorder
			doc any
			err error
		)
		switch task.artifact.Name {
		case clineAPIHistory.Name:
			rec, doc, err = shapeClineFile(fsys, task.artifact, p, clineDocumentArray, clineMessageKind)
			for _, message := range clineDocumentArray(doc) {
				meta.Observe(TimeField(message, "ts"), StringField(message, "modelInfo", "modelId"))
				turn, assistant, tools := clineCountMessage(message)
				apiTurns += turn
				meta.AssistantMsgs += assistant
				toolUse += tools
			}
		case clineUIMessages.Name:
			rec, doc, err = shapeClineFile(fsys, task.artifact, p, clineDocumentArray, clineUIKind)
			uiSeen = err == nil
			for _, message := range clineDocumentArray(doc) {
				meta.Observe(TimeField(message, "ts"), StringField(message, "modelInfo", "modelId"))
				say, ask := StringField(message, "say"), StringField(message, "ask")
				switch say {
				case "task":
					uiTurns++
					if uiTitle == "" {
						uiTitle = StringField(message, "text")
					}
				case "user_feedback":
					uiTurns++
				}
				if slices.Contains(clineUIToolKinds, say) || slices.Contains(clineUIToolKinds, ask) {
					uiTools++
				}
			}
		case clineTaskMetadata.Name:
			rec, doc, err = shapeClineFile(fsys, task.artifact, p, nil, nil)
			for _, usage := range ArrayField(doc, "model_usage") {
				meta.Observe(TimeField(usage, "ts"), StringField(usage, "model_id"))
			}
		default:
			rec, _, err = shapeClineFile(fsys, task.artifact, p, nil, nil)
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			decoded = true
		}
		shapes = append(shapes, rec.Shape())
	}
	if !decoded {
		return Capture{}, clineNoArtifacts(errs)
	}
	meta.UserTurns = apiTurns
	if uiSeen {
		meta.UserTurns = uiTurns
	}
	meta.ToolCalls = toolUse
	if toolUse == 0 {
		meta.ToolCalls = uiTools
	}
	if slices.Contains(paths, clineTaskHistory) {
		var entry any
		thisTask := func(document any) []any {
			for _, item := range clineDocumentArray(document) {
				if StringField(item, "id") == id {
					entry = item
					return []any{item}
				}
			}
			return nil
		}
		// A missing or malformed history leaves the task metadata to the task files.
		rec, _, _ := shapeClineFile(fsys, clineHistoryIndex, clineTaskHistory, thisTask, func(any) string { return "history_item" })
		if entry != nil {
			meta.Title = clineTitle(StringField(entry, "task"))
			meta.ProjectPath = StringField(entry, "cwdOnTaskInitialization")
			meta.Observe(TimeField(entry, "ts"), StringField(entry, "modelId"))
		}
		shapes = append(shapes, rec.Shape())
	}
	if meta.Title == "" {
		meta.Title = clineTitle(uiTitle)
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

func captureClineSession(fsys fs.FS, id string, paths []string) (Capture, error) {
	meta := Metadata{SessionID: id}
	dir := path.Join(clineSessionsDir, id)
	manifestPath := path.Join(dir, id+".json")
	messagesPath := path.Join(dir, id+clineMessagesSuffix)
	compactionPath := path.Join(dir, id+clineCompactSuffix)
	var (
		shapes  []ArtifactShape
		errs    []error
		decoded bool
	)
	record := func(rec *ShapeRecorder, err error) {
		if err != nil {
			errs = append(errs, err)
		} else {
			decoded = true
		}
		shapes = append(shapes, rec.Shape())
	}
	if slices.Contains(paths, manifestPath) {
		rec, doc, err := shapeClineFile(fsys, clineManifest, manifestPath, nil, clineManifestKind)
		if sessionID := StringField(doc, "session_id"); sessionID != "" {
			meta.SessionID = sessionID
		}
		meta.Title = clineTitle(StringField(doc, "metadata", "title"))
		if meta.Title == "" {
			meta.Title = clineTitle(StringField(doc, "prompt"))
		}
		meta.ProjectPath = StringField(doc, "cwd")
		if meta.ProjectPath == "" {
			meta.ProjectPath = StringField(doc, "workspace_root")
		}
		meta.Observe(TimeField(doc, "started_at"), StringField(doc, "model"))
		meta.Observe(TimeField(doc, "ended_at"), "")
		record(rec, err)
	}
	if slices.Contains(paths, messagesPath) {
		rec, doc, err := shapeClineFile(fsys, clineMessages, messagesPath, clineEnvelopeMessages, clineMessageKind)
		meta.Observe(TimeField(doc, "updated_at"), "")
		for _, message := range clineEnvelopeMessages(doc) {
			meta.Observe(TimeField(message, "ts"), StringField(message, "modelInfo", "id"))
			turn, assistant, tools := clineCountMessage(message)
			meta.UserTurns += turn
			meta.AssistantMsgs += assistant
			meta.ToolCalls += tools
		}
		record(rec, err)
	}
	for _, p := range paths {
		if p == messagesPath || !strings.HasSuffix(p, clineMessagesSuffix) {
			continue
		}
		rec, _, err := shapeClineFile(fsys, clineSubagentMessages, p, clineEnvelopeMessages, clineMessageKind)
		record(rec, err)
	}
	if slices.Contains(paths, compactionPath) {
		rec, _, err := shapeClineFile(fsys, clineCompaction, compactionPath, clineEnvelopeMessages, clineMessageKind)
		record(rec, err)
	}
	if !decoded {
		return Capture{}, clineNoArtifacts(errs)
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

// shapeClineFile shapes one JSON artifact. Its error names the artifact, not
// the path, because a shape-only report keeps failure messages.
func shapeClineFile(fsys fs.FS, artifact Artifact, p string, each func(any) []any, kind KindFunc) (*ShapeRecorder, any, error) {
	rec := NewShapeRecorder(artifact, p)
	file, err := fsys.Open(p)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return rec, nil, fmt.Errorf("%s: %w", artifact.Name, err)
	}
	defer file.Close()
	document, err := ShapeJSON(file, rec, each, kind)
	if err != nil {
		err = fmt.Errorf("%s: %w", artifact.Name, err)
	}
	return rec, document, err
}

func clineNoArtifacts(errs []error) error {
	if len(errs) == 0 {
		return errors.New("cline: no known session files")
	}
	return fmt.Errorf("cline: no session file decoded: %w", errors.Join(errs...))
}

func clineDocumentArray(document any) []any {
	array, _ := document.([]any)
	return array
}

func clineEnvelopeMessages(document any) []any {
	if array, ok := document.([]any); ok {
		return array
	}
	return ArrayField(document, "messages")
}

// clineMessageKind names a message by its role and the sorted set of its
// content block types, such as "assistant:text+tool_use" or "user:string".
func clineMessageKind(record any) string {
	role := StringField(record, "role")
	if role == "" {
		return ""
	}
	switch content := Field(record, "content").(type) {
	case string:
		return role + ":string"
	case []any:
		var types []string
		for _, block := range content {
			if t := StringField(block, "type"); t != "" && !slices.Contains(types, t) {
				types = append(types, t)
			}
		}
		if len(types) == 0 {
			return role + ":empty"
		}
		slices.Sort(types)
		return role + ":" + strings.Join(types, "+")
	}
	return role
}

func clineManifestKind(record any) string {
	if source := StringField(record, "source"); source != "" {
		return "source:" + source
	}
	return ""
}

func clineUIKind(record any) string {
	if say := StringField(record, "say"); say != "" {
		return "say:" + say
	}
	if ask := StringField(record, "ask"); ask != "" {
		return "ask:" + ask
	}
	return StringField(record, "type")
}

// clineCountMessage returns whether a message is a user turn, whether it is
// an assistant message, and its tool_use block count. A user message that
// carries a tool_result block answers a tool call and is not a turn.
func clineCountMessage(message any) (userTurn, assistant, toolCalls int) {
	blocks := ArrayField(message, "content")
	switch StringField(message, "role") {
	case "user":
		for _, block := range blocks {
			if StringField(block, "type") == "tool_result" {
				return 0, 0, 0
			}
		}
		return 1, 0, 0
	case "assistant":
		for _, block := range blocks {
			if StringField(block, "type") == "tool_use" {
				toolCalls++
			}
		}
		return 0, 1, toolCalls
	}
	return 0, 0, 0
}

func clineTitle(raw string) string {
	text := clineInputTag.ReplaceAllString(clineModeNotice.ReplaceAllString(raw, ""), "")
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > clineTitleMaxRunes {
		text = string(runes[:clineTitleMaxRunes-1]) + "…"
	}
	return text
}

func clineSubdirs(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, err
}

func clineExists(fsys fs.FS, p string) bool {
	info, err := fs.Stat(fsys, p)
	return err == nil && !info.IsDir()
}
