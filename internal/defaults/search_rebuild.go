package defaults

// SearchRebuildHashBytes is the in-memory postings budget of a whole
// search index rebuild: the FTS5 `hashsize` option the rebuild sets on the
// union search index for its own run and removes before it commits. FTS5
// writes its pending postings to a new segment each time they pass the
// budget, and merges those segments together again while the rebuild runs;
// at FTS5's default of 1 MiB a large index is written and merged many times,
// and the rebuild grows faster than the text. The postings, and so every
// search result and rank, do not depend on the budget.
//
// Derivation: sandbox measurement of the one-statement rebuild on synthetic
// stores whose text is in the bodies (2026-11-19, revision b6a9ea23c, a
// shared 32-core machine at load average 7 to 72; median of three runs, the
// range in brackets; the 16 MiB budget was not run at 2048 MiB):
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> PEASANT_SANDBOX_REBUILD_TEXT_MB=128,256,512,1024,2048 \
//	  go test -tags upgrade_measure ./internal/store -run TestSandboxSearchRebuildMeasure -v -timeout 0
//
//	indexed text   search index   1 MiB (FTS5 default)   16 MiB              64 MiB
//	  131 MB          84 MB         22.2 s [21.9-24.2]     10.8 s [9.8-13.4]    15.5 s [10.9-18.6]
//	  260 MB         164 MB         59.8 s [56.9-73.7]     18.0 s [14.5-19.2]   15.1 s [10.7-16.2]
//	  524 MB         327 MB        103.9 s [92.9-117]      31.1 s [24.4-36.8]   23.4 s [19.6-24.7]
//	 1049 MB         647 MB        279 s [236-282]         85.8 s [62.5-108]    65.9 s [51.9-127]
//	 2097 MB        1290 MB        692 s [578-776]         -                    147 s [130-196]
//
// Every variant returned the same rows and bm25 ranks for the fixture
// queries. 64 MiB is the measured best: 4.7 times faster than the default at
// 2.1 GB. It bounds the rebuild's postings buffer, held once, by the one
// connection that rebuilds. A larger budget was not measured.
const SearchRebuildHashBytes = 64 << 20
