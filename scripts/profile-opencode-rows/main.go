// profile-opencode-rows measures the production native-row decode boundary.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

type input struct {
	Row   ingest.OpenCodeProvenanceRow
	Scope ingest.OpenCodeProvenanceScope
}

type decoded struct {
	Message ingest.OpenCodeProvenanceMessage
	Settled bool
	Refused bool
}

type caseResult struct {
	Case    int    `json:"case"`
	Digest  string `json:"digest"`
	Refused bool   `json:"refused"`
}

type report struct {
	Phase          string       `json:"phase"`
	CorpusDigest   string       `json:"corpusDigest"`
	Cases          int          `json:"cases"`
	Repetitions    int          `json:"repetitions"`
	Success        int          `json:"success"`
	Refused        int          `json:"refused"`
	Rows           []caseResult `json:"rows,omitempty"`
	DecodeSeconds  float64      `json:"decodeSeconds,omitempty"`
	AllocatedBytes uint64       `json:"allocatedBytes,omitempty"`
	Allocations    uint64       `json:"allocations,omitempty"`
}

func main() {
	corpus := flag.String("corpus", "", "explicit JSON row/scope corpus; no native source acquisition")
	phase := flag.String("phase", "parity", "parity or timing (separate invocations)")
	repetitions := flag.Int("repetitions", 1, "positive timing repetition count; parity requires 1")
	flag.Parse()
	if err := run(*corpus, *phase, *repetitions); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(corpus, phase string, repetitions int) error {
	if phase != "parity" && phase != "timing" {
		return errors.New("use -phase=parity or -phase=timing")
	}
	if repetitions <= 0 || (phase == "parity" && repetitions != 1) {
		return errors.New("use positive -repetitions for timing and 1 for parity")
	}
	raw, err := os.ReadFile(corpus)
	if err != nil {
		return errors.New("corpus read failed; supply a readable explicit fixture corpus")
	}
	var inputs []input
	if json.Unmarshal(raw, &inputs) != nil || len(inputs) == 0 {
		return errors.New("corpus decode failed or empty; supply a non-empty JSON array of Row and Scope objects")
	}
	if repetitions > math.MaxInt/len(inputs) {
		return errors.New("decode count overflows; reduce -repetitions or corpus size")
	}
	out := report{Phase: phase, CorpusDigest: fmt.Sprintf("%x", sha256.Sum256(raw)), Cases: len(inputs), Repetitions: repetitions}
	var before, after runtime.MemStats
	var start time.Time
	var last ingest.OpenCodeProvenanceMessage
	if phase == "timing" {
		runtime.ReadMemStats(&before)
		start = time.Now()
	}
	for range repetitions {
		for i, in := range inputs {
			message, settled, decodeErr := ingest.DecodeOpenCodeProvenanceRow(in.Row, in.Scope)
			last = message
			if decodeErr == nil {
				out.Success++
			} else {
				out.Refused++
			}
			if phase == "parity" {
				// Maps encode deterministically; raw payload bytes, settled state,
				// and partial returned messages on refusal all participate.
				h := sha256.New()
				if json.NewEncoder(h).Encode(decoded{Message: message, Settled: settled, Refused: decodeErr != nil}) != nil {
					return fmt.Errorf("case %d: output encoding failed; inspect the decoder return types", i)
				}
				out.Rows = append(out.Rows, caseResult{Case: i, Digest: fmt.Sprintf("%x", h.Sum(nil)), Refused: decodeErr != nil})
			}
		}
	}
	if phase == "timing" {
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if out.Refused == 0 {
			out.DecodeSeconds = elapsed.Seconds()
			out.AllocatedBytes = after.TotalAlloc - before.TotalAlloc
			out.Allocations = after.Mallocs - before.Mallocs
		}
	}
	runtime.KeepAlive(last)
	if json.NewEncoder(os.Stdout).Encode(out) != nil {
		return errors.New("report write failed; choose a writable scratch output destination")
	}
	if phase == "timing" && out.Refused != 0 {
		return errors.New("timing invalid: row refusals occurred; filter using parity case ordinals, recheck output parity, then rerun timing")
	}
	return nil
}
