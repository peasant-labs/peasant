package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/spf13/cobra"
)

// BuildPlayIngestCommand creates a demo command that plays the ingest
// animation with simulated progress bars.
func BuildPlayIngestCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "play-ingest",
		Short:  "Play the ingest animation demo with progress bars",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			progState := ingest.NewProgressState()
			renderer := newProgressRenderer(os.Stderr, progState, animation.IngestAnimation())
			go renderer.Run(ctx)

			simulateIngestProgress(progState)

			cancel()
			renderer.Wait()
			renderer.Clear()

			fmt.Fprintln(cmd.OutOrStdout(), "peasant harvest: 10 sessions (8 new, 2 updated, 0 unchanged, 0 errors) [4.2s]")
			return nil
		},
	}
}

// BuildPlayPushCommand creates a demo command that plays the push animation.
func BuildPlayPushCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "play-push",
		Short:  "Play the push animation demo",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			anim := animation.NewRenderer(os.Stderr, animation.PushAnimation())
			ctx, cancel := context.WithCancel(cmd.Context())
			go anim.Run(ctx)

			// play for ~5 animation cycles
			time.Sleep(12 * time.Second)

			cancel()
			anim.Wait()
			anim.Clear()

			fmt.Fprintln(cmd.OutOrStdout(), "Summary: 8 new, 2 updated, 0 error(s), 0 skipped, 0 held")
			return nil
		},
	}
}

// simulateIngestProgress feeds fake progress events to simulate the
// 10-stage pipeline running slowly enough to watch the animation.
func simulateIngestProgress(state *ingest.ProgressState) {
	stages := []struct {
		stage ingest.Stage
		total int
		delay time.Duration // per-item delay
	}{
		{ingest.StageDiscover, 10, 80 * time.Millisecond},
		{ingest.StageDiff, 10, 100 * time.Millisecond},
		{ingest.StageFilter, 10, 60 * time.Millisecond},
		{ingest.StageExtract, 8, 200 * time.Millisecond},
		{ingest.StageDBInsert, 8, 120 * time.Millisecond},
		{ingest.StageIndex, 8, 150 * time.Millisecond},
		{ingest.StageCompute, 8, 120 * time.Millisecond},
		{ingest.StageAnnotate, 8, 100 * time.Millisecond},
		{ingest.StageCleanup, 3, 200 * time.Millisecond},
		{ingest.StageReport, 1, 300 * time.Millisecond},
	}

	for _, s := range stages {
		state.Update(ingest.ProgressEvent{
			Kind:  ingest.KindStart,
			Stage: s.stage,
			Total: s.total,
		})
		for i := 1; i <= s.total; i++ {
			time.Sleep(s.delay)
			state.Update(ingest.ProgressEvent{
				Kind:  ingest.KindAdvance,
				Stage: s.stage,
				Done:  i,
				Total: s.total,
			})
		}
		state.Update(ingest.ProgressEvent{
			Kind:  ingest.KindEnd,
			Stage: s.stage,
			Total: s.total,
			Done:  s.total,
		})
	}
}
