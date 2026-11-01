package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

const savedMetadataSuffix = "--metadata.json"

// savedTranscriptDamage names one session whose saved pair under the output
// directory is missing or no longer matches the identity the database holds.
type savedTranscriptDamage struct {
	SessionID schema.SessionID
	Reason    string
}

// verifySavedTranscripts checks every session that records a saved pair
// identity against the pair on disk: the pair must exist, its transcript must
// hash to the checksum in its metadata, and its identity must match the row.
func verifySavedTranscripts(ctx context.Context, output string, stored map[schema.SessionID]string) ([]savedTranscriptDamage, error) {
	filesystem := &ingest.OSFileSystem{}
	onDisk := make(map[schema.SessionID]string)
	readErrs := make(map[schema.SessionID]error)
	err := filepath.WalkDir(output, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == output {
				return filepath.SkipAll
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, savedMetadataSuffix) {
			return nil
		}
		id, err := schema.NewSessionID(strings.TrimSuffix(name, savedMetadataSuffix))
		if err != nil {
			return nil
		}
		if _, tracked := stored[id]; !tracked {
			return nil
		}
		pair, err := ingest.ReadManagedPair(filesystem, output, path, ingest.SessionID(id))
		if err != nil {
			readErrs[id] = err
			return nil
		}
		onDisk[id] = pair.ArtifactHash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk the saved transcripts under %s: %w", output, err)
	}
	var damaged []savedTranscriptDamage
	for id, want := range stored {
		got, found := onDisk[id]
		switch {
		case readErrs[id] != nil && !found:
			reason := "the saved transcript is unreadable or no longer matches its metadata checksum"
			if errors.Is(readErrs[id], fs.ErrNotExist) {
				reason = "the saved transcript is missing"
			}
			damaged = append(damaged, savedTranscriptDamage{SessionID: id, Reason: reason})
		case !found:
			damaged = append(damaged, savedTranscriptDamage{SessionID: id, Reason: "the saved metadata and transcript are missing"})
		case got != want:
			damaged = append(damaged, savedTranscriptDamage{SessionID: id, Reason: "the saved pair no longer matches the identity the database recorded"})
		}
	}
	sort.Slice(damaged, func(i, j int) bool { return damaged[i].SessionID < damaged[j].SessionID })
	return damaged, nil
}
