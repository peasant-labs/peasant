package main

import (
	"context"
	_ "embed"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/discoveryprogress"
)

//go:embed testdata/kickstart_discovery_progress.yaml
var kickstartDiscoveryProgressYAML []byte

//go:embed testdata/kickstart_discovery_progress.manifest.yaml
var kickstartDiscoveryProgressManifest []byte

type spinnerTestWriter struct{ lines chan string }

// Wrapping the narrow interface hides the underlying adapter's optional
// capability, as with any adapter that has not implemented progress reporting.
type adapterWithoutDiscoveryProgress struct{ ingest.SourceAdapter }

type spinnerDiscoveryEvents struct {
	spinner *discoverySpinner
	done    int
	total   int
	calls   int
}

func (p *spinnerDiscoveryEvents) DiscoveryTotal(_ ingest.Harness, total int) {
	p.total = total
	p.calls++
	p.spinner.SetProgress(p.done, p.total)
}

func (p *spinnerDiscoveryEvents) DiscoveryAdvance(_ ingest.Harness, delta int) {
	p.done += delta
	p.calls++
	p.spinner.SetProgress(p.done, p.total)
}

func (w *spinnerTestWriter) Write(p []byte) (int, error) {
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}

func TestKickstartDiscoverySpinnerProgress(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name  string `yaml:"name"`
			Mode  string `yaml:"mode"`
			Phase string `yaml:"phase"`
			Root  string `yaml:"root"`
			Files []struct {
				Path string `yaml:"path"`
				Body string `yaml:"body"`
			} `yaml:"files"`
			Block string `yaml:"block"`
			Want  string `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(kickstartDiscoveryProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(kickstartDiscoveryProgressManifest, "kickstart discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "kickstart discovery progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Mode == "unsupported-adapter" {
				fs := testutil.NewMemFS()
				for _, file := range c.Files {
					if err := fs.WriteFile(c.Root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				root, err := ingest.NewResolvedPath(c.Root)
				if err != nil {
					t.Fatal(err)
				}
				adapter := adapterWithoutDiscoveryProgress{SourceAdapter: ingest.NewClaudeAdapter(fs, nil, salt.Salt{})}
				spinner := discoveryprogress.New(io.Discard, false)
				spinner.SetPhase("previous")
				spinner.SetProgress(1, 2)
				spinner.SetDiscoveryReminedCount(1)
				spinner.SetPhase(c.Phase)
				events := &spinnerDiscoveryEvents{spinner: spinner}
				ingest.AttachDiscoveryProgress(adapter, events)
				sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
				if err != nil || len(sessions) != len(c.Files) || events.calls != 0 {
					t.Fatalf("unsupported adapter discovery: sessions=%d events=%d err=%v", len(sessions), events.calls, err)
				}
				line := spinner.View()
				if !strings.Contains(line, c.Want) || strings.Contains(line, "(") || strings.Contains(line, "re-read") {
					t.Fatalf("unsupported adapter spinner = %q; want %q with no inherited count", line, c.Want)
				}
				return
			}
			if c.Mode == "phase-reset" {
				writer := &spinnerTestWriter{lines: make(chan string, 16)}
				spinner := discoveryprogress.New(writer, true)
				spinner.SetPhase("previous")
				spinner.SetProgress(1, 2)
				spinner.SetDiscoveryReminedCount(1)
				spinner.SetPhase(c.Phase)
				defer spinner.Stop()
				ctx := testwait.Context(t)
				for {
					select {
					case line := <-writer.lines:
						if !strings.Contains(line, c.Want) {
							continue
						}
						if strings.Contains(line, "re-read") || strings.Contains(line, "1/2") {
							t.Fatalf("phase-only spinner = %q; want %q with no inherited count", line, c.Want)
						}
						return
					case <-ctx.Done():
						t.Fatal("phase-only spinner did not render")
					}
				}
			}
			if c.Mode != "discover" {
				t.Fatalf("unknown fixture mode %q", c.Mode)
			}
			fs := testutil.NewGatedFS(testutil.NewMemFS())
			for _, file := range c.Files {
				if err := fs.WriteFile(c.Root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs.Arm(fsdecorator.Gate{Op: fsdecorator.OpStat, Path: c.Root + "/" + c.Block})
			defer fs.Release()
			writer := &spinnerTestWriter{lines: make(chan string, 16)}
			spinner := discoveryprogress.New(writer, true)
			defer spinner.Stop()
			cfg := &config.Config{Sources: config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{c.Root}}}}
			done := make(chan struct{})
			go func() {
				discovery.Scan(context.Background(), cfg, fs, nil, nil, nil, spinner)
				close(done)
			}()
			ctx := testwait.Context(t)
			select {
			case <-fs.Reached():
			case <-ctx.Done():
				t.Fatal("discovery did not reach the gated source")
			}
			for {
				select {
				case line := <-writer.lines:
					if strings.Contains(line, c.Want) {
						fs.Release()
						select {
						case <-done:
						case <-ctx.Done():
							t.Fatal("scan did not finish after release")
						}
						return
					}
				case <-ctx.Done():
					t.Fatalf("spinner never rendered %q during discovery", c.Want)
				}
			}
		})
	}
}
