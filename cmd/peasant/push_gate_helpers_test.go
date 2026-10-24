package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
)

//go:embed testdata/push_gate_boundaries.yaml
var pushGateBoundariesYAML []byte

type pushGateBoundaryCase struct {
	Name           string   `yaml:"name"`
	UploadBudget   string   `yaml:"uploadBudget"`
	SessionID      string   `yaml:"sessionID"`
	Attempts       []string `yaml:"attempts"`
	ErrorContains  []string `yaml:"errorContains"`
	StdoutContains []string `yaml:"stdoutContains"`
}

func loadPushGateBoundary(t *testing.T, name string) pushGateBoundaryCase {
	t.Helper()
	var f struct {
		RequiredNames []string               `yaml:"requiredNames"`
		Cases         []pushGateBoundaryCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(pushGateBoundariesYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("required push boundary fixture %q is missing", name)
	return pushGateBoundaryCase{}
}

type pushGateRoundTrip func(*http.Request) (*http.Response, error)

func (f pushGateRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var _ http.RoundTripper = pushGateRoundTrip(nil)

// Observe the real response body's first Read without supplying data or errors.
type pushGateReceiptBody struct {
	io.ReadCloser
	reading func()
}

func (b *pushGateReceiptBody) Read(p []byte) (int, error) {
	b.reading()
	return b.ReadCloser.Read(p)
}

var _ io.ReadCloser = (*pushGateReceiptBody)(nil)

type pushGateCommandResult struct {
	stdout, stderr string
	err            error
}

func (r pushGateCommandResult) String() string {
	return fmt.Sprintf("error=%v stdout=%q stderr=%q", r.err, r.stdout, r.stderr)
}

var _ fmt.Stringer = pushGateCommandResult{}

func executePushGateCommand(ctx context.Context, cmd *cobra.Command, dir string, args []string) pushGateCommandResult {
	root := newTestRoot()
	root.AddCommand(cmd)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "push"}, args...))
	err := root.ExecuteContext(ctx)
	return pushGateCommandResult{stdout.String(), stderr.String(), err}
}
