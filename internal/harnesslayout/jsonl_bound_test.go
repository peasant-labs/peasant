package harnesslayout

import (
	"bytes"
	"strings"
	"testing"
)

func TestShapeJSONLRejectsLineOverTheBound(t *testing.T) {
	previous := maxJSONLLine
	maxJSONLLine = 64 << 10
	t.Cleanup(func() { maxJSONLLine = previous })

	artifact := Artifact{Name: "transcript", Format: FormatJSONL, Role: RoleTranscript}
	rec := NewShapeRecorder(artifact, "")
	payload := append(bytes.Repeat([]byte("x"), maxJSONLLine+1), '\n')
	err := ShapeJSONL(bytes.NewReader(payload), rec, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("error = %v, want the scanner token-too-long error", err)
	}
	if rec.shape.Records != 0 || rec.shape.Malformed != 0 {
		t.Fatalf("records=%d malformed=%d, want the line rejected before it is decoded", rec.shape.Records, rec.shape.Malformed)
	}

	short := NewShapeRecorder(artifact, "")
	if err := ShapeJSONL(strings.NewReader("not-json\n"), short, nil, nil); err != nil || short.shape.Malformed != 1 || short.shape.Records != 0 {
		t.Fatalf("short malformed line: err=%v records=%d malformed=%d", err, short.shape.Records, short.shape.Malformed)
	}
}
