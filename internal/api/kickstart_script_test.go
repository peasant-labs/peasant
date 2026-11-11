package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/schema"
)

// The kickstart fixtures are scripts. Each case names a world (a configuration
// file, a sign-in, what the scan finds) and a list of steps. A step is an HTTP
// request against the real mounted server, a wait for a published event, or a
// check of the events published so far. The server, the session store, the
// setup model, the forest, and the hub are the production ones. Only the three
// things a test cannot own are replaced: what the filesystem scan finds, the
// sign-in on this computer, and the idle timer.

//go:embed testdata/kickstart_listings.yaml
var kickstartListingsYAML []byte

// kickstartListingSets are the named sets of discovered sessions a scan finds.
type kickstartListingSets struct {
	Sets map[string][]onboarding.SessionListing `yaml:"sets"`
}

// kickstartScript is one fixture file.
type kickstartScript struct {
	RequiredNames []string        `yaml:"requiredNames"`
	Cases         []kickstartCase `yaml:"cases"`
}

type kickstartCase struct {
	Name  string          `yaml:"name"`
	World kickstartSpec   `yaml:"world"`
	Steps []kickstartStep `yaml:"steps"`
}

// kickstartSpec is the world a case runs in.
type kickstartSpec struct {
	// Config is the configuration file, absent when empty.
	Config string `yaml:"config"`
	// Village is "connected" or empty.
	Village string `yaml:"village"`
	// Listings names the set the scan finds.
	Listings string `yaml:"listings"`
	// Gate holds every scan until a release step lets it run.
	Gate bool `yaml:"gate"`
	// Transcripts are Claude transcript lines by session id, written under the
	// case's directory and pointed at by the listing of that session. A line
	// "@repeat N text" is text, N times, with {n} replaced by the count.
	Transcripts map[string][]string `yaml:"transcripts"`
	// ScanPush is the shortest interval between two pushed progress ticks. Empty
	// pushes every tick.
	ScanPush string `yaml:"scanPush"`
	// StoredError makes reading the local store fail with this text.
	StoredError string `yaml:"storedError"`
	// Recordings are Claude session ids whose recordings the import reads. They
	// are written under {recordings}, which a configuration may name.
	Recordings []string `yaml:"recordings"`
	// HoldIngest holds each import until a releaseIngest step.
	HoldIngest bool `yaml:"holdIngest"`
}

// kickstartStep is one thing a case does or checks. Exactly one of Call,
// Await, Events, Release, SetListings, SetVillage, Expire, WS, or WSEvents is
// set.
type kickstartStep struct {
	// Call is "METHOD /path"; {name} is replaced by a saved value.
	Call    string `yaml:"call"`
	Foreign bool   `yaml:"foreign"`
	Body    any    `yaml:"body"`
	// Save maps a name to the dotted path in the response it keeps.
	Save     map[string]string `yaml:"save"`
	Status   int               `yaml:"status"`
	JSON     map[string]any    `yaml:"json"`
	Absent   []string          `yaml:"absent"`
	Len      map[string]int    `yaml:"len"`
	Contains map[string]string `yaml:"contains"`
	// Same maps a dotted path to the saved name whose value it must equal.
	Same map[string]string `yaml:"same"`
	// NoReset requires that no reset control appears anywhere in the body.
	NoReset bool `yaml:"noReset"`
	// Keys requires the top-level keys of the body to be exactly these.
	Keys []string `yaml:"keys"`
	// LenMax requires an array to be no longer than this.
	LenMax map[string]int `yaml:"lenMax"`

	// Await waits for a published event to match, then moves the cursor past it.
	Await *kickstartEventSpec `yaml:"await"`
	// Events requires exactly these events since the cursor, in order.
	Events []kickstartEventSpec `yaml:"events"`
	// Release lets one held scan run.
	Release bool `yaml:"release"`
	// SetListings changes what the next scan finds.
	SetListings string `yaml:"setListings"`
	// SetVillage changes the sign-in: "connected" or "disconnected".
	SetVillage string `yaml:"setVillage"`
	// Expire fires the idle timer of the session saved under this name.
	Expire string `yaml:"expire"`
	// WS connects, subscribes, or reads a websocket client.
	WS       *kickstartWSStep `yaml:"ws"`
	WSEvents *kickstartWSRead `yaml:"wsEvents"`
	// Idle checks the idle timer scheduling.
	Idle string `yaml:"idle"`
	// SetStoredError changes the local-store failure the next scan meets.
	SetStoredError *string `yaml:"setStoredError"`
	// Commit drives the seam a commit uses.
	Commit *kickstartCommitStep `yaml:"commit"`
	// Conflicts publishes conflict rows the way a commit does.
	Conflicts *kickstartConflictsStep `yaml:"conflicts"`
	// ConfigFile checks the configuration file on disk: "unchanged" or "absent".
	ConfigFile string `yaml:"configFile"`

	// The steps below belong to the commit fixtures (kickstart_commit_test.go).
	// ReleaseIngest lets one held import run.
	ReleaseIngest bool `yaml:"releaseIngest"`
	// Ingest checks the imports the server ran.
	Ingest *kickstartIngestCheck `yaml:"ingest"`
	// Retention checks the Claude retention writes.
	Retention *kickstartRetentionCheck `yaml:"retention"`
	// SettingsSection requires the settings page to have this section.
	SettingsSection string `yaml:"settingsSection"`
	// ConfigHas requires the configuration file to contain each substring.
	ConfigHas []string `yaml:"configHas"`
	// WriteConfig replaces the configuration file the way another program would.
	WriteConfig *string `yaml:"writeConfig"`
	// EditConfig changes one line of the configuration file the way another
	// program would.
	EditConfig *kickstartEditConfig `yaml:"editConfig"`
}

type kickstartCommitStep struct {
	// Session is the saved name of the session id.
	Session string `yaml:"session"`
	// Action is begin, abort, or close.
	Action string `yaml:"action"`
	// Error is a substring the action's refusal must contain.
	Error string `yaml:"error"`
	// Answered and Resolutions are what begin must have read.
	Answered    []string          `yaml:"answered"`
	Resolutions map[string]string `yaml:"resolutions"`
	// Seen is the file value a resolution must carry as the one the user saw.
	Seen map[string]any `yaml:"seen"`
	// Ingest publishes an ingest state after a close.
	Ingest string `yaml:"ingest"`
}

type kickstartConflictsStep struct {
	Session string              `yaml:"session"`
	Rows    []kickstartConflict `yaml:"rows"`
}

type kickstartConflict struct {
	Field  string `yaml:"field"`
	Stop   string `yaml:"stop"`
	Label  string `yaml:"label"`
	Mine   any    `yaml:"mine"`
	Theirs any    `yaml:"theirs"`
}

type kickstartEventSpec struct {
	Kind     string         `yaml:"kind"`
	Session  string         `yaml:"session"`
	Revision *uint64        `yaml:"revision"`
	JSON     map[string]any `yaml:"json"`
	Absent   []string       `yaml:"absent"`
	Len      map[string]int `yaml:"len"`
}

type kickstartWSStep struct {
	Client    string `yaml:"client"`
	Subscribe bool   `yaml:"subscribe"`
}

type kickstartWSRead struct {
	Client string `yaml:"client"`
	// Events are the kickstart events the client receives next, in order.
	Events []kickstartEventSpec `yaml:"events"`
}

func decodeKickstartScript(t *testing.T, data []byte) kickstartScript {
	t.Helper()
	var script kickstartScript
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&script); err != nil {
		t.Fatalf("decode the kickstart fixture: %v", err)
	}
	present := make(map[string]struct{}, len(script.Cases))
	for _, c := range script.Cases {
		if _, dup := present[c.Name]; dup {
			t.Fatalf("kickstart fixture repeats case %q", c.Name)
		}
		present[c.Name] = struct{}{}
	}
	required := make(map[string]struct{}, len(script.RequiredNames))
	for _, name := range script.RequiredNames {
		if _, dup := required[name]; dup {
			t.Fatalf("kickstart fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
		if _, ok := present[name]; !ok {
			t.Fatalf("kickstart fixture is missing required case %q", name)
		}
	}
	for name := range present {
		if _, ok := required[name]; !ok {
			t.Fatalf("kickstart fixture has case %q that no required name lists", name)
		}
	}
	return script
}

func runKickstartScript(t *testing.T, data []byte) {
	t.Helper()
	script := decodeKickstartScript(t, data)
	var sets kickstartListingSets
	if err := yaml.Unmarshal(kickstartListingsYAML, &sets); err != nil {
		t.Fatalf("decode the kickstart listing sets: %v", err)
	}
	for _, c := range script.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			world := newKickstartWorld(t, c, sets)
			for index, step := range c.Steps {
				world.run(index, step)
			}
		})
	}
}

// kickstartLog records every event the session store publishes, in order.
type kickstartLog struct {
	mu     sync.Mutex
	events []schema.LocalKickstartEvent
	wake   chan struct{}
}

func newKickstartLog() *kickstartLog { return &kickstartLog{wake: make(chan struct{})} }

func (l *kickstartLog) add(event schema.LocalKickstartEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	close(l.wake)
	l.wake = make(chan struct{})
}

func (l *kickstartLog) from(index int) ([]schema.LocalKickstartEvent, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if index > len(l.events) {
		index = len(l.events)
	}
	return append([]schema.LocalKickstartEvent(nil), l.events[index:]...), l.wake
}

// kickstartWorld is one running case.
type kickstartWorld struct {
	t       *testing.T
	name    string
	baseURL string
	server  *Server
	dir     string
	sets    kickstartListingSets
	log     *kickstartLog
	cursor  int
	vars    map[string]any
	clients map[string]*kickstartWSClient

	gate        chan struct{}
	scanPush    time.Duration
	storedError atomic.Pointer[string]
	configPath  string
	configBytes string
	connected   atomic.Bool
	listings    atomic.Pointer[[]onboarding.SessionListing]
	sequence    atomic.Int64
	idle        []time.Duration
	idleMu      sync.Mutex
	saving      kickstartCommitWorld
}

// identityPaths resolves a working directory to itself. The fixture directories
// do not exist, and the physical resolver would refuse them.
type identityPaths struct{}

func (identityPaths) Resolve(dir string) (ingest.ClonePath, error) { return ingest.ClonePath(dir), nil }

func newKickstartWorld(t *testing.T, c kickstartCase, sets kickstartListingSets) *kickstartWorld {
	t.Helper()
	w := &kickstartWorld{
		t: t, name: c.Name, sets: sets, log: newKickstartLog(),
		vars: map[string]any{}, clients: map[string]*kickstartWSClient{},
		dir: t.TempDir(),
	}
	if c.World.Gate {
		w.gate = make(chan struct{}, 16)
	}
	if c.World.ScanPush != "" {
		pushEvery, err := time.ParseDuration(c.World.ScanPush)
		if err != nil {
			t.Fatalf("scanPush %q: %v", c.World.ScanPush, err)
		}
		w.scanPush = pushEvery
	}
	w.connected.Store(c.World.Village == "connected")
	w.setListings(c.World.Listings)
	stored := c.World.StoredError
	w.storedError.Store(&stored)
	for session, lines := range c.World.Transcripts {
		path := filepath.Join(w.dir, session+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(expandTranscript(lines), "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write transcript for %s: %v", session, err)
		}
	}

	homes := newTestXDGHomes(t)
	configPath := defaults.ResolveConfigFilePathWith(homes.Config).String()
	w.saving.prepare(t, w.dir, homes, c.World)
	c.World.Config = w.saving.fill(c.World.Config)
	w.configPath, w.configBytes = configPath, c.World.Config
	if c.World.Config != "" {
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(c.World.Config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hub := NewHub(&mockDataProvider{})
	server := NewServer(homes.config(ServerConfig{Hub: hub, ConfigPath: configPath, Config: w.saving.serverConfig()}))
	server.kickstartTune = w.tune
	server.syncTune = w.saving.tuneSync
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		for _, client := range w.clients {
			client.close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop")
		}
	})
	w.server = server
	w.baseURL = "http://" + server.Addr().String()
	return w
}

// tune replaces the three members a test cannot own and records every event.
func (w *kickstartWorld) tune(env *kickstartEnv) {
	env.newID = func() string { return fmt.Sprintf("ses-%d", w.sequence.Add(1)) }
	env.scanPush = time.Nanosecond
	if w.scanPush > 0 {
		env.scanPush = w.scanPush
	}
	env.paths = identityPaths{}
	env.repositories = nil
	env.retentionSeed = func() int { return 90 }
	env.village = func() kickstartFacts {
		if w.connected.Load() {
			return kickstartFacts{villageConnected: true, villageURL: "https://village.example.test", villageUserID: "user-1"}
		}
		return kickstartFacts{}
	}
	env.scan = w.scan
	env.writeRetention = w.saving.writeRetention(w.configPath)
	env.stored = func(context.Context) ([]store.IngestedSessionRow, error) {
		if message := *w.storedError.Load(); message != "" {
			return nil, errors.New(message)
		}
		return nil, nil
	}
	env.afterFunc = func(d time.Duration, _ func()) func() bool {
		w.idleMu.Lock()
		w.idle = append(w.idle, d)
		w.idleMu.Unlock()
		return func() bool { return true }
	}
	inner := env.emit
	env.emit = func(event schema.LocalKickstartEvent) {
		w.log.add(event)
		if inner != nil {
			inner(event)
		}
	}
}

func (w *kickstartWorld) setListings(name string) {
	if name == "" {
		empty := []onboarding.SessionListing{}
		w.listings.Store(&empty)
		return
	}
	set, ok := w.sets.Sets[name]
	if !ok {
		w.t.Fatalf("the fixture names listing set %q, which kickstart_listings.yaml does not define", name)
	}
	resolved := make([]onboarding.SessionListing, len(set))
	for index, listing := range set {
		if strings.Contains(listing.Source.Path, "{dir}") {
			listing.Source.Path = strings.ReplaceAll(listing.Source.Path, "{dir}", w.dir)
		}
		resolved[index] = listing
	}
	w.listings.Store(&resolved)
}

// scan is what the filesystem scan finds. It reports its phase and one count per
// session, as the real scan does.
func (w *kickstartWorld) scan(ctx context.Context, _ *config.Config, _ discovery.KnownSessionIndex, progress discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
	if w.gate != nil {
		select {
		case <-w.gate:
		case <-ctx.Done():
			return nil, nil, nil
		}
	}
	listings := *w.listings.Load()
	progress.SetPhase("Discovering Claude Code sessions...")
	for index := range listings {
		progress.SetProgress(index+1, len(listings))
	}
	inventory := discovery.ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: len(listings), Enabled: true}}
	relation := discovery.SubagentRelation{}
	for _, listing := range listings {
		relation[listing.SessionID] = listing.SubagentIDs
		for _, child := range listing.SubagentIDs {
			if _, ok := relation[child]; !ok {
				relation[child] = nil
			}
		}
	}
	return inventory, listings, relation
}

// expandTranscript replaces each "@repeat N text" line with N copies of text.
func expandTranscript(lines []string) []string {
	var out []string
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, "@repeat ")
		if !ok {
			out = append(out, line)
			continue
		}
		count, text, _ := strings.Cut(rest, " ")
		n, err := strconv.Atoi(count)
		if err != nil {
			panic(fmt.Sprintf("kickstart fixture: %q is not \"@repeat N text\"", line))
		}
		for i := 1; i <= n; i++ {
			out = append(out, strings.ReplaceAll(text, "{n}", strconv.Itoa(i)))
		}
	}
	return out
}

func (w *kickstartWorld) fail(index int, format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("case %q step %d: %s", w.name, index+1, fmt.Sprintf(format, args...))
}

func (w *kickstartWorld) run(index int, step kickstartStep) {
	w.t.Helper()
	switch {
	case step.Call != "":
		w.call(index, step)
	case step.Await != nil:
		w.await(index, *step.Await)
	case step.Events != nil:
		w.events(index, step.Events)
	case step.Release:
		w.gate <- struct{}{}
	case step.SetListings != "":
		w.setListings(step.SetListings)
	case step.SetVillage != "":
		w.connected.Store(step.SetVillage == "connected")
	case step.Expire != "":
		w.expire(index, step.Expire)
	case step.WS != nil:
		w.wsStep(index, *step.WS)
	case step.WSEvents != nil:
		w.wsRead(index, *step.WSEvents)
	case step.Idle != "":
		w.idleCheck(index, step.Idle)
	case step.SetStoredError != nil:
		w.storedError.Store(step.SetStoredError)
	case step.Commit != nil:
		w.commit(index, *step.Commit)
	case step.Conflicts != nil:
		w.conflicts(index, *step.Conflicts)
	case step.ConfigFile != "":
		w.configFile(index, step.ConfigFile)
	case step.ReleaseIngest:
		w.releaseIngest()
	case step.Ingest != nil:
		w.ingestCheck(index, *step.Ingest)
	case step.Retention != nil:
		w.retentionCheck(index, *step.Retention)
	case step.SettingsSection != "":
		w.settingsSectionCheck(index, step.SettingsSection)
	case step.ConfigHas != nil:
		w.configHas(index, step.ConfigHas)
	case step.WriteConfig != nil:
		w.writeConfig(index, *step.WriteConfig)
	case step.EditConfig != nil:
		w.editConfig(index, *step.EditConfig)
	default:
		w.fail(index, "the step does nothing")
	}
}

func (w *kickstartWorld) substitute(value any) any {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "{") && strings.HasSuffix(typed, "}") && strings.Count(typed, "{") == 1 {
			if saved, ok := w.vars[typed[1:len(typed)-1]]; ok {
				return saved
			}
		}
		for name, saved := range w.vars {
			typed = strings.ReplaceAll(typed, "{"+name+"}", fmt.Sprint(saved))
		}
		return typed
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = w.substitute(inner)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = w.substitute(inner)
		}
		return out
	default:
		return value
	}
}

func (w *kickstartWorld) call(index int, step kickstartStep) {
	method, path, ok := strings.Cut(w.substitute(step.Call).(string), " ")
	if !ok {
		w.fail(index, "call %q is not \"METHOD /path\"", step.Call)
	}
	var body io.Reader
	if step.Body != nil {
		data, err := json.Marshal(w.substitute(step.Body))
		if err != nil {
			w.fail(index, "encode the body: %v", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, w.baseURL+path, body)
	if err != nil {
		w.fail(index, "build the request: %v", err)
	}
	if body != nil {
		request.Header.Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	}
	if method != http.MethodGet {
		if step.Foreign {
			request.Header.Set(defaults.HeaderOrigin, "http://evil.example")
		} else {
			request.Header.Set(defaults.HeaderOrigin, w.baseURL)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		w.fail(index, "%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	want := step.Status
	if want == 0 {
		want = http.StatusOK
	}
	if response.StatusCode != want {
		w.fail(index, "%s %s: status %d, want %d; body: %s", method, path, response.StatusCode, want, raw)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		w.fail(index, "%s %s: the body is not JSON: %v; body: %s", method, path, err, raw)
	}
	// The configuration path is the case's directory, which carries the case's
	// name; it is not part of what the payload says.
	if step.NoReset && strings.Contains(strings.ToLower(strings.ReplaceAll(string(raw), w.configPath, "<config>")), "reset") {
		w.fail(index, "%s %s: the payload carries a reset control: %s", method, path, raw)
	}
	w.check(index, decoded, step.JSON, step.Absent, step.Len)
	if step.Keys != nil {
		object, _ := decoded.(map[string]any)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		want := append([]string(nil), step.Keys...)
		sort.Strings(want)
		if !reflect.DeepEqual(keys, want) {
			w.fail(index, "%s %s: top-level keys %v, want exactly %v", method, path, keys, want)
		}
	}
	for path, limit := range step.LenMax {
		got, found := jsonPath(decoded, path)
		list, _ := got.([]any)
		if !found || len(list) > limit {
			w.fail(index, "len(%s) = %d (found %t), want at most %d", path, len(list), found, limit)
		}
	}
	for path, substring := range step.Contains {
		got, found := jsonPath(decoded, path)
		text, _ := got.(string)
		if !found || !strings.Contains(text, substring) {
			w.fail(index, "%s should contain %q; got %v", path, substring, got)
		}
	}
	for path, name := range step.Same {
		got, found := jsonPath(decoded, path)
		saved, ok := w.vars[name]
		if !found || !ok || !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(saved)) {
			w.fail(index, "%s = %v, want the saved %s = %v", path, got, name, saved)
		}
	}
	for name, path := range step.Save {
		got, found := jsonPath(decoded, path)
		if !found {
			w.fail(index, "cannot save %s: %s is absent from %s", name, path, raw)
		}
		w.vars[name] = got
	}
}

func (w *kickstartWorld) check(index int, decoded any, want map[string]any, absent []string, lengths map[string]int) {
	w.t.Helper()
	paths := make([]string, 0, len(want))
	for path := range want {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		got, found := jsonPath(decoded, path)
		expected := normalizeJSON(w.substitute(want[path]))
		if !found || !reflect.DeepEqual(normalizeJSON(got), expected) {
			w.fail(index, "%s = %v (found %t), want %v", path, got, found, expected)
		}
	}
	for _, path := range absent {
		if got, found := jsonPath(decoded, path); found {
			w.fail(index, "%s should be absent; got %v", path, got)
		}
	}
	for path, length := range lengths {
		got, found := jsonPath(decoded, path)
		list, _ := got.([]any)
		if !found || len(list) != length {
			w.fail(index, "len(%s) = %d (found %t), want %d", path, len(list), found, length)
		}
	}
}

// normalizeJSON puts a decoded YAML or JSON value in one comparable shape.
func normalizeJSON(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return value
	}
	return out
}

// jsonPath reads a dotted path. A segment is a key, an array index, or
// key[field=value], which selects the array element whose field equals value.
func jsonPath(root any, path string) (any, bool) {
	current := root
	for _, segment := range strings.Split(path, ".") {
		name, selector, hasSelector := strings.Cut(segment, "[")
		if name != "" {
			switch typed := current.(type) {
			case map[string]any:
				next, ok := typed[name]
				if !ok {
					return nil, false
				}
				current = next
			case []any:
				position, err := strconv.Atoi(name)
				if err != nil || position < 0 || position >= len(typed) {
					return nil, false
				}
				current = typed[position]
			default:
				return nil, false
			}
		}
		if hasSelector {
			field, value, ok := strings.Cut(strings.TrimSuffix(selector, "]"), "=")
			list, isList := current.([]any)
			if !ok || !isList {
				return nil, false
			}
			found := false
			for _, element := range list {
				if object, isObject := element.(map[string]any); isObject && fmt.Sprint(object[field]) == value {
					current, found = element, true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
	}
	return current, true
}

func (w *kickstartWorld) eventMatches(event schema.LocalKickstartEvent, spec kickstartEventSpec) (bool, string) {
	data, err := json.Marshal(event)
	if err != nil {
		return false, err.Error()
	}
	var decoded any
	_ = json.Unmarshal(data, &decoded)
	if spec.Kind != "" && string(event.Kind) != spec.Kind {
		return false, fmt.Sprintf("kind %s, want %s", event.Kind, spec.Kind)
	}
	if spec.Session != "" {
		if want := fmt.Sprint(w.substitute(spec.Session)); event.SessionID != want {
			return false, fmt.Sprintf("session %s, want %s", event.SessionID, want)
		}
	}
	if spec.Revision != nil && event.Revision != *spec.Revision {
		return false, fmt.Sprintf("revision %d, want %d", event.Revision, *spec.Revision)
	}
	for path, want := range spec.JSON {
		got, found := jsonPath(decoded, path)
		if !found || !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(w.substitute(want))) {
			return false, fmt.Sprintf("%s = %v (found %t), want %v", path, got, found, want)
		}
	}
	for _, path := range spec.Absent {
		if got, found := jsonPath(decoded, path); found {
			return false, fmt.Sprintf("%s should be absent; got %v", path, got)
		}
	}
	for path, length := range spec.Len {
		got, found := jsonPath(decoded, path)
		list, _ := got.([]any)
		if !found || len(list) != length {
			return false, fmt.Sprintf("len(%s) = %d, want %d", path, len(list), length)
		}
	}
	return true, ""
}

// await waits for a published event to match, then moves the cursor past it.
func (w *kickstartWorld) await(index int, spec kickstartEventSpec) {
	w.t.Helper()
	ctx := testwait.Context(w.t)
	position := w.cursor
	for {
		events, wake := w.log.from(position)
		for offset, event := range events {
			if ok, _ := w.eventMatches(event, spec); ok {
				w.cursor = position + offset + 1
				return
			}
		}
		position += len(events)
		select {
		case <-wake:
		case <-ctx.Done():
			w.fail(index, "no published event matched %+v before the deadline; saw %d events since the cursor", spec, position-w.cursor)
		}
	}
}

// events requires exactly these events since the cursor.
func (w *kickstartWorld) events(index int, specs []kickstartEventSpec) {
	w.t.Helper()
	ctx := testwait.Context(w.t)
	for {
		events, wake := w.log.from(w.cursor)
		if len(events) >= len(specs) {
			if len(events) > len(specs) {
				w.fail(index, "%d events were published since the last check, want %d: %s", len(events), len(specs), describeEvents(events))
			}
			for i, spec := range specs {
				if ok, why := w.eventMatches(events[i], spec); !ok {
					w.fail(index, "event %d (%s): %s", i+1, events[i].Kind, why)
				}
			}
			w.cursor += len(specs)
			return
		}
		select {
		case <-wake:
		case <-ctx.Done():
			w.fail(index, "only %d of %d events were published: %s", len(events), len(specs), describeEvents(events))
		}
	}
}

func describeEvents(events []schema.LocalKickstartEvent) string {
	parts := make([]string, len(events))
	for i, event := range events {
		parts[i] = fmt.Sprintf("%s@%d(%s)", event.Kind, event.Revision, event.SessionID)
	}
	return strings.Join(parts, " ")
}

// expire fires the idle timer of a session the way its timer would, with the
// generation the session holds now.
func (w *kickstartWorld) expire(index int, name string) {
	id := fmt.Sprint(w.vars[name])
	w.server.kickstart.mu.Lock()
	sess, ok := w.server.kickstart.byID[id]
	var gen uint64
	if ok {
		gen = sess.idleGen
	}
	w.server.kickstart.mu.Unlock()
	if !ok {
		w.fail(index, "no live session %q to expire", id)
	}
	w.server.kickstart.expire(id, gen)
}

func (w *kickstartWorld) idleCheck(index int, want string) {
	duration, err := time.ParseDuration(want)
	if err != nil {
		w.fail(index, "idle %q: %v", want, err)
	}
	w.idleMu.Lock()
	defer w.idleMu.Unlock()
	for _, scheduled := range w.idle {
		if scheduled != duration {
			w.fail(index, "an idle timer was scheduled for %s, want %s", scheduled, duration)
		}
	}
	if len(w.idle) == 0 {
		w.fail(index, "no idle timer was scheduled")
	}
}

// kickstartWSClient is a websocket client of the kickstart topic.
type kickstartWSClient struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	events chan schema.LocalKickstartEvent
}

func (c *kickstartWSClient) close() {
	c.cancel()
	_ = c.conn.CloseNow()
}

func (w *kickstartWorld) wsStep(index int, step kickstartWSStep) {
	w.t.Helper()
	client, ok := w.clients[step.Client]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(w.baseURL, "http")+defaults.RouteWS.String(), &websocket.DialOptions{HTTPHeader: http.Header{defaults.HeaderOrigin: {w.baseURL}}})
		if err != nil {
			cancel()
			w.fail(index, "dial the websocket: %v", err)
		}
		client = &kickstartWSClient{conn: conn, cancel: cancel, events: make(chan schema.LocalKickstartEvent, 64)}
		w.clients[step.Client] = client
		go func() {
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				var message struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(data, &message) != nil || message.Type != "kickstart" {
					continue
				}
				var event schema.LocalKickstartEvent
				if err := json.Unmarshal(message.Data, &event); err != nil {
					continue
				}
				client.events <- event
			}
		}()
	}
	if step.Subscribe {
		payload := fmt.Sprintf(`{"type":"subscribe","channels":[{"topic":"%s"}]}`, schema.TopicKickstart)
		if err := client.conn.Write(context.Background(), websocket.MessageText, []byte(payload)); err != nil {
			w.fail(index, "subscribe: %v", err)
		}
	}
}

func (w *kickstartWorld) wsRead(index int, read kickstartWSRead) {
	w.t.Helper()
	client, ok := w.clients[read.Client]
	if !ok {
		w.fail(index, "no websocket client %q", read.Client)
	}
	for i, spec := range read.Events {
		event := testwait.Receive(w.t, client.events, fmt.Sprintf("kickstart event %d for client %s", i+1, read.Client))
		if ok, why := w.eventMatches(event, spec); !ok {
			w.fail(index, "client %s event %d (%s): %s", read.Client, i+1, event.Kind, why)
		}
	}
}

// commit drives the seam the commit owner uses, the way a commit route would.
func (w *kickstartWorld) commit(index int, step kickstartCommitStep) {
	w.t.Helper()
	id := fmt.Sprint(w.vars[step.Session])
	sessions := w.server.kickstart
	var err error
	switch step.Action {
	case "begin":
		var draft kickstartDraft
		draft, err = sessions.beginCommit(id)
		if err == nil {
			answered := make([]string, len(draft.Answered))
			for i, key := range draft.Answered {
				answered[i] = key.String()
			}
			if step.Answered != nil && !reflect.DeepEqual(answered, step.Answered) {
				w.fail(index, "the draft's answered fields are %v, want %v", answered, step.Answered)
			}
			for field, side := range step.Resolutions {
				if got := draft.Resolutions[onboarding.FieldKey(field)]; string(got.Choice) != side {
					w.fail(index, "the draft's resolution for %s is %q, want %q", field, got.Choice, side)
				}
			}
			for field, want := range step.Seen {
				got := draft.Resolutions[onboarding.FieldKey(field)].Seen
				wantJSON, _ := json.Marshal(want)
				if !equalSettingValue(got, wantJSON) {
					w.fail(index, "the draft's resolution for %s saw %s, want %s", field, got, wantJSON)
				}
			}
			if draft.SessionID != id || draft.Draft == nil || draft.Write.Config == nil {
				w.fail(index, "the draft does not carry its session, draft, and write context: %+v", draft)
			}
		}
	case "abort":
		sessions.abortCommit(id)
	case "close":
		var body schema.LocalKickstartCommitted
		for _, next := range onboarding.NextStepsWeb() {
			body.NextSteps = append(body.NextSteps, schema.LocalKickstartNextStep(next))
		}
		body.Saved, body.ConfigPath, body.Ingest = true, w.configPath, schema.LocalKickstartIngest{State: schema.LocalKickstartIngestIdle}
		var finish *kickstartFinish
		finish, err = sessions.closeCommitted(id, body)
		if err == nil {
			if step.Ingest != "" {
				finish.publishIngest(schema.LocalKickstartIngest{State: schema.LocalKickstartIngestState(step.Ingest)})
			}
		}
	case "ingest":
		// An import started after the session is gone publishes under its id.
		sessions.finishFor(id).publishIngest(schema.LocalKickstartIngest{State: schema.LocalKickstartIngestState(step.Ingest)})
	default:
		w.fail(index, "commit action %q is not begin, abort, close, or ingest", step.Action)
	}
	switch {
	case step.Error == "" && err != nil:
		w.fail(index, "commit %s: %v", step.Action, err)
	case step.Error != "" && err == nil:
		w.fail(index, "commit %s succeeded, want an error containing %q", step.Action, step.Error)
	case step.Error != "" && !strings.Contains(err.Error(), step.Error):
		w.fail(index, "commit %s: %v, want an error containing %q", step.Action, err, step.Error)
	}
}

// conflicts publishes conflict rows the way a refused commit does.
func (w *kickstartWorld) conflicts(index int, step kickstartConflictsStep) {
	w.t.Helper()
	rows := make([]schema.LocalKickstartConflict, len(step.Rows))
	for i, row := range step.Rows {
		mine, _ := json.Marshal(row.Mine)
		theirs, _ := json.Marshal(row.Theirs)
		rows[i] = schema.LocalKickstartConflict{Field: row.Field, Stop: row.Stop, Label: row.Label, Mine: mine, Theirs: theirs}
	}
	if _, err := w.server.kickstart.publishConflicts(fmt.Sprint(w.vars[step.Session]), rows); err != nil {
		w.fail(index, "publish conflicts: %v", err)
	}
}

func (w *kickstartWorld) configFile(index int, want string) {
	w.t.Helper()
	data, err := os.ReadFile(w.configPath)
	switch want {
	case "absent":
		if !errors.Is(err, os.ErrNotExist) {
			w.fail(index, "the configuration file should be absent; read it: %v", err)
		}
	case "unchanged":
		if err != nil || string(data) != w.configBytes {
			w.fail(index, "the configuration file changed: %v\n%s", err, data)
		}
	default:
		w.fail(index, "configFile %q is not unchanged or absent", want)
	}
}
