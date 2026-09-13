package transcript

import "github.com/peasant-labs/schema"

// Compatibility integers summarize only known assistant fields. Tool and
// summary evidence remains exclusively in detailed usage, never repriced.
func piLegacyMirrors(detail *schema.SessionDetailPayload) error {
	detail.TokensIn, detail.TokensOut, detail.TotalTokens = 0, 0, 0
	detail.TurnCount = len(detail.Turns)
	detail.ToolCallCount = 0
	for i := range detail.Turns {
		t := &detail.Turns[i]
		detail.ToolCallCount += len(t.ToolCalls)
		t.TokensIn, t.TokensOut = nil, nil
		if t.Usage == nil || t.Usage.Scope != schema.UsageScopeAssistant || t.Usage.Tokens == nil {
			continue
		}
		tokens := t.Usage.Tokens
		if tokens.Input != nil {
			n := int(*tokens.Input)
			t.TokensIn = &n
			if err := addPiMirror(&detail.TokensIn, n); err != nil {
				return err
			}
		}
		if tokens.Output != nil {
			n := int(*tokens.Output)
			t.TokensOut = &n
			if err := addPiMirror(&detail.TokensOut, n); err != nil {
				return err
			}
		}
		for _, value := range []*int64{tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheWrite} {
			if value != nil {
				if err := addPiMirror(&detail.TotalTokens, int(*value)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func addPiMirror(sum *int, value int) error {
	const maxSafe = 9007199254740991
	if value < 0 || *sum > maxSafe-value {
		return projectionError("assistant compatibility token sum exceeds JS-safe integer range")
	}
	*sum += value
	return nil
}
