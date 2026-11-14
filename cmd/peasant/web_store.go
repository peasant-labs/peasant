package main

import (
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// webStoreTarget is the store a web server opens: where it is and the
// options it is opened with. `peasant web start` resolves it once in the
// parent and the server child resolves it the same way, so the parent
// upgrades and pre-opens exactly what the child will open.
type webStoreTarget struct {
	dataDir string
	dbPath  string
	// options are the managed-generation options rooted at the configured
	// output base path. optionsErr is why they could not be built; the
	// store is then opened without them.
	options    []store.OpenOption
	optionsErr error
}

// resolveWebStoreTarget names the store a web server started by cmd with
// cfg opens. cfg must already carry the --data-dir override.
func resolveWebStoreTarget(cmd *cobra.Command, cfg *config.Config) webStoreTarget {
	target := webStoreTarget{
		dataDir: string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))),
		dbPath:  string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))),
	}
	if cfg == nil {
		return target
	}
	target.options, target.optionsErr = generationStoreOptions(cfg.Output.BasePath)
	if target.optionsErr != nil {
		target.options = nil
	}
	return target
}

// webStartStoreTarget is the target the `peasant web start` parent prepares:
// it loads the browsing configuration the server child loads and resolves
// the target from it. A configuration that does not load leaves the options
// out; the child reports the configuration error itself.
func webStartStoreTarget(cmd *cobra.Command, cfgPath string) webStoreTarget {
	cfg, err := config.LoadForBrowsing(cfgPath, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{})
	if err != nil {
		return resolveWebStoreTarget(cmd, nil)
	}
	applyDataDirOverride(cmd, cfg)
	return resolveWebStoreTarget(cmd, cfg)
}

// openWebStore opens a web store target. It is a variable so a test can see
// that the parent pre-open and the server child open the same target.
var openWebStore = func(target webStoreTarget) (*store.Store, error) {
	if err := os.MkdirAll(target.dataDir, defaults.PrivateDirPerm); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", target.dataDir, err)
	}
	return store.Open(target.dbPath, target.options...)
}

// prepareStoreForWebStart runs in the `peasant web start` parent, before it
// forks the server. The parent is where a terminal can show an upgrade, so
// it upgrades a behind store under the entry-point policy; a refusal ends
// the start before any server is forked. It then opens and closes the store
// once, so a new store is created here and not inside the child's short
// health window. A failed pre-open only warns: the child reports a store it
// cannot open the same way it always has.
func prepareStoreForWebStart(cmd *cobra.Command, target webStoreTarget, stderr io.Writer) error {
	if err := ensureStoreCurrent(cmd, target.dbPath, storeEntryModeFor(cmd)); err != nil {
		return err
	}
	db, err := openWebStore(target)
	if err != nil {
		fmt.Fprintf(stderr, "warning: pre-open analytics store %s: %v\n", target.dbPath, err)
		return nil
	}
	_ = db.Close()
	return nil
}
