//go:build unix

package pull_test

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/install-permissions.yaml
var installPermissionsYAML []byte

type installPermissionsCase struct {
	Name              string `yaml:"name"`
	Recovery          bool   `yaml:"recovery"`
	FailBarrier       bool   `yaml:"failBarrier"`
	TrailingSeparator bool   `yaml:"trailingSeparator"`
}

func loadInstallPermissionsFixtures(t *testing.T) []installPermissionsCase {
	t.Helper()
	var corpus struct {
		RequiredNames []string                 `yaml:"requiredNames"`
		Cases         []installPermissionsCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(installPermissionsYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("empty/duplicate permission fixture: %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range corpus.RequiredNames {
		if !seen[name] {
			t.Fatalf("required permission fixture missing: %s", name)
		}
	}
	return corpus.Cases
}

func TestContentInstallSearchOnlyAncestor(t *testing.T) {
	// Test the effective kernel boundary below, not the numeric UID: container
	// CI can run as root with both DAC bypass capabilities irreversibly removed.
	for _, fixture := range loadInstallPermissionsFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			outer := t.TempDir()
			search := filepath.Join(outer, "search-only")
			anchor := filepath.Join(search, "data")
			if err := os.MkdirAll(anchor, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(search, 0o111); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(search, 0o700); err != nil {
					t.Errorf("restore owned permission sandbox: %v", err)
				}
			})
			if file, err := os.Open(search); !errors.Is(err, fs.ErrPermission) {
				if file != nil {
					_ = file.Close()
				}
				t.Fatalf("permission boundary is ineffective for uid %d: %v", os.Geteuid(), err)
			}
			t.Logf("effective unprivileged permission boundary: uid=%d, unchanged ancestor mode=0111, existing data anchor mode=0700", os.Geteuid())
			dbPath := filepath.Join(anchor, "index.db")
			storetest.CopyGoldenTo(t, dbPath)
			db, err := store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			ctx := context.Background()
			pulls := filepath.Join(anchor, "pulls")
			if fixture.TrailingSeparator {
				pulls += string(filepath.Separator)
			}
			filesystem := pull.NewFileSystem(pulls)
			ref := newRef(t)
			audit := &installAuditStore{PullStore: db}
			if fixture.FailBarrier {
				failed := false
				_, err := pull.NewPipeline(happyReader(), filesystem, audit, testutil.NewFixedClock(), testCreds(), pulls, pull.WithInstallSync(func(path string) error {
					if path == anchor {
						failed = true
						return errInstallInjected
					}
					return filesystem.Sync(path)
				})).PullTranscript(ctx, ref, pull.PullOptions{})
				if !errors.Is(err, errInstallInjected) || !failed || audit.commits != 0 {
					t.Fatalf("required data-anchor barrier did not guard index: %v", err)
				}
			}
			if _, err := pull.NewPipeline(happyReader(), filesystem, audit, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{}); err != nil {
				t.Fatalf("normal pull through search-only ancestor: %v", err)
			}
			reader := happyReader()
			if fixture.Recovery {
				reader = replacementReader()
				fault := &installFaultStore{PullStore: db}
				if _, err := pull.NewPipeline(reader, filesystem, fault, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{Force: true}); !errors.Is(err, errInstallInjected) {
					t.Fatalf("leave actual pending index projection: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = store.Open(dbPath, store.WithSkipMigrations())
				if err != nil {
					t.Fatal(err)
				}
			}
			retry := happyReader()
			retry.Meta = reader.Meta
			retry.ContentErr = errors.New("hash reuse/recovery must not download content")
			retry.AnnotationsErr = errors.New("hash reuse/recovery must not transfer annotations")
			if _, err := pull.NewPipeline(retry, filesystem, db, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{}); err != nil {
				t.Fatalf("reuse/recovery through search-only ancestor: %v", err)
			}
			if retry.ContentCalls != 0 || retry.AnnotationsCalls != 0 {
				t.Fatal("remote transfers masked local reuse/recovery")
			}
			assertInstalledContent(t, db, filepath.Join(pulls, testutil.TestVillageHost, ref.ID.String()), reader)
		})
	}
}
