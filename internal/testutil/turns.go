package testutil

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TurnFixture is one recorded turn declared in a YAML fixture. It exists so
// every surface that renders or reads a transcript - the turn renderer, the
// kickstart preview, the mounted selection step, the command wiring over a real
// store - declares its cases in ONE shape, and a change to what a turn carries
// updates one struct instead of N inline tables.
type TurnFixture struct {
	Role      string            `yaml:"role"`
	EntryType string            `yaml:"entryType"`
	Content   string            `yaml:"content"`
	Depth     int               `yaml:"depth"`
	ToolCalls []ToolCallFixture `yaml:"toolCalls"`
}

// ToolCallFixture is one tool invocation inside a TurnFixture.
type ToolCallFixture struct {
	Name      string `yaml:"name"`
	Arguments string `yaml:"arguments"`
	Result    string `yaml:"result"`
	FilePath  string `yaml:"filePath"`
	IsError   bool   `yaml:"isError"`
}

// Turns converts fixture rows into the real ingest.Turn values a production
// read path produces, failing the test on any role or entry type outside the
// contract's closed sets. Failing closed here is what keeps a typo in a fixture
// from silently becoming a turn that renders as some other kind.
func Turns(t *testing.T, label string, rows []TurnFixture) []ingest.Turn {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s declares no turns; an empty transcript renders nothing to assert on", label)
	}
	turns := make([]ingest.Turn, 0, len(rows))
	for i, row := range rows {
		role := ingest.Role(row.Role)
		if !role.IsValid() {
			t.Fatalf("%s turn %d declares role %q, which is not one of the contract's roles", label, i, row.Role)
		}
		entryType := ingest.EntryType(row.EntryType)
		if !entryType.IsValid() {
			t.Fatalf("%s turn %d declares entry type %q, which is not one of the contract's entry types", label, i, row.EntryType)
		}
		if row.Content == "" && len(row.ToolCalls) == 0 {
			t.Fatalf("%s turn %d carries neither content nor a tool call; it renders as nothing", label, i)
		}
		calls := make([]ingest.ToolCall, 0, len(row.ToolCalls))
		for j, call := range row.ToolCalls {
			if call.Name == "" {
				t.Fatalf("%s turn %d tool call %d declares no name", label, i, j)
			}
			calls = append(calls, ingest.ToolCall{
				Name:      call.Name,
				Arguments: call.Arguments,
				Result:    call.Result,
				FilePath:  call.FilePath,
				IsError:   call.IsError,
			})
		}
		turns = append(turns, ingest.Turn{
			Index:     i,
			Role:      role,
			EntryType: entryType,
			Content:   row.Content,
			Depth:     row.Depth,
			ToolCalls: calls,
		})
	}
	return turns
}
