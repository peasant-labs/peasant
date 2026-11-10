package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

const profileSessionID = "ses_3cd91f52effeXd3QAJ54jProf"

func profileScope() ingest.OpenCodeProvenanceScope {
	return ingest.OpenCodeProvenanceScope{SessionID: profileSessionID, Shape: ingest.OpenCodeProvenanceCurrent, ParentNullProven: true}
}

func profileInput(id, kind, data string) input {
	return input{
		Row:   ingest.OpenCodeProvenanceRow{ID: id, SessionID: profileSessionID, Type: kind, Data: data},
		Scope: profileScope(),
	}
}

func ignoredOnlyInputs() []input {
	return []input{profileInput("msg_idle", "idle", `{"time":{"created":2},"outcome":"succeeded"}`)}
}

// TestCountRowsSeparatesIgnoredFromRefused pins the classification the timing
// gate relies on: an interpreted idle marker is not a refusal, a malformed
// marker is, and a message-producing row is a success.
func TestCountRowsSeparatesIgnoredFromRefused(t *testing.T) {
	inputs := []input{
		profileInput("msg_user", "user", `{"id":"msg_user","text":"hi","files":[],"agents":[],"time":{"created":1}}`),
		profileInput("msg_idle", "idle", `{"time":{"created":2},"outcome":"succeeded"}`),
		profileInput("msg_bad_idle", "idle", `{"outcome":"succeeded"}`),
	}
	counts, err := countRows(inputs, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Success != 1 || counts.Ignored != 1 || counts.Refused != 1 {
		t.Fatalf("counts = %+v, want one success, one ignored, one refusal", counts)
	}
	if len(counts.Rows) != 3 {
		t.Fatalf("parity rows = %d, want 3", len(counts.Rows))
	}
	if !counts.Rows[1].Ignored || counts.Rows[1].Refused {
		t.Fatalf("idle parity row = %+v, want ignored and not refused", counts.Rows[1])
	}
	if !counts.Rows[2].Refused || counts.Rows[2].Ignored {
		t.Fatalf("malformed parity row = %+v, want refused and not ignored", counts.Rows[2])
	}
}

// TestIgnoredOnlyCorpusKeepsTiming drives the real timing path: an ignored
// marker must not suppress the decode/allocation numbers.
func TestIgnoredOnlyCorpusKeepsTiming(t *testing.T) {
	out, err := runCorpus(t, ignoredOnlyInputs(), "timing")
	if err != nil {
		t.Fatalf("ignored-only timing run: %v", err)
	}
	if out.Refused != 0 || out.Ignored != 1 || out.Success != 0 {
		t.Fatalf("ignored-only report = %+v, want zero refusals and one ignored", out)
	}
	if out.DecodeSeconds <= 0 {
		t.Fatalf("ignored-only timing reported no decode time: %+v", out)
	}
}

// TestRefusedCorpusSuppressesTiming drives the other side of the gate: a
// malformed row keeps the run invalid.
func TestRefusedCorpusSuppressesTiming(t *testing.T) {
	inputs := []input{profileInput("msg_bad_idle", "idle", `{"outcome":"succeeded"}`)}
	_, err := runCorpus(t, inputs, "timing")
	if err == nil || !strings.Contains(err.Error(), "timing invalid") {
		t.Fatalf("refused timing run error = %v, want the invalid-timing refusal", err)
	}
}

// runCorpus writes the inputs to a scratch corpus, runs the real profiler, and
// decodes the report it printed.
func runCorpus(t *testing.T, inputs []input, phase string) (report, error) {
	t.Helper()
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(corpus, raw, 0600); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	runErr := run(corpus, phase, 1)
	os.Stdout = old
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var out report
	if err := json.NewDecoder(reader).Decode(&out); err != nil {
		t.Fatalf("decode profiler report: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return out, runErr
}
