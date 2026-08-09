package kit

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedAsyncOwnershipCaseCount      = 6
	expectedAsyncOwnershipTreeCount      = 3
	expectedAsyncOwnershipPreviewCount   = 3
	expectedAsyncOwnershipReversedCount  = 2
	expectedAsyncOwnershipForeignCount   = 2
	expectedAsyncOwnershipOwnerSwapCount = 2
)

//go:embed testdata/async_ownership.yaml
var asyncOwnershipData []byte

type asyncOwnershipComponent string

const (
	asyncOwnershipTree    asyncOwnershipComponent = "tree"
	asyncOwnershipPreview asyncOwnershipComponent = "preview"
)

func (c asyncOwnershipComponent) valid() bool {
	return c == asyncOwnershipTree || c == asyncOwnershipPreview
}

type asyncOwnershipDelivery string

const (
	asyncOwnershipReversed  asyncOwnershipDelivery = "reversed"
	asyncOwnershipForeign   asyncOwnershipDelivery = "foreign"
	asyncOwnershipOwnerSwap asyncOwnershipDelivery = "owner-swap"
)

func (d asyncOwnershipDelivery) valid() bool {
	return d == asyncOwnershipReversed || d == asyncOwnershipForeign || d == asyncOwnershipOwnerSwap
}

type asyncOwnershipCase struct {
	Name      string                  `yaml:"name"`
	Component asyncOwnershipComponent `yaml:"component"`
	Delivery  asyncOwnershipDelivery  `yaml:"delivery"`
	FirstID   string                  `yaml:"firstID"`
	SecondID  string                  `yaml:"secondID"`
}

type asyncOwnershipDocument struct {
	ExpectedCaseCount      int                  `yaml:"expectedCaseCount"`
	ExpectedTreeCaseCount  int                  `yaml:"expectedTreeCaseCount"`
	ExpectedPreviewCount   int                  `yaml:"expectedPreviewCaseCount"`
	ExpectedReversedCount  int                  `yaml:"expectedReversedCount"`
	ExpectedForeignCount   int                  `yaml:"expectedForeignCount"`
	ExpectedOwnerSwapCount int                  `yaml:"expectedOwnerSwapCount"`
	Cases                  []asyncOwnershipCase `yaml:"cases"`
}

func decodeAsyncOwnership(data []byte) (asyncOwnershipDocument, error) {
	var document asyncOwnershipDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/async_ownership.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("async_ownership.yaml must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedAsyncOwnershipCaseCount || len(document.Cases) != expectedAsyncOwnershipCaseCount {
		return document, fmt.Errorf("async ownership cases: declared=%d actual=%d required=%d", document.ExpectedCaseCount, len(document.Cases), expectedAsyncOwnershipCaseCount)
	}
	componentCounts := map[asyncOwnershipComponent]int{}
	deliveryCounts := map[asyncOwnershipDelivery]int{}
	seen := map[string]bool{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || !row.Component.valid() || !row.Delivery.valid() ||
			strings.TrimSpace(row.FirstID) == "" || strings.TrimSpace(row.SecondID) == "" || row.FirstID == row.SecondID {
			return document, fmt.Errorf("async ownership fixture contains an invalid or duplicate row: %#v", row)
		}
		seen[row.Name] = true
		componentCounts[row.Component]++
		deliveryCounts[row.Delivery]++
	}
	if document.ExpectedTreeCaseCount != expectedAsyncOwnershipTreeCount || componentCounts[asyncOwnershipTree] != expectedAsyncOwnershipTreeCount ||
		document.ExpectedPreviewCount != expectedAsyncOwnershipPreviewCount || componentCounts[asyncOwnershipPreview] != expectedAsyncOwnershipPreviewCount {
		return document, fmt.Errorf("async ownership component counts are not pinned: declared tree=%d preview=%d actual tree=%d preview=%d", document.ExpectedTreeCaseCount, document.ExpectedPreviewCount, componentCounts[asyncOwnershipTree], componentCounts[asyncOwnershipPreview])
	}
	if document.ExpectedReversedCount != expectedAsyncOwnershipReversedCount || deliveryCounts[asyncOwnershipReversed] != expectedAsyncOwnershipReversedCount ||
		document.ExpectedForeignCount != expectedAsyncOwnershipForeignCount || deliveryCounts[asyncOwnershipForeign] != expectedAsyncOwnershipForeignCount ||
		document.ExpectedOwnerSwapCount != expectedAsyncOwnershipOwnerSwapCount || deliveryCounts[asyncOwnershipOwnerSwap] != expectedAsyncOwnershipOwnerSwapCount {
		return document, fmt.Errorf("async ownership delivery counts are not pinned: reversed=%d foreign=%d owner-swap=%d", deliveryCounts[asyncOwnershipReversed], deliveryCounts[asyncOwnershipForeign], deliveryCounts[asyncOwnershipOwnerSwap])
	}
	return document, nil
}

func loadAsyncOwnership(t *testing.T) asyncOwnershipDocument {
	t.Helper()
	document, err := decodeAsyncOwnership(asyncOwnershipData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

type asyncTreeSource struct{ id string }

func (s asyncTreeSource) Load(ctx context.Context) ([]*TreeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []*TreeNode{{ID: s.id, Label: s.id}}, nil
}

type asyncPreviewSource struct{ prefix string }

func (s asyncPreviewSource) Content(id string, _ int) (string, error) {
	return s.prefix + ":" + id, nil
}

func asyncTestTheme() theme.Theme { return theme.New(theme.ModeDark) }

func previewResultFromCommand(t *testing.T, command tea.Cmd) previewLoadedMsg {
	t.Helper()
	var visit func(tea.Cmd) (previewLoadedMsg, bool)
	visit = func(cmd tea.Cmd) (previewLoadedMsg, bool) {
		if cmd == nil {
			return previewLoadedMsg{}, false
		}
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			for _, child := range batch {
				if result, found := visit(child); found {
					return result, true
				}
			}
			return previewLoadedMsg{}, false
		}
		result, ok := message.(previewLoadedMsg)
		return result, ok
	}
	result, ok := visit(command)
	if !ok {
		t.Fatal("preview load command produced no preview result")
	}
	return result
}

func assertTreeWaiting(t *testing.T, tree Tree) {
	t.Helper()
	if !tree.Loading() {
		t.Fatal("foreign async result ended tree loading")
	}
	if _, ok := tree.CurrentNode(); ok {
		t.Fatal("foreign async result populated a tree")
	}
}

func assertTreeLoadedID(t *testing.T, tree Tree, want string) {
	t.Helper()
	node, ok := tree.CurrentNode()
	if tree.Loading() || !ok || node.ID != want {
		t.Fatalf("tree state: loading=%t node=%v present=%t, want loaded %q", tree.Loading(), node, ok, want)
	}
}

func exerciseTreeOwnership(t *testing.T, row asyncOwnershipCase) {
	t.Helper()
	first := NewTree(asyncTestTheme(), asyncTreeSource{id: row.FirstID})
	second := NewTree(asyncTestTheme(), asyncTreeSource{id: row.SecondID})
	if !first.owner.valid() || !second.owner.valid() || first.owner == second.owner {
		t.Fatalf("tree owners must be nonzero and unique: first=%d second=%d", first.owner, second.owner)
	}
	firstCopy := first
	first, firstCommand := first.Load()
	second, secondCommand := second.Load()
	firstTick := first.spinner.inner.Tick().(spinner.TickMsg)
	if !first.OwnsAsync(firstTick) || second.OwnsAsync(firstTick) {
		t.Fatal("tree spinner ownership is not instance-specific")
	}
	firstResult := firstCommand().(treeLoadedMsg)
	secondResult := secondCommand().(treeLoadedMsg)
	if !first.OwnsAsync(firstResult) || first.OwnsAsync(secondResult) || !firstCopy.OwnsAsync(firstResult) {
		t.Fatal("tree OwnsAsync does not preserve instance identity across value copies")
	}

	switch row.Delivery {
	case asyncOwnershipReversed:
		first, _ = first.Update(secondResult)
		second, _ = second.Update(secondResult)
		assertTreeWaiting(t, first)
		assertTreeLoadedID(t, second, row.SecondID)
		second, _ = second.Update(firstResult)
		first, _ = first.Update(firstResult)
	case asyncOwnershipForeign:
		second, _ = second.Update(firstResult)
		assertTreeWaiting(t, second)
		first, _ = first.Update(firstResult)
		second, _ = second.Update(secondResult)
	case asyncOwnershipOwnerSwap:
		mutatedFirst := firstResult
		mutatedSecond := secondResult
		mutatedFirst.owner, mutatedSecond.owner = mutatedSecond.owner, mutatedFirst.owner
		first, _ = first.Update(mutatedFirst)
		second, _ = second.Update(mutatedSecond)
		assertTreeWaiting(t, first)
		assertTreeWaiting(t, second)
		first, _ = first.Update(firstResult)
		second, _ = second.Update(secondResult)
	}
	assertTreeLoadedID(t, first, row.FirstID)
	assertTreeLoadedID(t, second, row.SecondID)
}

func assertPreviewWaiting(t *testing.T, split PreviewSplit) {
	t.Helper()
	if !split.Loading() || split.body != nil {
		t.Fatalf("foreign async result changed preview: loading=%t body=%v", split.Loading(), split.body)
	}
}

func assertPreviewLoaded(t *testing.T, split PreviewSplit, prefix, id string) {
	t.Helper()
	if split.Loading() || split.body == nil {
		t.Fatalf("preview state: loading=%t body=%v, want loaded", split.Loading(), split.body)
	}
	if got, want := split.body.Render(80), prefix+":"+id; got != want {
		t.Fatalf("preview body=%q want=%q", got, want)
	}
}

func exercisePreviewOwnership(t *testing.T, row asyncOwnershipCase) {
	t.Helper()
	firstPrefix := "body-first"
	secondPrefix := "body-second"
	first := NewPreviewSplit(asyncTestTheme(), NewListLeftPane(NewList(asyncTestTheme(), []ListItem{StringItem(row.FirstID)})), asyncPreviewSource{prefix: firstPrefix})
	second := NewPreviewSplit(asyncTestTheme(), NewListLeftPane(NewList(asyncTestTheme(), []ListItem{StringItem(row.SecondID)})), asyncPreviewSource{prefix: secondPrefix})
	if !first.owner.valid() || !second.owner.valid() || first.owner == second.owner {
		t.Fatalf("preview owners must be nonzero and unique: first=%d second=%d", first.owner, second.owner)
	}
	firstCopy := first
	firstCommand := first.Load()
	secondCommand := second.Load()
	firstTick := first.spinner.inner.Tick().(spinner.TickMsg)
	if !first.OwnsAsync(firstTick) || second.OwnsAsync(firstTick) {
		t.Fatal("preview spinner ownership is not instance-specific")
	}
	firstResult := previewResultFromCommand(t, firstCommand)
	secondResult := previewResultFromCommand(t, secondCommand)
	if !first.OwnsAsync(firstResult) || first.OwnsAsync(secondResult) || !firstCopy.OwnsAsync(firstResult) {
		t.Fatal("PreviewSplit OwnsAsync does not preserve instance identity across value copies")
	}

	switch row.Delivery {
	case asyncOwnershipReversed:
		first, _ = first.Update(secondResult)
		second, _ = second.Update(secondResult)
		assertPreviewWaiting(t, first)
		assertPreviewLoaded(t, second, secondPrefix, row.SecondID)
		second, _ = second.Update(firstResult)
		first, _ = first.Update(firstResult)
	case asyncOwnershipForeign:
		second, _ = second.Update(firstResult)
		assertPreviewWaiting(t, second)
		first, _ = first.Update(firstResult)
		second, _ = second.Update(secondResult)
	case asyncOwnershipOwnerSwap:
		mutatedFirst := firstResult
		mutatedSecond := secondResult
		mutatedFirst.owner, mutatedSecond.owner = mutatedSecond.owner, mutatedFirst.owner
		first, _ = first.Update(mutatedFirst)
		second, _ = second.Update(mutatedSecond)
		assertPreviewWaiting(t, first)
		assertPreviewWaiting(t, second)
		first, _ = first.Update(firstResult)
		second, _ = second.Update(secondResult)
	}
	assertPreviewLoaded(t, first, firstPrefix, row.FirstID)
	assertPreviewLoaded(t, second, secondPrefix, row.SecondID)
}

func TestAsyncComponentsRejectForeignOwnersBeforeSequence(t *testing.T) {
	for _, row := range loadAsyncOwnership(t).Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			switch row.Component {
			case asyncOwnershipTree:
				exerciseTreeOwnership(t, row)
			case asyncOwnershipPreview:
				exercisePreviewOwnership(t, row)
			}
		})
	}
}

func TestAsyncOwnerAllocatorIsAtomicAndFailsClosedAtWrap(t *testing.T) {
	const constructorCount = 128
	ids := make(chan asyncOwnerID, constructorCount)
	var group sync.WaitGroup
	group.Add(constructorCount)
	for range constructorCount {
		go func() {
			defer group.Done()
			ids <- NewTree(asyncTestTheme(), asyncTreeSource{id: "node"}).owner
		}()
	}
	group.Wait()
	close(ids)
	seen := map[asyncOwnerID]bool{}
	for id := range ids {
		if !id.valid() || seen[id] {
			t.Fatalf("allocator returned zero or duplicate owner %d", id)
		}
		seen[id] = true
	}
	if len(seen) != constructorCount {
		t.Fatalf("allocated owners=%d want=%d", len(seen), constructorCount)
	}

	var exhausted asyncOwnerAllocator
	exhausted.last.Store(^uint64(0) - 1)
	if got := exhausted.next(); got != asyncOwnerID(^uint64(0)) {
		t.Fatalf("last valid owner=%d want=%d", got, asyncOwnerID(^uint64(0)))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("allocator allowed owner wrap instead of failing closed")
		}
		if got := exhausted.last.Load(); got != ^uint64(0) {
			t.Fatalf("exhausted allocator counter=%d want pinned maximum", got)
		}
	}()
	_ = exhausted.next()
}

func TestAsyncOwnershipFixtureGuards(t *testing.T) {
	if _, err := decodeAsyncOwnership(append(append([]byte(nil), asyncOwnershipData...), []byte("\nunknownField: true\n")...)); err == nil {
		t.Fatal("async ownership fixture accepted an unknown field")
	}
	if _, err := decodeAsyncOwnership(append(append([]byte(nil), asyncOwnershipData...), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("async ownership fixture accepted a trailing document")
	}
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedAsyncOwnershipCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedAsyncOwnershipCaseCount+1))
	mutated := bytes.Replace(asyncOwnershipData, declared, changed, 1)
	if bytes.Equal(mutated, asyncOwnershipData) {
		t.Fatal("async ownership count mutation did not alter the fixture")
	}
	if _, err := decodeAsyncOwnership(mutated); err == nil {
		t.Fatal("async ownership fixture accepted a changed exact count")
	}
}
