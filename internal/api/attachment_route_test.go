package api

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestAttachmentRouteUsesTheServerMux proves the mounted production route,
// including a row whose file is missing. The path value is supplied by the
// server mux, not by trimming the URL inside the handler.
func TestAttachmentRouteUsesTheServerMux(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	storetest.SeedSession(t, db, testutil.TestSessionUUID)
	if err := db.EnsureAttachmentDir(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	row, err := db.AttachmentStore().Put(context.Background(), attachments.Pending{
		Row: attachments.Row{
			SessionID:      testutil.TestSessionUUID,
			SourceEntryRef: "img-route",
			PartitionID:    "main",
			MediaType:      "image/png",
		},
		Bytes: buf.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := NewServer(ServerConfig{Port: 0, Store: db})
	if err := srv.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, listener := range srv.lns {
			_ = listener.Close()
		}
	}()
	go func() { _ = srv.Serve(ctx) }()

	base := "http://" + srv.Addr().String()
	ok, err := http.Get(base + "/api/v1/attachments/" + row.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK || ok.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("mounted route status=%d type=%q, want 200 image/png", ok.StatusCode, ok.Header.Get("Content-Type"))
	}
	if err := os.Remove(filepath.Join(db.AttachmentStore().Dir(), row.SHA256)); err != nil {
		t.Fatal(err)
	}
	missing, err := http.Get(base + "/api/v1/attachments/" + row.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("row with a missing file status = %d, want 404", missing.StatusCode)
	}
}
