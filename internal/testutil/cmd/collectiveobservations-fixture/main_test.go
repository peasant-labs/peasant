package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/seeder-tests.yaml
var casesYAML []byte

func TestDisposableSeederProductionStore(t *testing.T) {
	var corpus struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name string `yaml:"name"`
			Mode string `yaml:"mode"`
		} `yaml:"cases"`
		Kinds               []schema.LocalContributionEvidenceKind `yaml:"kinds"`
		SeededEvidenceTotal int64                                  `yaml:"seededEvidenceTotal"`
		PageLimit           int                                    `yaml:"pageLimit"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(casesYAML)))
	d.KnownFields(true)
	if err := d.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range corpus.Cases {
		if names[c.Name] {
			t.Fatal("duplicate case")
		}
		names[c.Name] = true
	}
	for _, name := range corpus.RequiredNames {
		if !names[name] {
			t.Fatalf("required case %s absent", name)
		}
	}
	kinds := map[schema.LocalContributionEvidenceKind]bool{}
	for _, kind := range corpus.Kinds {
		if !kind.IsValid() || kinds[kind] {
			t.Fatal("fixture category membership is invalid or repeated")
		}
		kinds[kind] = true
	}
	for _, kind := range schema.AllLocalContributionEvidenceKinds {
		if !kinds[kind] {
			t.Fatalf("required evidence category %s absent", kind)
		}
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Mode == "export" {
				f, err := loadFixtures()
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := exportFixtures(f, &out); err != nil {
					t.Fatal(err)
				}
				var exported struct {
					ContentBytesBase64 string                    `json:"contentBytesBase64"`
					Metadata           schema.PullTranscriptInfo `json:"metadata"`
				}
				if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
					t.Fatal(err)
				}
				blob, err := base64.StdEncoding.DecodeString(exported.ContentBytesBase64)
				if err != nil {
					t.Fatal(err)
				}
				if len(blob) == 0 || schema.ComputeTranscriptHash(blob) != exported.Metadata.ContentHash {
					t.Fatal("fixture content hash does not match the canonical served-byte identity")
				}
				return
			}
			root := t.TempDir()
			owner := "disposable-seeder-test-owner"
			t.Setenv("PEASANT_OBSERVATIONS_OWNER", owner)
			if c.Mode != "ownership" {
				if err := os.WriteFile(filepath.Join(root, ".collective-observations-fixture"), []byte(owner), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := func(action, account string) (schema.CollectiveObservationResponse, error) {
				var out bytes.Buffer
				err := run(t.Context(), []string{"--root", root, "--village-url", "http://127.0.0.1:19999", "--account", account, "--action", action}, &out)
				var view schema.CollectiveObservationResponse
				if err == nil {
					err = json.Unmarshal(out.Bytes(), &view)
				}
				return view, err
			}
			before, err := command("seed", "alice")
			if c.Mode == "ownership" {
				if err == nil {
					t.Fatal("missing ownership accepted")
				}
				if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
					t.Fatal("unowned data was created")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			read := func() map[schema.LocalContributionEvidenceKind]schema.LocalContributionHistoryResponse {
				snapshot, err := auth.CaptureCredentials(t.Context(), filepath.Join(root, "config"))
				if err != nil {
					t.Fatal(err)
				}
				db, err := store.Open(defaults.ResolveDBFilePathWith(filepath.Join(root, "data")).String(), store.WithSkipMigrations())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				pages := map[schema.LocalContributionEvidenceKind]schema.LocalContributionHistoryResponse{}
				for _, kind := range corpus.Kinds {
					if !kind.IsValid() {
						t.Fatal("fixture kind is invalid")
					}
					page, err := db.ReadContributionHistory(context.Background(), snapshot, schema.LocalContributionHistoryQuery{Kind: kind, Page: 1, Limit: corpus.PageLimit})
					if err != nil {
						t.Fatal(err)
					}
					pages[kind] = page
				}
				return pages
			}
			pages := read()
			for _, page := range pages {
				if page.Total != corpus.SeededEvidenceTotal || page.Limit != corpus.PageLimit {
					t.Fatalf("fixture did not create three real pages: %+v", page)
				}
			}
			if c.Mode == "account" {
				after, err := command("seed", "bob")
				if err != nil {
					t.Fatal(err)
				}
				if before.Scope == after.Scope {
					t.Fatal("account selection did not change real scope")
				}
				return
			}
			if _, err := command(c.Mode, "alice"); err != nil {
				t.Fatal(err)
			}
			if c.Mode == "association-invalidated" {
				f, err := loadFixtures()
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := auth.CaptureCredentials(t.Context(), filepath.Join(root, "config"))
				if err != nil {
					t.Fatal(err)
				}
				db, err := store.Open(defaults.ResolveDBFilePathWith(filepath.Join(root, "data")).String(), store.WithSkipMigrations())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				id, err := schema.NewTranscriptID(f.TranscriptID)
				if err != nil {
					t.Fatal(err)
				}
				associations, err := db.ReadTranscriptAssociations(t.Context(), snapshot, []schema.TranscriptID{id})
				if err != nil {
					t.Fatal(err)
				}
				if associations[id].Metadata == nil || associations[id].Metadata.State != schema.PullAssociationMetadataInvalidated || len(associations[id].Collectives) != 0 {
					t.Fatal("known negative did not invalidate the actual scoped disclosed set")
				}
			}
			after := read()
			for kind, old := range pages {
				changed := old.TraversalRevision != after[kind].TraversalRevision
				diagnosticChanged := c.Mode == "diagnostic" && kind == schema.LocalContributionEvidenceOperationDiagnostics
				attemptChanged := c.Mode == "terminal" && kind == schema.LocalContributionEvidenceRemoteAttempts
				want := diagnosticChanged || attemptChanged
				if changed != want {
					t.Fatalf("%s changed=%v, want %v after %s", kind, changed, want, c.Mode)
				}
			}
		})
	}
}
