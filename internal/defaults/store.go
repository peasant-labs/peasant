package defaults

import (
	"os"
	"path/filepath"
)

// DataDirPath is a typed XDG data directory path.
type DataDirPath string

func (p DataDirPath) String() string { return string(p) }

// DBFilePath is a typed database file path.
type DBFilePath string

func (p DBFilePath) String() string { return string(p) }

type storeDefaults struct {
	DataDirPath         DataDirPath
	DBFilePath          DBFilePath
	VillagePullsDirPath VillagePullsDirPath
}

// Data holds data-directory defaults, resolved from XDG_DATA_HOME or HOME.
// These values are computed once at package init time; use ResolveDataDirPath()
// and ResolveDBFilePath() when the environment may have changed (e.g. in tests).
var Data = newStoreDefaults()

func newStoreDefaults() storeDefaults {
	dataDir := ResolveDataDirPath()
	return storeDefaults{
		DataDirPath:         dataDir,
		DBFilePath:          DBFilePath(filepath.Join(string(dataDir), "peasant.db")),
		VillagePullsDirPath: VillagePullsDirPath(filepath.Join(string(dataDir), VillagePullsSubdir)),
	}
}

// ResolveDataDirPath returns the data directory path, resolved from the
// environment at call time. This supports t.Setenv in tests where XDG_DATA_HOME
// is set after package init.
func ResolveDataDirPath() DataDirPath {
	if dataHome := os.Getenv(EnvXDGDataHome.String()); dataHome != "" {
		return DataDirPath(filepath.Join(dataHome, string(AppName)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		return DataDirPath(filepath.Join(home, ".local", "share", string(AppName)))
	}
	return DataDirPath(filepath.Join(os.Getenv("HOME"), ".local", "share", string(AppName)))
}

// ResolveDBFilePath returns the database file path, resolved from the
// environment at call time.
func ResolveDBFilePath() DBFilePath {
	return DBFilePath(filepath.Join(string(ResolveDataDirPath()), "peasant.db"))
}

// ResolveDataDirPathWith resolves the data directory, preferring an explicit
// XDG_DATA_HOME override (e.g. from the --data-dir flag) over the process
// environment. This is the parallel-safe path: callers thread the override
// down explicitly instead of mutating process-global env (t.Setenv). An empty
// override falls back to ResolveDataDirPath() (env / home).
func ResolveDataDirPathWith(xdgDataHomeOverride string) DataDirPath {
	if xdgDataHomeOverride != "" {
		return DataDirPath(filepath.Join(xdgDataHomeOverride, string(AppName)))
	}
	return ResolveDataDirPath()
}

// ResolveDBFilePathWith returns the database file path under the resolved data
// directory, preferring the given XDG_DATA_HOME override. See ResolveDataDirPathWith.
func ResolveDBFilePathWith(xdgDataHomeOverride string) DBFilePath {
	return DBFilePath(filepath.Join(string(ResolveDataDirPathWith(xdgDataHomeOverride)), "peasant.db"))
}

// ResolveOutputBasePath returns the default base directory for ingested
// transcripts (peasant-sync), resolved from the environment at call time. Like
// ResolveDBFilePath, it nests under ResolveDataDirPath(), so it HONORS
// XDG_DATA_HOME — setting XDG_DATA_HOME fully sandboxes ingest output, matching
// the DB path (they no longer diverge). The DB lives at <data dir>/peasant.db and
// transcripts under <data dir>/peasant-sync.
func ResolveOutputBasePath() OutputPath {
	return OutputPath(filepath.Join(string(ResolveDataDirPath()), OutputSyncSubdir))
}

// VillagePullsSubdir is the leaf directory (under the resolved data dir) that
// holds transcripts PULLED from a village. It is a SEPARATE namespace from
// OutputSyncSubdir (peasant-sync, the ingest output): pulled transcripts are
// foreign and one-way — they never feed ingest/analytics or the annotate-push
// candidate set. The on-disk layout under it is
// {villageHost}/{transcriptId}/{transcript.jsonl,metadata.json,pull-manifest.json}.
const VillagePullsSubdir = "village-pulls"

// VillagePullsDirPath is a typed path to the village-pulls/ root directory.
type VillagePullsDirPath string

func (p VillagePullsDirPath) String() string { return string(p) }

// ResolveVillagePullsDirPath returns the village-pulls/ root directory, resolved
// from the environment at call time. Like ResolveOutputBasePath it nests under
// ResolveDataDirPath(), so setting XDG_DATA_HOME fully sandboxes pulled output.
func ResolveVillagePullsDirPath() VillagePullsDirPath {
	return VillagePullsDirPath(filepath.Join(string(ResolveDataDirPath()), VillagePullsSubdir))
}

// ResolveVillagePullsDirPathWith resolves the village-pulls/ root directory,
// preferring an explicit XDG_DATA_HOME override (e.g. from the --data-dir flag)
// over the process environment. This is the parallel-safe path: callers thread
// the override down explicitly instead of mutating process-global env. An empty
// override falls back to ResolveVillagePullsDirPath() (env / home).
func ResolveVillagePullsDirPathWith(xdgDataHomeOverride string) VillagePullsDirPath {
	return VillagePullsDirPath(filepath.Join(string(ResolveDataDirPathWith(xdgDataHomeOverride)), VillagePullsSubdir))
}
