package harnesslayout

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const ToolKiroCrew Tool = "kirocrew"

var (
	kiroCrewTranscript = Artifact{
		Name:        "transcript",
		Pattern:     "sessions/*.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "one JSONL file per session key: a _type metadata line, then one row per message",
	}
	kiroCrewArchive = Artifact{
		Name:        "archive",
		Pattern:     "sessions/archive/*__*.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "rows rotated out of a transcript: a _type archive line, then message rows",
	}
	kiroCrewThreads = Artifact{
		Name:        "threads",
		Pattern:     "sessions/.threads/*.json",
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "reply threads on messages of one transcript, keyed by message id",
	}
	kiroCrewSessionMap = Artifact{
		Name:        "session-map",
		Pattern:     "session_map.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "session key to kiro-cli session id, working directory, provider, and channel links",
	}
	kiroCrewCrewLog = Artifact{
		Name:        "crew-log",
		Pattern:     "crew-log/sessions/*/log*.jsonl",
		Format:      FormatJSONL,
		Role:        RoleMetadata,
		Description: "append-only event log per ACP session: a header line, then typed turn, message, tool, and model entries",
	}
	kiroCrewSearchIndex = Artifact{
		Name:        "search-index",
		Pattern:     "sessions/.index/session_index.db",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "FTS5 search index derived from the transcripts; not captured",
	}
)

var kiroCrewToolRoles = map[string]bool{"tool": true, "tool_call": true}

// kiroCrewProbe reads the Kiro Crew data home. A session is one transcript
// stem; crew-log units join it through the header slot, and a unit that
// names no transcript, such as a subagent run, is a session of its own.
type kiroCrewProbe struct{}

var _ Probe = kiroCrewProbe{}

func (kiroCrewProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	sessions := map[string][]string{}
	transcripts, err := fs.Glob(src.FS, kiroCrewTranscript.Pattern)
	if err != nil {
		return nil, err
	}
	for _, match := range transcripts {
		stem := strings.TrimSuffix(path.Base(match), ".jsonl")
		sessions[stem] = append(sessions[stem], match)
	}
	archives, _ := fs.Glob(src.FS, kiroCrewArchive.Pattern)
	for _, match := range archives {
		stem := kiroCrewArchiveStem(match)
		sessions[stem] = append(sessions[stem], match)
	}
	for stem := range sessions {
		sidecar := path.Join("sessions/.threads", stem+".json")
		if _, err := fs.Stat(src.FS, sidecar); err == nil {
			sessions[stem] = append(sessions[stem], sidecar)
		}
	}
	mapStems, mapPresent := kiroCrewMapStems(src.FS)
	for stem := range sessions {
		if mapStems[stem] || (mapPresent && mapStems == nil) {
			sessions[stem] = append(sessions[stem], kiroCrewSessionMap.Pattern)
		}
	}
	for _, unit := range kiroCrewCrewLogUnits(src.FS) {
		owner := ""
		for _, candidate := range kiroCrewSlotStems(unit.slot) {
			if _, ok := sessions[candidate]; ok {
				owner = candidate
				break
			}
		}
		if owner == "" {
			owner = unit.id
		}
		sessions[owner] = append(sessions[owner], unit.segments...)
	}

	refs := make([]SessionRef, 0, len(sessions))
	for id, paths := range sessions {
		sort.Slice(paths, func(i, j int) bool { return kiroCrewPathLess(paths[i], paths[j]) })
		refs = append(refs, SessionRef{ID: id, Paths: paths})
	}
	return refs, nil
}

func (kiroCrewProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	var (
		shapes     []ArtifactShape
		transcript kiroCrewCounts
		crew       kiroCrewCounts
		sawRows    bool
		sawCrewLog bool
		mapCwd     string
		crewCwd    string
	)
	meta := Metadata{SessionID: ref.ID}
	for _, p := range ref.Paths {
		artifact, ok := kiroCrewArtifactFor(p)
		if !ok {
			continue
		}
		rec := NewShapeRecorder(artifact, p)
		var err error
		switch artifact.Name {
		case kiroCrewTranscript.Name, kiroCrewArchive.Name:
			sawRows = true
			err = shapeFile(src.FS, p, func(r io.Reader) error {
				return ShapeJSONL(r, rec, kiroCrewTranscriptKind, func(record any) {
					kiroCrewVisitTranscript(record, &meta, &transcript)
				})
			})
		case kiroCrewThreads.Name:
			err = shapeFile(src.FS, p, func(r io.Reader) error { return kiroCrewShapeThreads(r, rec) })
		case kiroCrewSessionMap.Name:
			err = shapeFile(src.FS, p, func(r io.Reader) error {
				cwd, err := kiroCrewShapeMapEntries(r, rec, ref.ID)
				mapCwd = cwd
				return err
			})
		case kiroCrewCrewLog.Name:
			sawCrewLog = true
			err = shapeFile(src.FS, p, func(r io.Reader) error {
				return ShapeJSONL(r, rec, KindField("type"), func(record any) {
					if cwd := kiroCrewVisitCrewLog(record, &meta, &crew); cwd != "" {
						crewCwd = cwd
					}
				})
			})
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Capture{}, fmt.Errorf("%s: %w", p, err)
		}
		shapes = append(shapes, rec.Shape())
	}

	if sawRows {
		meta.UserTurns, meta.AssistantMsgs = transcript.user, transcript.assistant
	} else {
		meta.UserTurns, meta.AssistantMsgs = crew.user, crew.assistant
	}
	meta.ToolCalls = transcript.tools
	if sawCrewLog {
		meta.ToolCalls = crew.tools
	}
	for _, cwd := range []string{mapCwd, crewCwd} {
		if meta.ProjectPath == "" {
			meta.ProjectPath = cwd
		}
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

type kiroCrewCounts struct {
	user, assistant, tools int
}

func shapeFile(fsys fs.FS, name string, shape func(io.Reader) error) error {
	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	return shape(file)
}

func kiroCrewArtifactFor(p string) (Artifact, bool) {
	for _, artifact := range []Artifact{kiroCrewTranscript, kiroCrewArchive, kiroCrewThreads, kiroCrewSessionMap, kiroCrewCrewLog} {
		if ok, _ := path.Match(artifact.Pattern, p); ok {
			return artifact, true
		}
	}
	return Artifact{}, false
}

func kiroCrewTranscriptKind(record any) string {
	if kind := StringField(record, "_type"); kind != "" {
		return kind
	}
	return StringField(record, "role")
}

func kiroCrewVisitTranscript(record any, meta *Metadata, counts *kiroCrewCounts) {
	switch StringField(record, "_type") {
	case "metadata":
		if title := StringField(record, "title"); title != "" {
			meta.Title = title
		}
		if project := StringField(record, "project"); project != "" {
			meta.ProjectPath = project
		}
		meta.Observe(TimeField(record, "created_at"), StringField(record, "model"))
		return
	case "":
	default:
		return
	}
	meta.Observe(TimeField(record, "ts"), "")
	role := StringField(record, "role")
	switch role {
	case "user":
		counts.user++
	case "assistant":
		counts.assistant++
	}
	counts.tools += len(ArrayField(record, "tools"))
	if kiroCrewToolRoles[role] {
		counts.tools++
	}
}

// kiroCrewVisitCrewLog folds one crew-log line and returns the working
// directory it names, if any.
func kiroCrewVisitCrewLog(record any, meta *Metadata, counts *kiroCrewCounts) string {
	if Field(record, "seq") == nil {
		meta.Observe(TimeField(record, "createdAt"), "")
		return StringField(record, "cwd")
	}
	meta.Observe(TimeField(record, "time"), StringField(record, "data", "model"))
	switch StringField(record, "type") {
	case "session/opened":
		return StringField(record, "data", "cwd")
	case "message/received":
		if StringField(record, "data", "role") == "user" {
			counts.user++
		}
	case "message/sent":
		counts.assistant++
	case "tool/called":
		counts.tools++
	}
	return ""
}

// kiroCrewShapeThreads records a sidecar with its message-id keys folded to
// one placeholder, so identifiers never become field paths, and counts each
// reply as a record of its role.
func kiroCrewShapeThreads(r io.Reader, rec *ShapeRecorder) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	document, err := decodeValue(raw)
	if err != nil {
		rec.shape.Malformed++
		return nil
	}
	var replies []any
	if object, ok := document.(map[string]any); ok {
		if threads, ok := object["threads"].(map[string]any); ok {
			var folded []any
			for _, thread := range threads {
				rows, _ := thread.([]any)
				replies = append(replies, rows...)
				folded = append(folded, rows...)
			}
			normalized := make(map[string]any, len(object))
			for k, v := range object {
				normalized[k] = v
			}
			normalized["threads"] = map[string]any{"{mid}": folded}
			document = normalized
		}
	}
	rec.walk("$", document)
	for _, reply := range replies {
		rec.shape.Records++
		if role := StringField(reply, "role"); role != "" {
			rec.shape.Kinds[role]++
		}
	}
	return nil
}

// kiroCrewShapeMapEntries records only the session map entries whose key
// names this session's stem and returns the working directory they carry.
func kiroCrewShapeMapEntries(r io.Reader, rec *ShapeRecorder, stem string) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	document, err := decodeValue(raw)
	if err != nil {
		rec.shape.Malformed++
		return "", nil
	}
	entries, _ := document.(map[string]any)
	keys := make([]string, 0, len(entries))
	for key := range entries {
		if kiroCrewSafeKey(key) == stem {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	cwd := ""
	for _, key := range keys {
		entry := entries[key]
		kind := "legacy"
		if _, ok := entry.(map[string]any); ok {
			kind = StringField(entry, "provider")
			if kind == "" {
				kind = "acp"
			}
		}
		rec.AddRecord(kind, entry)
		if c := StringField(entry, "cwd"); c != "" && cwd == "" {
			cwd = c
		}
	}
	return cwd, nil
}

// kiroCrewMapStems returns the stems the session map names and whether the
// map exists. A map that exists but does not decode returns nil stems, so
// every session carries it and the capture reports it as malformed.
func kiroCrewMapStems(fsys fs.FS) (map[string]bool, bool) {
	raw, err := fs.ReadFile(fsys, kiroCrewSessionMap.Pattern)
	if err != nil {
		return map[string]bool{}, false
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, true
	}
	stems := make(map[string]bool, len(entries))
	for key := range entries {
		stems[kiroCrewSafeKey(key)] = true
	}
	return stems, true
}

type kiroCrewUnit struct {
	id, slot string
	segments []string
}

// kiroCrewCrewLogUnits groups crew-log segments by unit directory and reads
// the unit id and slot from the first segment whose header decodes. A unit
// with no readable header cannot be attributed and is skipped.
func kiroCrewCrewLogUnits(fsys fs.FS) []kiroCrewUnit {
	matches, _ := fs.Glob(fsys, kiroCrewCrewLog.Pattern)
	byDir := map[string][]string{}
	for _, match := range matches {
		byDir[path.Dir(match)] = append(byDir[path.Dir(match)], match)
	}
	var units []kiroCrewUnit
	for _, segments := range byDir {
		sort.Slice(segments, func(i, j int) bool { return kiroCrewPathLess(segments[i], segments[j]) })
		for _, segment := range segments {
			header, ok := kiroCrewFirstLine(fsys, segment)
			if !ok || StringField(header, "type") != "session" {
				continue
			}
			id := StringField(header, "id")
			if id == "" {
				continue
			}
			units = append(units, kiroCrewUnit{id: id, slot: StringField(header, "slot"), segments: segments})
			break
		}
	}
	return units
}

// kiroCrewPathLess orders crew-log segments of one unit by their first seq:
// log.jsonl holds seq 1 and log.<first seq>.jsonl each later segment.
func kiroCrewPathLess(a, b string) bool {
	if path.Dir(a) == path.Dir(b) {
		sa, oka := kiroCrewSegmentSeq(a)
		sb, okb := kiroCrewSegmentSeq(b)
		if oka && okb {
			return sa < sb
		}
	}
	return a < b
}

func kiroCrewSegmentSeq(p string) (int, bool) {
	if ok, _ := path.Match(kiroCrewCrewLog.Pattern, p); !ok {
		return 0, false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(path.Base(p), "log"), ".jsonl")
	if middle == "" {
		return 1, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(middle, "."))
	return n, err == nil && strings.HasPrefix(middle, ".")
}

func kiroCrewFirstLine(fsys fs.FS, name string) (any, bool) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 4<<10), 1<<20)
	if !scanner.Scan() {
		return nil, false
	}
	value, err := decodeValue(bytes.TrimSpace(scanner.Bytes()))
	return value, err == nil
}

// kiroCrewSlotStems lists the transcript stems a crew-log header slot can
// name: a channel slot is its own stem, a dashboard slot is dashboard_<slot>.
func kiroCrewSlotStems(slot string) []string {
	if slot == "" {
		return nil
	}
	safe := kiroCrewSafeKey(slot)
	return []string{slot, safe, "dashboard_" + slot, "dashboard_" + safe}
}

// kiroCrewArchiveStem strips the archive timestamp. Session keys may hold
// dots, so the tool splits on the last "__" rather than on a dot.
func kiroCrewArchiveStem(p string) string {
	name := strings.TrimSuffix(path.Base(p), ".jsonl")
	if i := strings.LastIndex(name, "__"); i > 0 {
		return name[:i]
	}
	return name
}

// kiroCrewSafeKey mirrors history._safe_key: every character outside word
// characters, "-", and "." becomes "_".
func kiroCrewSafeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, key)
}

func init() {
	Register(Layout{
		Tool:        ToolKiroCrew,
		DisplayName: "Kiro Crew",
		Sessions:    "One JSONL transcript per session key under sessions/, joined by crew-log units whose header slot names it.",
		Roots: []Root{
			{Path: "{home}/.kiro/crew"},
			{Path: "{home}/.kirocrew"},
		},
		Artifacts: []Artifact{kiroCrewTranscript, kiroCrewArchive, kiroCrewThreads, kiroCrewSessionMap, kiroCrewCrewLog, kiroCrewSearchIndex},
		Sources: []string{
			"https://github.com/kirodotdev/KiroCrew",
			"https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/config/paths.py",
			"https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/history.py",
			"https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/session_storage.py",
			"https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/session_map.py",
			"https://github.com/kirodotdev/KiroCrew/blob/main/docs/architecture/overview.md#data-home",
			"https://github.com/kirodotdev/KiroCrew/blob/main/docs/reference/crew-log/envelope.md",
			"https://github.com/kirodotdev/KiroCrew/blob/main/docs/reference/crew-log/session-types.md",
			"https://kiro.dev/docs/crew/",
		},
		Probe: kiroCrewProbe{},
	})
}
