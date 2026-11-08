package ingest

import (
	"strings"
	"testing"
)

// Claude Code writes api-request* accounting records into subagent transcripts
// with sessionId and agentId but no isSidechain marker. They must not break
// native subagent proof, while every other proof requirement still holds.
func TestMineClaudeIdentityUnscopedRequestRecords(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	const agent = "a168506be0b789bce"
	msg := func(kind, extra string) string {
		return `{"type":"` + kind + `","uuid":"u-` + kind + `","sessionId":"` + owner + `","agentId":"` + agent + `"` + extra + `}`
	}
	conversation := []string{
		msg("user", `,"isSidechain":true,"message":{"role":"user","content":"task"}`),
		msg("assistant", `,"isSidechain":true,"message":{"role":"assistant","content":[]}`),
	}

	cases := []struct {
		name      string
		extra     []string
		wantValid bool
		wantContr bool
	}{
		{name: "request records without marker", extra: []string{msg("api-request", ""), msg("api-request-blob", ""), msg("api-request-shape", "")}, wantValid: true},
		{name: "request record with explicit false", extra: []string{msg("api-request", `,"isSidechain":false`)}, wantValid: false, wantContr: true},
		{name: "request record with foreign agent", extra: []string{strings.Replace(msg("api-request", ""), agent, "abcdef0123456789a", 1)}, wantValid: false},
		{name: "other unmarked record kind", extra: []string{msg("progress", "")}, wantValid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(strings.Join(append(append([]string{}, conversation...), tc.extra...), "\n") + "\n")
			var proof ClaudeTranscriptEvidence
			mineClaudeIdentity(data, &proof)
			if proof.NativeAgentValid != tc.wantValid {
				t.Fatalf("NativeAgentValid = %v, want %v", proof.NativeAgentValid, tc.wantValid)
			}
			if got := claudeContradictorySidechain(proof); got != tc.wantContr {
				t.Fatalf("claudeContradictorySidechain = %v, want %v", got, tc.wantContr)
			}
			if proof.NativeAgentID != agent {
				t.Fatalf("NativeAgentID = %q, want %q", proof.NativeAgentID, agent)
			}
		})
	}
}
