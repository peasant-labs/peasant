// Package scannerfix loads captured provider -> remote -> worktree -> session
// scan shapes from embedded YAML fixtures into the kit tree's *TreeNode forest,
// and provides the two test-only [kit.TreeSource] implementations the tree's
// async work is exercised against: a deterministic [FixtureTreeSource] and a
// deliberately-blocking [DelayedTreeSource] wrapper.
//
// It exists as its own package (not a _test.go helper) so the settings
// TreeField flow and the rebuilt kickstart's end-to-end tests can reuse the
// EXACT same fixtures and sources for their dev loop, without an import cycle:
// scannerfix imports kit, never the other way around. The REAL scanner adapter
// - the one that walks a live machine's providers and git worktrees - lives in
// the kickstart slice and is deliberately NOT here; these fixtures are the
// shared contract artifact standing in for it.
//
// The fixtures are the single source of truth for their own shape: each file
// declares an expectedNodeCount the loader asserts against the nodes it parsed
// (a row-count guard), so a fixture edit that adds or drops a node without
// updating the count fails loudly rather than silently changing what every
// downstream test scans.
package scannerfix

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"path"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/scanner/*.yaml
var fixtureFS embed.FS

const fixtureDir = "testdata/scanner"

// fixtureNode is one node as written in a scanner fixture. State is a string
// naming a kit.TriState ("unchecked"/"partial"/"checked"/"conflict") so the
// fixture stays human-editable; it is resolved to the typed enum on load and an
// unknown value fails the load rather than defaulting.
type fixtureNode struct {
	ID       string            `yaml:"id"`
	Label    string            `yaml:"label"`
	State    string            `yaml:"state"`
	Meta     map[string]string `yaml:"meta"`
	Children []fixtureNode     `yaml:"children"`
}

// fixtureDocument is one scanner fixture file: a named forest plus the
// self-consistency node count the row-count guard asserts.
type fixtureDocument struct {
	Name              string        `yaml:"name"`
	ExpectedNodeCount int           `yaml:"expectedNodeCount"`
	Roots             []fixtureNode `yaml:"roots"`
}

// parseState resolves a fixture state string to a kit.TriState, failing on an
// unknown value so a typo cannot silently become Unchecked.
func parseState(s string) (kit.TriState, error) {
	switch s {
	case "unchecked", "":
		return kit.Unchecked, nil
	case "partial":
		return kit.Partial, nil
	case "checked":
		return kit.Checked, nil
	case "conflict":
		return kit.Conflict, nil
	default:
		return kit.Unchecked, fmt.Errorf("unknown tri-state %q", s)
	}
}

// toKitNode converts a parsed fixtureNode (and its subtree) into a fresh
// *kit.TreeNode, counting every node it produces into n.
func toKitNode(fn fixtureNode, n *int) (*kit.TreeNode, error) {
	state, err := parseState(fn.State)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", fn.ID, err)
	}
	*n++
	node := &kit.TreeNode{
		ID:    fn.ID,
		Label: fn.Label,
		State: state,
	}
	if len(fn.Meta) > 0 {
		node.Meta = make(map[string]string, len(fn.Meta))
		for k, v := range fn.Meta {
			node.Meta[k] = v
		}
	}
	for _, c := range fn.Children {
		child, err := toKitNode(c, n)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, child)
	}
	return node, nil
}

// decodeDocument decodes exactly one YAML document from data, rejecting a
// second document (a stray "---" appended to a fixture).
func decodeDocument(data []byte) (fixtureDocument, error) {
	var doc fixtureDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("fixture must hold exactly one document: %w", err)
	}
	return doc, nil
}

// loadForest reads the named fixture and returns its root nodes, applying the
// row-count guard: the number of nodes actually parsed must equal the fixture's
// declared expectedNodeCount.
func loadForest(name string) ([]*kit.TreeNode, error) {
	data, err := fixtureFS.ReadFile(path.Join(fixtureDir, name+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("scannerfix: read fixture %q: %w", name, err)
	}
	doc, err := decodeDocument(data)
	if err != nil {
		return nil, fmt.Errorf("scannerfix: fixture %q: %w", name, err)
	}
	if doc.Name != name {
		return nil, fmt.Errorf("scannerfix: fixture %q declares name %q", name, doc.Name)
	}
	count := 0
	var roots []*kit.TreeNode
	for _, r := range doc.Roots {
		node, err := toKitNode(r, &count)
		if err != nil {
			return nil, fmt.Errorf("scannerfix: fixture %q: %w", name, err)
		}
		roots = append(roots, node)
	}
	if count != doc.ExpectedNodeCount || count == 0 {
		return nil, fmt.Errorf(
			"scannerfix: fixture %q declares expectedNodeCount=%d but has %d nodes (must be non-zero)",
			name, doc.ExpectedNodeCount, count)
	}
	return roots, nil
}

// Names returns the sorted set of available fixture names (a file
// "standard.yaml" is the fixture "standard").
func Names() ([]string, error) {
	entries, err := fixtureFS.ReadDir(fixtureDir)
	if err != nil {
		return nil, fmt.Errorf("scannerfix: list fixtures: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		const ext = ".yaml"
		if len(n) <= len(ext) || n[len(n)-len(ext):] != ext {
			continue
		}
		names = append(names, n[:len(n)-len(ext)])
	}
	sort.Strings(names)
	return names, nil
}

// Load parses the named fixture into a fresh *kit.TreeNode forest. Each call
// returns an independent copy, so a tree that mutates node State in place never
// clobbers the fixture for the next load.
func Load(name string) ([]*kit.TreeNode, error) {
	return loadForest(name)
}

// FixtureTreeSource is a deterministic [kit.TreeSource] backed by one embedded
// fixture. Every Load re-parses the fixture, so repeated loads (and the tree's
// in-place mutation of node State) start from the same captured shape.
type FixtureTreeSource struct {
	name string
}

// NewFixtureTreeSource returns a source that loads the named fixture. It does
// not validate the name until Load is called (so construction never fails); an
// unknown name surfaces as a Load error.
func NewFixtureTreeSource(name string) FixtureTreeSource {
	return FixtureTreeSource{name: name}
}

// Load implements kit.TreeSource. It honors ctx cancellation before doing any
// work so a cancelled load returns promptly.
func (s FixtureTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return loadForest(s.name)
}

var _ kit.TreeSource = FixtureTreeSource{}

// DelayedTreeSource wraps an inner [kit.TreeSource] and blocks its Load until a
// release signal is delivered (or ctx is cancelled). It is the async harness's
// instrument: it holds the tree in its loading state long enough for a test to
// assert the spinner renders and that scripted input is still processed, then
// Release lets the real load complete.
type DelayedTreeSource struct {
	inner   kit.TreeSource
	release chan struct{}
}

// NewDelayedTreeSource wraps inner with a fresh, un-released gate.
func NewDelayedTreeSource(inner kit.TreeSource) *DelayedTreeSource {
	return &DelayedTreeSource{
		inner:   inner,
		release: make(chan struct{}),
	}
}

// Release unblocks a pending (or future) Load. It is safe to call exactly once;
// a second call panics on a closed channel, mirroring sync.Once semantics the
// harness relies on to prove a single release path.
func (d *DelayedTreeSource) Release() { close(d.release) }

// Load blocks until Release is called or ctx is cancelled, then delegates to
// the inner source (returning the context error on cancellation so the tree's
// stale/cancel path is exercised).
func (d *DelayedTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	select {
	case <-d.release:
		return d.inner.Load(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ kit.TreeSource = (*DelayedTreeSource)(nil)
