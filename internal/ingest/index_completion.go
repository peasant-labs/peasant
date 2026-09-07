package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// indexCompletion accounts for strict parsing without changing the tolerant
// slice API used by previews. A nil account selects the legacy parsing policy.
type indexCompletion struct {
	ctx        context.Context
	session    DiscoveredSession
	line       int
	records    int
	recognized int
}

func (completion *indexCompletion) record(raw []byte) error {
	if err := completion.ctx.Err(); err != nil {
		return err
	}
	completion.records++
	if err := requireJSONObject(raw); err != nil {
		return err
	}
	return nil
}

func (completion *indexCompletion) failure(err error) error {
	return fmt.Errorf("%s indexer: session %s at %s line %d could not verify complete input: %w; no replacement result was produced and previous entries and producer stamps remain unchanged; restore a complete supported transcript and retry harvest", completion.session.Harness, completion.session.SessionID, completion.session.SourcePath, completion.line, err)
}

func (completion *indexCompletion) result(entries []schema.SessionEntry, err error) (indexformat.Result, error) {
	if err != nil {
		return nil, completion.failure(err)
	}
	if err := completion.ctx.Err(); err != nil {
		return nil, completion.failure(err)
	}
	if completion.records > 0 && completion.recognized == 0 {
		return nil, completion.failure(fmt.Errorf("the input contains no recognized transcript records; ignored future vocabulary alone cannot prove a supported empty transcript"))
	}
	return indexformat.V1{Entries: entries}, nil
}

// validateIndexContent checks the union consumed by Claude and Cursor. Unknown
// block vocabulary is retained, but malformed known fields cannot be discarded
// while certifying completion. Tool-result content remains raw JSON.
func validateIndexContent(content json.RawMessage) error {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return nil
	}
	var text string
	if content[0] == '"' {
		return json.Unmarshal(content, &text)
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return fmt.Errorf("message content must be a string or content-block array: %w", err)
	}
	for _, raw := range blocks {
		if err := requireJSONObject(raw); err != nil {
			return fmt.Errorf("content block: %w", err)
		}
		var block claudeFullBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("content block fields: %w", err)
		}
		if block.Type == "" {
			return fmt.Errorf("content block lacks its type")
		}
	}
	return nil
}
