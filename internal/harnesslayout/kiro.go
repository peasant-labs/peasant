package harnesslayout

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

const ToolKiro Tool = "kiro"

var (
	kiroIDESessionMeta = Artifact{
		Name:        "session",
		Pattern:     "*/*/session.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "Kiro 1.0 and CLI 3.0 session header: id, title, modelId, workspacePaths, timestamps; the parent directory is a 16-hex workspace hash",
	}
	kiroIDESessionMessages = Artifact{
		Name:        "messages",
		Pattern:     "*/*/messages.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "Kiro 1.0 and CLI 3.0 append-only event log of {id, timestamp, payload{type}} records",
	}
	kiroIDEWorkspaceSession = Artifact{
		Name:        "workspace-session",
		Pattern:     "workspace-sessions/*/*.json",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "pre-1.0 session with a history[] of {message{role, content}, executionId}; the directory name is the base64url workspace path",
	}
	kiroIDEWorkspaceIndex = Artifact{
		Name:        "workspace-sessions-index",
		Pattern:     "workspace-sessions/*/sessions.json",
		Format:      FormatJSON,
		Role:        RoleIndex,
		Description: "pre-1.0 per-workspace list of sessions that the classic chat sidebar reads",
	}
	kiroIDEMigrationMarker = Artifact{
		Name:        "migration-marker",
		Pattern:     "workspace-sessions/*/.migrated-*.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "written by the 1.0 update beside a migrated pre-1.0 session: {migratedAt, v2SessionId}",
	}
	kiroIDELegacyChat = Artifact{
		Name:        "legacy-chat",
		Pattern:     "*/*.chat",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "earliest format: one execution per file with chat[] of {role human|bot|tool, content} and metadata{modelId, workflowId, startTime, endTime}",
	}
	kiroIDEExecution = Artifact{
		Name:        "execution",
		Pattern:     "*/*/*",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "pre-1.0 extensionless execution log under <workspace>/<32-hex>/<32-hex>: executionId, chatSessionId, input, actions[]",
	}
	kiroIDEExecutionIndex = Artifact{
		Name:        "execution-index",
		Pattern:     "*/*",
		Format:      FormatJSON,
		Role:        RoleIndex,
		Description: "extensionless 32-hex file beside the execution directories: {executions[{executionId, type, status, startTime}], version}",
	}
)

// kiroIDENotWorkspaces are kiro.kiroagent children that are not workspace
// hash directories.
var kiroIDENotWorkspaces = []string{"workspace-sessions", "dev_data"}

const kiroIDECLIDir = "cli"

type kiroIDEProbe struct{}

var _ Probe = kiroIDEProbe{}

func init() {
	Register(Layout{
		Tool:        ToolKiro,
		DisplayName: "Kiro",
		Sessions: "Kiro 1.0 and CLI 3.0 write one directory per session under ~/.kiro/sessions; " +
			"older IDE builds keep one JSON file per workspace session, legacy .chat file, or execution log in globalStorage.",
		Roots: []Root{
			{Path: "{home}/.kiro/sessions"},
			{Path: "{config}/Kiro/User/globalStorage/kiro.kiroagent"},
			{OS: []OS{OSLinux}, Path: "{home}/.kiro-server/data/User/globalStorage/kiro.kiroagent"},
		},
		Artifacts: []Artifact{
			kiroIDESessionMeta, kiroIDESessionMessages,
			kiroIDEWorkspaceSession, kiroIDEWorkspaceIndex, kiroIDEMigrationMarker,
			kiroIDELegacyChat, kiroIDEExecution, kiroIDEExecutionIndex,
		},
		Sources: []string{
			"https://kiro.dev/docs/reference/cli-commands/",
			"https://kiro.dev/docs/cli/v3/",
			"https://github.com/kirodotdev/Kiro/issues/9472",
			"https://github.com/kirodotdev/Kiro/issues/5469",
			"https://builder.aws.com/content/3HVFGgtEiTeFTUTzmNCWhopvLQL/how-i-recovered-a-lost-kiro-ai-brainstorming-session-and-what-i-learned-about-where-kiro-actually-stores-your-work",
			"https://github.com/getagentseal/codeburn/blob/main/docs/providers/kiro.md",
			"https://github.com/getagentseal/codeburn/blob/main/src/providers/kiro.ts",
			"https://github.com/DevOps-Nirvana/Kiro-Ception/blob/main/src/kiro_ception/ide_loader.py",
			"https://vshulcz.github.io/deja-vu/guide/delete-kiro-session-history.html",
			"https://github.com/aws-samples/sample-kiro-cli-multiagent-development/blob/main/docs/observability.md",
		},
		Probe: kiroIDEProbe{},
	})
}

func (kiroIDEProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	var refs []SessionRef
	var errs []error
	glob := func(pattern string) []string {
		matches, err := fs.Glob(src.FS, pattern)
		if err != nil {
			errs = append(errs, err)
		}
		return matches
	}

	sessionDirs := map[string][]string{}
	for _, pattern := range []string{kiroIDESessionMeta.Pattern, kiroIDESessionMessages.Pattern} {
		for _, match := range glob(pattern) {
			if strings.HasPrefix(match, kiroIDECLIDir+"/") {
				continue
			}
			dir := path.Dir(match)
			sessionDirs[dir] = append(sessionDirs[dir], match)
		}
	}
	for dir, paths := range sessionDirs {
		refs = append(refs, SessionRef{ID: dir, Paths: paths})
	}

	for _, match := range glob(kiroIDEWorkspaceSession.Pattern) {
		name := path.Base(match)
		if name == path.Base(kiroIDEWorkspaceIndex.Pattern) || strings.HasPrefix(name, ".migrated-") {
			continue
		}
		dir := path.Dir(match)
		paths := []string{match}
		for _, sibling := range []string{
			path.Join(dir, path.Base(kiroIDEWorkspaceIndex.Pattern)),
			path.Join(dir, ".migrated-"+strings.TrimSuffix(name, ".json")+".json"),
		} {
			if kiroIsFile(src.FS, sibling) {
				paths = append(paths, sibling)
			}
		}
		refs = append(refs, SessionRef{ID: match, Paths: paths})
	}

	for _, match := range glob(kiroIDELegacyChat.Pattern) {
		if !kiroIDEUnderWorkspace(match) {
			continue
		}
		refs = append(refs, SessionRef{ID: match, Paths: []string{match}})
	}
	for _, pattern := range []string{kiroIDEExecution.Pattern, kiroIDEExecutionIndex.Pattern} {
		for _, match := range glob(pattern) {
			if !kiroIDEUnderWorkspace(match) || !kiroIsHexNames(strings.Split(match, "/")[1:]) || !kiroIsFile(src.FS, match) {
				continue
			}
			refs = append(refs, SessionRef{ID: match, Paths: []string{match}})
		}
	}
	return refs, errors.Join(errs...)
}

func (p kiroIDEProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	if len(ref.Paths) == 0 {
		return Capture{}, fmt.Errorf("kiro session %q has no artifacts", ref.ID)
	}
	first := ref.Paths[0]
	switch {
	case path.Base(first) == path.Base(kiroIDESessionMeta.Pattern) || path.Base(first) == path.Base(kiroIDESessionMessages.Pattern):
		return p.captureSession(src, ref)
	case strings.HasPrefix(first, "workspace-sessions/"):
		return p.captureWorkspaceSession(src, ref)
	case strings.HasSuffix(first, ".chat"):
		return p.captureLegacyChat(src, first)
	case strings.Count(first, "/") == 2:
		return p.captureExecution(src, first)
	default:
		return p.captureExecutionIndex(src, first)
	}
}

func (kiroIDEProbe) captureSession(src Source, ref SessionRef) (Capture, error) {
	meta := Metadata{SessionID: path.Base(ref.ID)}
	var shapes []ArtifactShape
	for _, rel := range ref.Paths {
		switch path.Base(rel) {
		case path.Base(kiroIDESessionMeta.Pattern):
			rec := NewShapeRecorder(kiroIDESessionMeta, rel)
			doc, err := kiroShapeJSONFile(src.FS, rel, rec, nil, KindField("status"))
			if err == nil {
				if id := StringField(doc, "id"); id != "" {
					meta.SessionID = id
				}
				meta.Title = StringField(doc, "title")
				if paths := ArrayField(doc, "workspacePaths"); len(paths) > 0 {
					meta.ProjectPath, _ = paths[0].(string)
				}
				meta.Observe(TimeField(doc, "createdAt"), StringField(doc, "modelId"))
				meta.Observe(TimeField(doc, "lastModifiedAt"), "")
			}
			shapes = append(shapes, rec.Shape())
		case path.Base(kiroIDESessionMessages.Pattern):
			rec := NewShapeRecorder(kiroIDESessionMessages, rel)
			file, err := src.FS.Open(rel)
			if err != nil {
				return Capture{}, err
			}
			err = ShapeJSONL(file, rec, kiroIDEPayloadType, func(record any) {
				meta.Observe(TimeField(record, "timestamp"), "")
				switch StringField(record, "payload", "type") {
				case "user":
					meta.UserTurns++
				case "assistant":
					meta.AssistantMsgs++
				case "tool_call":
					meta.ToolCalls++
				}
			})
			file.Close()
			if err != nil {
				return Capture{}, err
			}
			shapes = append(shapes, rec.Shape())
		}
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

func kiroIDEPayloadType(record any) string {
	kind := StringField(record, "payload", "type")
	if operation := StringField(record, "payload", "operationType"); kind != "" && operation != "" {
		return kind + ":" + operation
	}
	return kind
}

func (kiroIDEProbe) captureWorkspaceSession(src Source, ref SessionRef) (Capture, error) {
	rel := ref.Paths[0]
	meta := Metadata{SessionID: strings.TrimSuffix(path.Base(rel), ".json")}
	rec := NewShapeRecorder(kiroIDEWorkspaceSession, rel)
	doc, err := kiroShapeJSONFile(src.FS, rel, rec, func(document any) []any {
		return ArrayField(document, "history")
	}, func(item any) string { return StringField(item, "message", "role") })
	if err != nil {
		return Capture{}, err
	}
	if id := StringField(doc, "sessionId"); id != "" {
		meta.SessionID = id
	}
	meta.Title = StringField(doc, "title")
	meta.ProjectPath = StringField(doc, "workspaceDirectory")
	if meta.ProjectPath == "" {
		meta.ProjectPath = StringField(doc, "workspacePath")
	}
	if meta.ProjectPath == "" {
		meta.ProjectPath = kiroIDEDecodeWorkspace(path.Base(path.Dir(rel)))
	}
	meta.Observe(time.Time{}, StringField(doc, "selectedModel"))
	for _, item := range ArrayField(doc, "history") {
		switch StringField(item, "message", "role") {
		case "user":
			meta.UserTurns++
		case "assistant":
			meta.AssistantMsgs++
		}
	}
	shapes := []ArtifactShape{rec.Shape()}
	for _, sibling := range ref.Paths[1:] {
		artifact := kiroIDEMigrationMarker
		var each func(any) []any
		if path.Base(sibling) == path.Base(kiroIDEWorkspaceIndex.Pattern) {
			artifact = kiroIDEWorkspaceIndex
			each = kiroIDEIndexEntries
		}
		side := NewShapeRecorder(artifact, sibling)
		if _, err := kiroShapeJSONFile(src.FS, sibling, side, each, nil); err != nil && !errors.Is(err, errKiroMalformed) {
			return Capture{}, err
		}
		shapes = append(shapes, side.Shape())
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

// kiroIDEIndexEntries treats either a top-level array or an object's
// sessions[] array as the index records; the index shape is not published.
func kiroIDEIndexEntries(document any) []any {
	if entries, ok := document.([]any); ok {
		return entries
	}
	return ArrayField(document, "sessions")
}

// kiroIDEDecodeWorkspace reverses the directory name encoding: URL-safe
// base64 whose padding is written as '_' instead of '='.
func kiroIDEDecodeWorkspace(name string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(name, "_"))
	if err != nil {
		return ""
	}
	return string(decoded)
}

func (kiroIDEProbe) captureLegacyChat(src Source, rel string) (Capture, error) {
	meta := Metadata{SessionID: strings.TrimSuffix(path.Base(rel), ".chat")}
	rec := NewShapeRecorder(kiroIDELegacyChat, rel)
	doc, err := kiroShapeJSONFile(src.FS, rel, rec, func(document any) []any {
		return ArrayField(document, "chat")
	}, KindField("role"))
	if err != nil {
		return Capture{}, err
	}
	if id := StringField(doc, "executionId"); id != "" {
		meta.SessionID = id
	}
	meta.Observe(TimeField(doc, "metadata", "startTime"), StringField(doc, "metadata", "modelId"))
	meta.Observe(TimeField(doc, "metadata", "endTime"), "")
	for _, message := range ArrayField(doc, "chat") {
		content := StringField(message, "content")
		switch StringField(message, "role") {
		case "human":
			if !strings.HasPrefix(content, "<identity>") {
				meta.UserTurns++
			}
		case "bot":
			meta.AssistantMsgs++
			meta.ToolCalls += strings.Count(content, "<tool_use>")
		}
	}
	return Capture{Metadata: meta, Shapes: []ArtifactShape{rec.Shape()}}, nil
}

func (kiroIDEProbe) captureExecution(src Source, rel string) (Capture, error) {
	meta := Metadata{}
	rec := NewShapeRecorder(kiroIDEExecution, rel)
	doc, err := kiroShapeJSONFile(src.FS, rel, rec, func(document any) []any {
		return ArrayField(document, "actions")
	}, KindField("actionType"))
	if err != nil {
		return Capture{}, err
	}
	meta.SessionID = StringField(doc, "chatSessionId")
	meta.Observe(TimeField(doc, "startTime"), StringField(doc, "modelId"))
	meta.Observe(TimeField(doc, "endTime"), "")
	if prompt := StringField(doc, "input", "data", "userPrompt"); prompt != "" {
		meta.UserTurns++
	}
	for _, action := range ArrayField(doc, "actions") {
		if meta.SessionID == "" {
			meta.SessionID = StringField(action, "chatSessionId")
		}
		meta.Observe(TimeField(action, "emittedAt"), "")
		switch StringField(action, "actionType") {
		case "":
		case "say":
			meta.AssistantMsgs++
		default:
			meta.ToolCalls++
		}
	}
	return Capture{Metadata: meta, Shapes: []ArtifactShape{rec.Shape()}}, nil
}

func (kiroIDEProbe) captureExecutionIndex(src Source, rel string) (Capture, error) {
	meta := Metadata{}
	rec := NewShapeRecorder(kiroIDEExecutionIndex, rel)
	doc, err := kiroShapeJSONFile(src.FS, rel, rec, func(document any) []any {
		return ArrayField(document, "executions")
	}, KindField("type"))
	if err != nil {
		return Capture{}, err
	}
	for _, execution := range ArrayField(doc, "executions") {
		meta.Observe(TimeField(execution, "startTime"), "")
	}
	return Capture{Metadata: meta, Shapes: []ArtifactShape{rec.Shape()}}, nil
}

var errKiroMalformed = errors.New("malformed JSON")

// kiroShapeJSONFile shapes one JSON file. A document that does not decode is
// counted as malformed on the recorder and reported as errKiroMalformed.
func kiroShapeJSONFile(fsys fs.FS, rel string, rec *ShapeRecorder, each func(any) []any, kind KindFunc) (any, error) {
	file, err := fsys.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	doc, err := ShapeJSON(file, rec, each, kind)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", rel, errKiroMalformed, err)
	}
	return doc, nil
}

func kiroIDEUnderWorkspace(rel string) bool {
	top, _, _ := strings.Cut(rel, "/")
	for _, skip := range kiroIDENotWorkspaces {
		if top == skip {
			return false
		}
	}
	return true
}

func kiroIsHexNames(names []string) bool {
	for _, name := range names {
		if len(name) != 32 {
			return false
		}
		for _, r := range name {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return true
}

func kiroIsFile(fsys fs.FS, rel string) bool {
	info, err := fs.Stat(fsys, rel)
	return err == nil && info.Mode().IsRegular()
}
