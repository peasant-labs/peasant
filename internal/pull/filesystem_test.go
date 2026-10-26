package pull_test

import (
	"context"
	"io/fs"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/schema"
)

// The legacy MemFS moves only directory nodes. This dependency adapter supplies
// directory-rename semantics for its existing unit tests; durability tests use
// the real OS filesystem and real cross-process locks instead.
type treeRenameFS struct{ ingest.FileSystem }

var _ ingest.FileSystem = treeRenameFS{}

func (f treeRenameFS) Rename(src, dst string) error {
	info, err := f.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return f.FileSystem.Rename(src, dst)
	}
	if err := f.MkdirAll(dst, defaults.PrivateDirPerm); err != nil {
		return err
	}
	err = f.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return f.MkdirAll(target, defaults.PrivateDirPerm)
		}
		return f.CopyFile(path, target, defaults.PrivateFilePerm)
	})
	if err != nil {
		return err
	}
	return f.RemoveAll(src)
}

func newPipeline(reader pull.VillageReader, filesystem ingest.FileSystem, st pull.PullStore, clock pull.Clock, creds pull.Credentials, root string) *pull.Pipeline {
	return pull.NewPipeline(reader, treeRenameFS{filesystem}, st, clock, creds, root,
		pull.WithInstallSync(func(string) error { return nil }),
		pull.WithInstallLock(func(context.Context, string, schema.TranscriptID) (func() error, error) {
			return func() error { return nil }, nil
		}))
}
