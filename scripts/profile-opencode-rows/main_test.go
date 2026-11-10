package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	out, err := runCorpus(t, ignoredOnlyInputs(), "timing", 1)
	if err != nil {
		t.Fatalf("ignored-only timing run: %v", err)
	}
	if out.Refused != 0 || out.Ignored != 1 || out.Success != 0 {
		t.Fatalf("ignored-only report = %+v, want zero refusals and one ignored", out)
	}
	if out.DecodeSeconds <= 0 || out.AllocatedBytes == 0 || out.Allocations == 0 {
		t.Fatalf("ignored-only timing suppressed a metric: %+v", out)
	}
	// The bracket must enclose the repeated decode loop. Compare the reported
	// time against the same decodes measured independently: a bracket moved
	// outside the loop reports nanoseconds against microseconds of real work.
	const baselineReps = 256
	ignored := ignoredOnlyInputs()[0]
	benchStart := time.Now()
	for range baselineReps {
		if _, _, err := ingest.DecodeOpenCodeProvenanceRow(ignored.Row, ignored.Scope); err != nil && !ingest.OpenCodeControlRowIgnored(err) {
			t.Fatal(err)
		}
	}
	independent := time.Since(benchStart)
	slow, err := runCorpus(t, ignoredOnlyInputs(), "timing", baselineReps)
	if err != nil {
		t.Fatalf("ignored-only timing at %d repetitions: %v", baselineReps, err)
	}
	if slow.DecodeSeconds < independent.Seconds()/4 || slow.DecodeSeconds > independent.Seconds()*4 {
		t.Fatalf("reported %v s for %d rows, but the same decodes take %v s here: the timing bracket no longer encloses the decode loop", slow.DecodeSeconds, baselineReps, independent)
	}
}

// TestRefusedCorpusSuppressesTiming drives the other side of the gate: a
// malformed row keeps the run invalid.
func TestRefusedCorpusSuppressesTiming(t *testing.T) {
	inputs := []input{profileInput("msg_bad_idle", "idle", `{"outcome":"succeeded"}`)}
	out, err := runCorpus(t, inputs, "timing", 1)
	if err == nil || !strings.Contains(err.Error(), "timing invalid") {
		t.Fatalf("refused timing run error = %v, want the invalid-timing refusal", err)
	}
	if out.Refused != 1 || out.DecodeSeconds != 0 || out.AllocatedBytes != 0 || out.Allocations != 0 {
		t.Fatalf("refused report = %+v, want the timing fields suppressed", out)
	}
}

// runCorpus writes the inputs to a scratch corpus, runs the real profiler, and
// decodes the report it printed.
func runCorpus(t *testing.T, inputs []input, phase string, repetitions int) (report, error) {
	t.Helper()
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(corpus, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runErr := run(corpus, phase, repetitions, &out)
	var decodedReport report
	if err := json.NewDecoder(&out).Decode(&decodedReport); err != nil {
		t.Fatalf("decode profiler report: %v", err)
	}
	return decodedReport, runErr
}
