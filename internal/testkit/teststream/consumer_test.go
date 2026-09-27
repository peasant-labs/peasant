// Package teststream_test is an external consumer of the shared stream library:
// it uses only exported names.
package teststream_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/teststream"
)

func TestConsumer_ParsesAStream(t *testing.T) {
	stream := `{"Action":"pass","Package":"example.com/m","Test":"TestOne","Elapsed":0.01}
{"Action":"fail","Package":"example.com/m","Test":"TestTwo","Elapsed":0.02}
`
	records, err := teststream.ParseStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := teststream.TopLevel(records); len(got) != 2 {
		t.Fatalf("top-level records = %d, want 2", len(got))
	}
	if got := teststream.Failing(records); len(got) != 1 || got[0].Test != "TestTwo" {
		t.Fatalf("failing records = %+v, want the one failed test", got)
	}
}
