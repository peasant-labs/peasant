//go:build e2e

// Local end-to-end harness for transcript + annotation
// skip-gate + retraction, driven through the REAL peasant CLI (subprocesses) and
// a REAL village server, with ephemeral podman Postgres + MinIO. See fixture.go
// for the package doc; docs/e2e.md for how to run.
//
// Flow: ingest the committed fixtures — claude root+subagent, two codex rollouts,
// and two cursor sessions (ExpectedTranscriptCount in fixture.go) → annotate
// create K system annotations → push#1 (ExpectedPushTranscriptCount model-bearing
// transcripts + K annotations; cursor sessions omit message.model and hit
// ErrNoModel) → push#2 (manifest skip) → supersede one locally
// → push (retraction) → a DIRECT publish with an unknown harness → 422.
// Every peasant subprocess runs with XDG_{DATA,CONFIG,STATE}_HOME
// under a throwaway SANDBOX rooted at the resolved XDG_STATE_HOME; the real data
// dir is asserted untouched.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	postgresImage = "docker.io/library/postgres:16-alpine"
	minioImage    = "docker.io/minio/minio:latest"

	pgUser, pgPassword, pgDatabase = "peasant", "peasant", "peasant"

	strongJWTSecret = "e2e-skipgate-harness-jwt-secret-please-ignore-0123456789abcdef0123"

	// systemAnnotationType is a system-origin annotation type (→ pushed) with a
	// known domain-valid value.
	systemAnnotationType = "quality.session_approval"
	systemAnnotationVal  = "approve"

	startupTimeout = 120 * time.Second
	pollInterval   = 300 * time.Millisecond
)

func TestSkipGateE2E(t *testing.T) {
	runSkipGateHarness(t, harnessOptions{assert: true})
}

func runSkipGateHarness(t *testing.T, opts harnessOptions) {
	t.Helper()
	bins := resolveVillageBinaries(t)
	peasantBin := buildPeasant(t)

	// SANDBOX rooted at the resolved (ambient) XDG_STATE_HOME. We never t.Setenv
	// the XDG vars in-process, so ResolveStateDirPath/DataDirPath keep returning
	// the REAL ambient locations throughout — only subprocesses are redirected.
	realStateDir := string(defaults.ResolveStateDirPath())
	realDataDir := string(defaults.ResolveDataDirPath())
	if err := pruneStaleSandboxes(realStateDir); err != nil {
		t.Fatalf("prune stale sandboxes: %v", err)
	}
	sandbox := computeSandbox(realStateDir, time.Now().UnixNano())
	dataHome, configHome, stateHome := sandboxXDG(sandbox)
	for _, d := range []string{dataHome, configHome, stateHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir sandbox dir %s: %v", d, err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(sandbox) })
	realBefore := snapshotRealDataDir(realDataDir)

	// Register the sandbox-isolation assertion as a cleanup (not a trailing call)
	// so it runs on EVERY exit path — including a t.Skipf from a later skip-capable
	// phase (villageScanPhase / unknownHarnessPhase). The harness must never be
	// able to touch the real data dir and skip the proof that it didn't. Registered
	// after the sandbox-removal cleanup so it runs BEFORE the sandbox is torn down.
	t.Cleanup(func() { assertRealDataDirUnchanged(t, opts, realDataDir, realBefore) })

	xdgEnv := xdgEnvAssignments(dataHome, configHome, stateHome)

	// Ephemeral infra, unless the caller injected a complete external stack.
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Logf("%s: DATABASE_URL, S3_ENDPOINT, S3_BUCKET, and VILLAGE_URL are injected; self-provisioning is disabled", externalStackEngagedMarker)
	}
	apiKey := mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, configHome)

	// Sandbox config: claude-code, codex, and cursor enabled, each pointed at its
	// committed fixture; opencode disabled; output.basePath under the SANDBOX. This
	// is required because (a) --source-provider/--source-path overrides ONLY the named
	// provider's path while leaving the others enabled at their real defaults, and
	// (b) the default output.basePath is a hardcoded ~/.local/share path that does
	// NOT follow XDG_DATA_HOME — so without this, ingest would read the real
	// ~/.claude + ~/.codex + ~/.cursor dirs and write transcripts into the real data
	// dir. Enabling all three fixtures makes push#1 a mixed multi-harness batch.
	writeSandboxConfig(t, configHome, dataHome)

	// Seed: ingest the fixtures (config-scoped to claude-code + codex + cursor) →
	// each ingested session. --include-active is REQUIRED for determinism: ingest
	// classifies any source modified within the 60s staleness threshold as "active"
	// and skips it, and a fresh `git checkout` (or any fixture edit) stamps the
	// committed fixture files with a current mtime — so without this flag the run
	// is mtime-dependent (e.g. a just-touched codex fixture would be skipped,
	// yielding 2 transcripts instead of 3).
	runPeasant(t, peasantBin, xdgEnv, "ingest", "--include-active")

	dbPath := filepath.Join(dataHome, string(defaults.AppName), "peasant.db")
	assertDBUnderSandbox(t, opts, sandbox, dbPath)
	assertStateInvariant(t, opts, realStateDir)

	sessionIDs := readSessionIDs(t, dbPath)
	check(t, opts, len(sessionIDs) == ExpectedTranscriptCount,
		"ingested %d sessions, want %d (see Expected*Transcripts in fixture.go)", len(sessionIDs), ExpectedTranscriptCount)
	assertAllSessionsHaveMetrics(t, opts, dbPath)
	for _, sid := range sessionIDs {
		runPeasant(t, peasantBin, xdgEnv, "annotate", "create", sid, systemAnnotationType, systemAnnotationVal)
	}
	if !stack.external {
		assertSeededBaselineBeforePush(t, opts, stack)
	}

	// push #1: full mixed batch + K annotations (K derived from the CLI's report).
	p1 := runPeasantPush(t, peasantBin, xdgEnv)
	k := p1.Annotations.Created
	check(t, opts, p1.transcripts() == ExpectedPushTranscriptCount,
		"push#1 transcripts = %d, want %d\nsessions: %s", p1.transcripts(), ExpectedPushTranscriptCount, p1.sessionStatuses())
	assertCursorSessionsHeldForNoModel(t, opts, p1)
	check(t, opts, k > 0, "push#1 created %d annotations, want K>0", k)
	t.Logf("push#1: %d transcripts, K=%d annotations", p1.transcripts(), k)

	// push #2: 0 transcripts + 0 created (manifest skip), K skipped.
	p2 := runPeasantPush(t, peasantBin, xdgEnv)
	check(t, opts, p2.transcripts() == 0, "push#2 transcripts = %d, want 0", p2.transcripts())
	check(t, opts, p2.Annotations.Created == 0, "push#2 created = %d, want 0", p2.Annotations.Created)
	check(t, opts, p2.Annotations.Skipped == k, "push#2 skipped = %d, want K=%d", p2.Annotations.Skipped, k)

	// retraction: supersede exactly one local annotation → push.
	supersedeOneAnnotation(t, dbPath)
	p3 := runPeasantPush(t, peasantBin, xdgEnv)
	check(t, opts, p3.Annotations.Retracted == 1, "retraction push: retracted = %d, want 1", p3.Annotations.Retracted)
	t.Logf("retraction push: retracted=%d", p3.Annotations.Retracted)

	// Content round-trip: GET /content renders a bare SessionDetailPayload (turns +
	// snippet survived migrate-on-read), NOT the legacy fallback, for both the
	// real-pushed current-format transcript and production-encrypted legacy
	// fixtures carrying only the pre-envelope version marker.
	contentRenderPhase(t, opts, stack, apiKey, configHome)

	// Village-scan: a DIRECT publish with a planted secret must be rejected 422
	// (server-side redaction gate), proving the village scans independently of the
	// client. NOT via peasant push (client redaction would make it vacuous).
	villageScanPhase(t, opts, stack.villageURL, apiKey, bins.backendDir)

	// Unknown-harness guard: a DIRECT publish whose metadata carries an unknown
	// harness must be rejected as the schema enum 422 path from the peasant side.
	unknownHarnessPhase(t, opts, stack.villageURL, apiKey, bins.backendDir)

	// Required-fields guard: a direct publish whose metadata omits a required
	// model field (harness or model) must trigger the documented schema-422 from
	// the schema module's publish contract. This proves the village-side rejection
	// independently of the peasant client (which never emits such a body from valid
	// fixtures; client-side reject is covered by
	// TestPipeline_InvalidBody_RefusedNotUploaded).
	requiredFieldsPhase(t, opts, stack.villageURL, apiKey, bins.backendDir)
	assertVillageHealth(t, opts, stack.villageURL, "after full skip-gate flow")

	// NOTE: the sandbox-isolation assertion (assertRealDataDirUnchanged) is
	// registered as a t.Cleanup above so it runs even if a phase above t.Skipf'd.
}

// --- skip / dependency resolution ---

func requirePodman(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("e2e: podman not found on PATH — install podman to run the local end-to-end harness")
	}
}

func resolveVillageBinaries(t *testing.T) villageBinaries {
	t.Helper()
	if sb, db := getenv(envVillageBin), getenv(envSetupDemoBin); sb != "" && db != "" {
		backend, ok, searched := resolveVillageBackend(t)
		if !ok {
			t.Skipf("e2e: village backend not found; schema parity and village contract fixtures require VILLAGE_REPO or VILLAGE_BACKEND_DIR even with prebuilt binaries (searched: %s)", strings.Join(searched, ", "))
		}
		assertSchemaModuleParity(t, backend)
		return villageBinaries{server: sb, setupDemo: db, backendDir: backend}
	} else if sb != "" || db != "" {
		t.Skipf("e2e: set both VILLAGE_BIN and SETUP_DEMO_BIN, or neither (got VILLAGE_BIN=%t SETUP_DEMO_BIN=%t)", sb != "", db != "")
	}

	backend, ok, searched := resolveVillageBackend(t)
	if !ok {
		t.Skipf("e2e: village backend not found. Set VILLAGE_REPO to a village checkout, VILLAGE_BACKEND_DIR to its backend dir, or VILLAGE_BIN+SETUP_DEMO_BIN to prebuilt binaries. Searched: %s", strings.Join(searched, ", "))
	}
	assertSchemaModuleParity(t, backend)
	outDir := t.TempDir()
	return villageBinaries{
		server:     buildVillageBinary(t, backend, "./cmd/server", filepath.Join(outDir, "village-server")),
		setupDemo:  buildVillageBinary(t, backend, "./cmd/village-setup-demo", filepath.Join(outDir, "village-setup-demo")),
		backendDir: backend,
	}
}

func resolveVillageBackend(t *testing.T) (backend string, ok bool, searched []string) {
	t.Helper()
	if backend := getenv(envVillageBackendDir); backend != "" {
		if isVillageBackend(backend) {
			return backend, true, nil
		}
		t.Skipf("e2e: VILLAGE_BACKEND_DIR=%s is not a village backend dir with go.mod", backend)
	}
	if repo := getenv(envVillageRepo); repo != "" {
		backend := filepath.Join(repo, "backend")
		if isVillageBackend(backend) {
			return backend, true, nil
		}
		t.Skipf("e2e: VILLAGE_REPO=%s does not contain backend/go.mod", repo)
	}

	candidates := villageBackendCandidates(t)
	for _, candidate := range candidates {
		if isVillageBackend(candidate) {
			return candidate, true, candidates
		}
	}
	return "", false, candidates
}

func isVillageBackend(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		return false
	}
	return true
}

func villageBackendCandidates(t *testing.T) []string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	root := peasantRepoRoot(wd)
	if root == "" {
		root = wd
	}

	repoNames := []string{
		"village",
		filepath.Join("village", "develop"),
		filepath.Join("peasant-labs", "village"),
		filepath.Join("peasant-labs", "village", "develop"),
	}
	seen := make(map[string]bool)
	var candidates []string
	for base, depth := root, 0; depth < 6; base, depth = filepath.Dir(base), depth+1 {
		for _, repoName := range repoNames {
			backend := filepath.Join(base, repoName, "backend")
			if !seen[backend] {
				seen[backend] = true
				candidates = append(candidates, backend)
			}
		}
		if parent := filepath.Dir(base); parent == base {
			break
		}
	}
	return candidates
}

func buildVillageBinary(t *testing.T, backendDir, pkg, out string) string {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = backendDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("e2e: `go build %s` in %s failed (village half not ready?): %v\n%s", pkg, backendDir, err, output)
	}
	return out
}

// --- stack provisioning seams ---

func provisionHarnessStack(t *testing.T, bins villageBinaries) harnessStack {
	t.Helper()
	external, err := externalStackFromEnv()
	if err != nil {
		t.Fatalf("e2e: external-stack validation failed\n"+
			"what: DATABASE_URL, S3_ENDPOINT, S3_BUCKET, and VILLAGE_URL were not usable as an injected stack\n"+
			"why: %v\n"+
			"where: internal/e2e/skipgate_e2e_test.go provisionHarnessStack\n"+
			"when: resolving harness infrastructure before provisioning\n"+
			"means: the harness cannot safely decide whether to reuse a warm stack or provision its own\n"+
			"fix: set all four variables to usable values, or unset all four to let the harness self-provision", err)
	}
	if external.engaged {
		validateExternalStackUsable(t, external)
		return harnessStack{
			dsn:           external.dsn,
			minioEndpoint: external.minioEndpoint,
			bucket:        external.bucket,
			villageURL:    external.villageURL,
			external:      true,
		}
	}

	requirePodman(t)
	reapStaleE2EInfra(t)
	bucket := uniqueName("transcripts")
	dsn, db := startEphemeralPostgres(t)
	minioEndpoint := startEphemeralMinIO(t, bucket)
	village := startVillageProcess(t, bins.server, dsn, minioEndpoint, bucket)
	return harnessStack{
		dsn:           dsn,
		db:            db,
		minioEndpoint: minioEndpoint,
		bucket:        bucket,
		villageURL:    village.url,
		village:       village,
	}
}

func validateExternalStackUsable(t *testing.T, cfg externalStackConfig) {
	t.Helper()
	hc := &http.Client{Timeout: 2 * time.Second}
	waitReady(t, "external village", func() bool {
		resp, err := hc.Get(cfg.villageURL + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	})
	waitReady(t, "external minio", func() bool {
		resp, err := hc.Get(cfg.minioEndpoint + "/minio/health/live")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	client, err := newMinioClient(cfg.minioEndpoint)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "external stack bucket preflight failed",
			what:  "S3_ENDPOINT could not be converted into a MinIO client",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go provisionExternalStack",
			when:  "checking injected external stack before running the harness",
			means: "the harness cannot prove S3_BUCKET belongs to the injected stack",
			fix:   "set S3_ENDPOINT to http(s)://host:port for the injected MinIO endpoint",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	if failure := externalStackBucketPreflight(ctx, client, cfg.bucket); failure != nil {
		fatalActionable(t, *failure)
	}
}

// --- ephemeral Postgres ---

// startEphemeralPostgres runs Postgres on a random port, waits for readiness, and
// returns the connection DSN and an OPEN *sql.DB over the DSN. Container
// lifecycle is owned by this function's cleanup. The harness's in-database
// seed SQL runs through that database/sql handle (the pgx stdlib driver) with $1
// parameterized statements — never by shelling out to `podman exec … psql` with
// interpolated SQL. Readiness is gated twice — in-container pg_isready, then a
// polled host-side db.Ping past the port-forward race — and the handle is Closed
// via t.Cleanup (registered here, so it closes before the container is removed).
func startEphemeralPostgres(t *testing.T) (dsn string, db *sql.DB) {
	t.Helper()
	name := uniqueName("pg")
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-fv", name).Run() })
	args := []string{"run", "-d", "--name", name,
		"-e", "POSTGRES_USER=" + pgUser, "-e", "POSTGRES_PASSWORD=" + pgPassword,
		"-e", "POSTGRES_DB=" + pgDatabase, "-p", "127.0.0.1::5432", postgresImage}
	if out, err := exec.Command("podman", args...).CombinedOutput(); err != nil {
		t.Skipf("e2e: `podman run %s` failed (image pull/network?): %v\n%s", postgresImage, err, out)
	}
	port := readMappedPort(t, name, "5432/tcp")
	waitReady(t, "postgres", func() bool {
		return exec.Command("podman", "exec", name, "pg_isready", "-U", pgUser, "-d", pgDatabase).Run() == nil
	})
	dsn = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable", pgUser, pgPassword, port, pgDatabase)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "open ephemeral Postgres failed",
			what:  "database/sql could not create a pgx-backed handle for the harness database",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go startEphemeralPostgres",
			when:  "after the Postgres container became ready and its host port was resolved",
			means: "the harness cannot seed, refresh, or inspect Village state",
			fix:   "confirm the pgx stdlib blank import is present and the generated Postgres DSN is valid",
		})
	}
	t.Cleanup(func() { _ = db.Close() })
	// The in-container pg_isready gate above proves the server is up INSIDE the
	// container's netns, but not that the mapped host port accepts a real SQL
	// connection yet: the FIRST host->container connect can land before podman's
	// port-forward proxy is serving and get reset (a CI-only race the old
	// `podman exec psql` path never hit because it ran in-container). Poll
	// db.Ping() past that — database/sql discards the reset conn and retries a
	// fresh one each tick — so this is the second, host-connection readiness gate.
	waitReady(t, "postgres (host connection)", func() bool { return db.Ping() == nil })
	return dsn, db
}

// --- ephemeral MinIO (S3) ---

// startEphemeralMinIO runs MinIO on a random port, waits for health, creates the
// transcript bucket via the in-process minio-go client, and health-gates the
// bucket on a real S3 op (BucketExists). Returns the S3 endpoint URL.
func startEphemeralMinIO(t *testing.T, bucket string) string {
	t.Helper()
	bucket = requireTranscriptBucket(t, bucket)
	name := uniqueName("minio")
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-fv", name).Run() })
	args := []string{"run", "-d", "--name", name,
		"-e", "MINIO_ROOT_USER=" + minioUser, "-e", "MINIO_ROOT_PASSWORD=" + minioPassword,
		"-p", "127.0.0.1::9000", minioImage, "server", "/data"}
	if out, err := exec.Command("podman", args...).CombinedOutput(); err != nil {
		t.Skipf("e2e: `podman run %s` failed (image pull/network?): %v\n%s", minioImage, err, out)
	}
	port := readMappedPort(t, name, "9000/tcp")
	endpoint := "http://127.0.0.1:" + port

	hc := &http.Client{Timeout: 2 * time.Second}
	waitReady(t, "minio", func() bool {
		resp, err := hc.Get(endpoint + "/minio/health/live")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	if err := startEphemeralMinIOBucket(t, endpoint, bucket); err != nil {
		t.Fatalf("e2e: provision transcript bucket on %s: %v", endpoint, err)
	}
	return endpoint
}

func reapStaleE2EInfra(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		return
	}
	// stdout-only (Output, not CombinedOutput): the parse below keys on a tab +
	// the peasant-e2e- prefix, so podman's cgroup/warning stderr lines cannot be
	// misread as container rows. This mirrors the typed-API principle behind the
	// minio-go S3 migration — never let a quantity/identity decision read stderr.
	out, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}\t{{.Status}}").Output()
	if err != nil {
		t.Logf("e2e: could not list stale podman infrastructure before self-provisioning: %v\n%s", err, out)
		return
	}
	names := staleStoppedE2EInfraNames(string(out), time.Now(), staleE2ETTL)
	args := podmanReapE2EInfraArgs(names)
	if len(args) == 0 {
		return
	}
	if out, err := exec.Command("podman", args...).CombinedOutput(); err != nil {
		t.Fatalf("e2e: failed to reap stale podman infrastructure\nwhat: podman rm -fv failed for stale containers %s\nwhy: %v\nwhere: internal/e2e/skipgate_e2e_test.go reapStaleE2EInfra\nwhen: before self-provisioning a new harness stack\nmeans: stale DB/S3/village state could contaminate this run\nfix: inspect or manually remove stale peasant-e2e-* podman containers\noutput:\n%s",
			strings.Join(names, ", "), err, out)
	}
}

// --- village server subprocess ---

func startVillageServer(t *testing.T, serverBin, dsn, s3Endpoint, bucket string) string {
	t.Helper()
	return startVillageProcess(t, serverBin, dsn, s3Endpoint, bucket).url
}

func assertVillageHealth(t *testing.T, opts harnessOptions, villageURL, when string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(villageURL + "/health")
	if err != nil {
		check(t, opts, false, "village health failed %s: %v", when, err)
		return
	}
	defer resp.Body.Close()
	check(t, opts, resp.StatusCode >= 200 && resp.StatusCode < 300,
		"village health status %s = %d, want 2xx", when, resp.StatusCode)
}

func startVillageProcess(t *testing.T, serverBin, dsn, s3Endpoint, bucket string) *villageProcess {
	t.Helper()
	bucket = requireTranscriptBucket(t, bucket)
	port := freePort(t)
	villageURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command(serverBin)
	cmd.Dir = t.TempDir() // so godotenv ../.env finds nothing
	cmd.Env = append(os.Environ(),
		envAssignment(envDatabaseURL, dsn),
		"JWT_SECRET="+strongJWTSecret,
		fmt.Sprintf("PORT=%d", port),
		envAssignment(envS3Endpoint, s3Endpoint),
		envAssignment(envS3Bucket, bucket),
		"S3_ACCESS_KEY="+minioUser,
		"S3_SECRET_KEY="+minioPassword,
		"S3_USE_PATH_STYLE=true",
	)
	cmd.Env = append(cmd.Env, villageEncryptionEnvAssignments()...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	lockVillageLaunchThread()
	setVillageProcDeath(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("e2e: start village server: %v", err)
	}
	proc := &villageProcess{bin: serverBin, dsn: dsn, s3Endpoint: s3Endpoint, bucket: bucket, url: villageURL, cmd: cmd}
	t.Cleanup(func() { proc.stop() })
	hc := &http.Client{Timeout: 2 * time.Second}
	waitReady(t, "village", func() bool {
		resp, err := hc.Get(villageURL + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	})
	return proc
}

func (p *villageProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	stopVillageProcess(p.cmd)
	p.cmd = nil
}

func (s *harnessStack) refresh(t *testing.T, bins villageBinaries, configHome string) string {
	t.Helper()
	s.requireRefreshable(t)
	s.village.stop()
	truncateVillageDatabase(t, s.db)
	clearTranscriptBucket(t, s.minioEndpoint, s.bucket)
	s.village = startVillageProcess(t, bins.server, s.dsn, s.minioEndpoint, s.bucket)
	s.villageURL = s.village.url
	apiKey := mintDemoCredentials(t, bins.setupDemo, s.dsn, s.villageURL, configHome)
	assertSeededBaselineBeforePush(t, harnessOptions{assert: true}, *s)
	return apiKey
}

func truncateVillageDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	psqlExec(t, db, `
		DO $$
		DECLARE
			mutable_tables text[] := ARRAY[
				'users', 'groups', 'group_members', 'transcripts', 'tags',
				'transcript_tags', 'transcript_shares', 'api_keys',
				'cli_auth_sessions', 'user_github_orgs', 'attestations',
				'annotations', 'transcript_commits', 'transcript_associations', 'github_app_installations',
				'collective_repositories', 'repository_commits',
				'transcript_governance_events_audit'
			];
			preserved_tables text[] := ARRAY[
				'schema_migrations', 'licenses', 'governance_event_types'
			];
			stmt text;
			unexpected text;
		BEGIN
			SELECT string_agg(format('%I.%I', schemaname, tablename), ', ' ORDER BY tablename)
			INTO unexpected
			FROM pg_tables
			WHERE schemaname = 'public'
			  AND NOT (tablename = ANY (mutable_tables))
			  AND NOT (tablename = ANY (preserved_tables));
			IF unexpected IS NOT NULL THEN
				RAISE EXCEPTION 'unclassified public table(s) during E2E refresh: %. Classify each as mutable or migration-owned before retrying', unexpected;
			END IF;

			SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE'
			INTO stmt
			FROM pg_tables
			WHERE schemaname = 'public'
			  AND tablename = ANY (mutable_tables);
			IF stmt IS NOT NULL THEN
				EXECUTE stmt;
			END IF;
		END $$;`)
}

func assertSeededBaselineBeforePush(t *testing.T, opts harnessOptions, stack harnessStack) {
	t.Helper()
	got := seededBaselineCounts{
		transcripts: villageTableCount(t, stack.db, "transcripts"),
		annotations: villageTableCount(t, stack.db, "annotations"),
		s3Objects:   transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket),
	}
	if got.s3Objects != seededZeroContentBaselineBeforePush.s3Objects {
		dumpTranscriptBucketOnBaselineMismatch(t, stack)
	}
	assertSeededBaselineCounts(t, opts, got, seededZeroContentBaselineBeforePush)
}

// dumpTranscriptBucketOnBaselineMismatch is a LEAN, on-failure-only diagnostic: it
// runs ONLY when the seeded S3 baseline is non-zero, listing the bucket's actual
// object keys via the in-process minio-go client so a future CI failure is
// self-diagnosing from the log alone (the Blacksmith console is view-only) — with
// none of the always-on noise of the reverted temp DIAG. Because the keys come
// from typed ObjectInfo (not parsed CLI text), the dump itself cannot reintroduce
// the stderr-miscount it diagnoses.
func dumpTranscriptBucketOnBaselineMismatch(t *testing.T, stack harnessStack) {
	t.Helper()
	keys, err := listTranscriptBucketObjectKeys(stack.minioEndpoint, stack.bucket)
	if err != nil {
		t.Logf("e2e DIAG: seeded S3 baseline non-zero; could not list bucket %q on %q: %v",
			stack.bucket, stack.minioEndpoint, err)
		return
	}
	t.Logf("e2e DIAG: seeded S3 baseline non-zero; bucket %q on %q holds %d object(s): %v",
		stack.bucket, stack.minioEndpoint, len(keys), keys)
}

func villageTableCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	// A table name is an IDENTIFIER, which cannot be a $1 value placeholder
	// (placeholders bind VALUES, not identifiers). pgx.Identifier.Sanitize quotes it
	// safely for interpolation into the query text — the correct way to parameterize
	// an identifier; a value placeholder here would be a syntax error.
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", pgx.Identifier{table}.Sanitize())
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		fatalActionable(t, actionableFailure{
			title: "count Village table rows failed",
			what:  fmt.Sprintf("the harness could not count rows in public.%s using query %q", table, query),
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go villageTableCount",
			when:  "checking seeded, refreshed, or published Village database state",
			means: "the E2E cannot prove its database precondition or postcondition",
			fix:   "confirm the harness Postgres handle is open and the pinned Village migrations contain the expected table",
		})
	}
	return count
}

// mintDemoCredentials runs village-setup-demo --local, writes its credentials
// JSON into the SANDBOX config dir (the path peasant reads under XDG_CONFIG_HOME),
// and returns the minted API key (for the direct village-scan POST).
func mintDemoCredentials(t *testing.T, setupDemoBin, dsn, villageURL, configHome string) string {
	t.Helper()
	cmd := exec.Command(setupDemoBin, "--local")
	cmd.Env = append(os.Environ(), envAssignment(envDatabaseURL, dsn), envAssignment(envVillageURL, villageURL))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("e2e: village-setup-demo --local: %v", err)
	}
	var minted struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(out, &minted); err != nil || minted.APIKey == "" {
		t.Fatalf("e2e: parse village-setup-demo stdout (api_key): %v\n%s", err, out)
	}
	credDir := filepath.Join(configHome, string(defaults.AppName))
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "credentials.json"), out, 0o600); err != nil {
		t.Fatalf("e2e: write credentials.json: %v", err)
	}
	return minted.APIKey
}

// villageScanPhase issues DIRECT multipart publishes to the village (bypassing the
// peasant client's own redaction, which would make the check vacuous): a clean
// transcript_file → 2xx control; a transcript_file carrying a planted secret →
// 422 whose body is scanner.FormatScanErrors. Uses the village's own valid
// contract fixtures for schema-valid metadata + content.
func villageScanPhase(t *testing.T, opts harnessOptions, villageURL, apiKey, backendDir string) {
	t.Helper()
	fixtureDir := filepath.Join(backendDir, "internal", "handler", "testdata", "contract", "current", "valid")
	metadata, err := os.ReadFile(filepath.Join(fixtureDir, "metadata.json"))
	if err != nil {
		t.Skipf("e2e: village contract metadata fixture not found (%v) — skipping village-scan", err)
	}
	cleanContent, err := os.ReadFile(filepath.Join(fixtureDir, "content.json"))
	if err != nil {
		t.Skipf("e2e: village contract content fixture not found (%v) — skipping village-scan", err)
	}
	// Plant a high-confidence secret in the transcript body.
	dirtyContent := append(append([]byte(nil), cleanContent...),
		[]byte("\nleaked token: ghp_0123456789abcdef0123456789abcdef0123\n")...)

	// Control: clean publish → 2xx.
	status, _ := directPublish(t, villageURL, apiKey, metadata, cleanContent)
	check(t, opts, status >= 200 && status < 300, "village-scan control: clean publish status = %d, want 2xx", status)

	// Planted secret → 422 with the scanner's FormatScanErrors body.
	status, body := directPublish(t, villageURL, apiKey, metadata, dirtyContent)
	check(t, opts, status == http.StatusUnprocessableEntity,
		"village-scan: planted-secret publish status = %d, want 422\nbody: %s", status, body)
	check(t, opts, strings.Contains(body, "Redaction check failed. Potential secrets detected"),
		"village-scan: 422 body is not scanner.FormatScanErrors:\n%s", body)
	t.Logf("village-scan: planted secret → %d (FormatScanErrors); clean → 2xx", status)
}

// unknownHarnessPhase issues a DIRECT multipart publish whose metadata carries
// the shared publish-verdict corpus's unknown-harness value with otherwise-valid,
// CLEAN content. The expected schema-enforcement status comes from the
// publish-verdict corpus embedded in the github.com/peasant-labs/schema module.
func unknownHarnessPhase(t *testing.T, opts harnessOptions, villageURL, apiKey, backendDir string) {
	t.Helper()
	verdicts, err := schema.LoadPublishVerdictFixtures()
	if err != nil {
		t.Fatalf("e2e: load publish verdict fixtures: %v", err)
	}
	unknownHarness, ok := verdicts.CaseByName("unknown-harness")
	if !ok {
		t.Fatal("e2e: publish verdict fixtures missing unknown-harness row")
	}
	unknownHarnessValue, ok, err := unknownHarness.ModelHarness()
	if err != nil {
		t.Fatalf("e2e: parse unknown-harness verdict body: %v", err)
	}
	if !ok {
		t.Fatal("e2e: unknown-harness verdict row has no model.harness")
	}

	fixtureDir := filepath.Join(backendDir, "internal", "handler", "testdata", "contract", "current", "valid")
	metadataBytes, err := os.ReadFile(filepath.Join(fixtureDir, "metadata.json"))
	if err != nil {
		t.Skipf("e2e: village contract metadata fixture not found (%v) — skipping unknown-harness case", err)
	}
	cleanContent, err := os.ReadFile(filepath.Join(fixtureDir, "content.json"))
	if err != nil {
		t.Skipf("e2e: village contract content fixture not found (%v) — skipping unknown-harness case", err)
	}

	// The harness is nested under metadata.model.harness (see the village contract
	// fixture) — override it there, leaving every other field contract-valid so the
	// ONLY rejection cause is the unknown harness.
	var meta map[string]any
	if err := json.Unmarshal(metadataBytes, &meta); err != nil {
		t.Fatalf("e2e: parse village contract metadata.json: %v", err)
	}
	model, ok := meta["model"].(map[string]any)
	if !ok {
		t.Fatalf("e2e: village contract metadata.json missing object field \"model\" — cannot plant unknown harness (fixture shape changed?)")
	}
	model["harness"] = unknownHarnessValue
	meta["model"] = model
	dirtyMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("e2e: re-marshal metadata with unknown harness: %v", err)
	}

	status, body := directPublish(t, villageURL, apiKey, dirtyMeta, cleanContent)
	check(t, opts, status == unknownHarness.Expect.HTTPStatus,
		"unknown-harness publish status = %d, want %d\nbody: %s", status, unknownHarness.Expect.HTTPStatus, body)
	if unknownHarness.Expect.ErrorContains != "" {
		check(t, opts, strings.Contains(body, unknownHarness.Expect.ErrorContains),
			"unknown-harness %d body does not contain %q:\n%s", status, unknownHarness.Expect.ErrorContains, body)
	}
	t.Logf("unknown-harness: harness=%q → %d (%s)", unknownHarnessValue, status, unknownHarness.Expect.ErrorCategory)
}

// requiredFieldsPhase issues DIRECT multipart publishes whose metadata OMITS a
// required model field, proving the village rejects them with the documented
// schema-422 from the schema module's publish contract, independently of the
// peasant client (which cannot emit such a body from valid fixtures; the
// client-side reject is covered by
// TestPipeline_InvalidBody_RefusedNotUploaded). Expectations (HTTP status +
// error substring) are anchored to the shared publish-verdict corpus rows
// (absent-harness-key / absent-model-key), so the e2e and the schema corpus
// cannot drift.
func requiredFieldsPhase(t *testing.T, opts harnessOptions, villageURL, apiKey, backendDir string) {
	t.Helper()
	verdicts, err := schema.LoadPublishVerdictFixtures()
	if err != nil {
		t.Fatalf("e2e: load publish verdict fixtures: %v", err)
	}

	fixtureDir := filepath.Join(backendDir, "internal", "handler", "testdata", "contract", "current", "valid")
	metadataBytes, err := os.ReadFile(filepath.Join(fixtureDir, "metadata.json"))
	if err != nil {
		t.Skipf("e2e: village contract metadata fixture not found (%v) — skipping required-fields case", err)
	}
	cleanContent, err := os.ReadFile(filepath.Join(fixtureDir, "content.json"))
	if err != nil {
		t.Skipf("e2e: village contract content fixture not found (%v) — skipping required-fields case", err)
	}

	// Control: the unmodified, contract-valid metadata + clean content → 2xx, so a
	// rejection below is attributable ONLY to the omitted required field.
	status, body := directPublish(t, villageURL, apiKey, metadataBytes, cleanContent)
	check(t, opts, status >= 200 && status < 300,
		"required-fields control: valid publish status = %d, want 2xx\nbody: %s", status, body)

	// Each case omits exactly one required model field from the otherwise-valid
	// metadata; the village must answer the documented schema-422 whose body
	// carries the corpus-pinned santhosh message.
	for _, c := range []struct{ row, dropKey string }{
		{"absent-harness-key", "harness"},
		{"absent-model-key", "model"},
	} {
		vc, ok := verdicts.CaseByName(c.row)
		if !ok {
			t.Fatalf("e2e: publish verdict fixtures missing %s row", c.row)
		}
		dirtyMeta := dropModelField(t, metadataBytes, c.dropKey)
		status, body := directPublish(t, villageURL, apiKey, dirtyMeta, cleanContent)
		check(t, opts, status == vc.Expect.HTTPStatus,
			"%s publish status = %d, want %d\nbody: %s", c.row, status, vc.Expect.HTTPStatus, body)
		if vc.Expect.ErrorContains != "" {
			check(t, opts, strings.Contains(body, vc.Expect.ErrorContains),
				"%s %d body does not contain %q:\n%s", c.row, status, vc.Expect.ErrorContains, body)
		}
		t.Logf("required-fields: omit model.%s → %d (%s)", c.dropKey, status, vc.Expect.ErrorCategory)
	}
}

// dropModelField returns metadata with metadata.model.<key> removed, leaving every
// other field contract-valid so the ONLY rejection cause is the omitted key.
func dropModelField(t *testing.T, metadataBytes []byte, key string) []byte {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal(metadataBytes, &meta); err != nil {
		t.Fatalf("e2e: parse village contract metadata.json: %v", err)
	}
	model, ok := meta["model"].(map[string]any)
	if !ok {
		t.Fatalf("e2e: village contract metadata.json missing object field \"model\" (fixture shape changed?)")
	}
	delete(model, key)
	meta["model"] = model
	dirty, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("e2e: re-marshal metadata without model.%s: %v", key, err)
	}
	return dirty
}

// directPublish POSTs a multipart publish (metadata + transcript_file) with a
// Bearer key and returns the status + body.
func directPublish(t *testing.T, villageURL, apiKey string, metadata, content []byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mf, err := mw.CreateFormField("metadata")
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish multipart construction failed",
			what:  "the metadata form field could not be created",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "building a direct Village publish request",
			means: "the schema and server-side scan controls cannot be exercised",
			fix:   "inspect the multipart writer and retry with readable contract fixture bytes",
		})
	}
	if _, err := mf.Write(metadata); err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish multipart construction failed",
			what:  "the metadata fixture could not be written into the multipart request",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "serializing a direct Village publish request",
			means: "the request body is incomplete and cannot provide valid E2E evidence",
			fix:   "confirm the metadata fixture is readable and the in-memory multipart buffer is writable",
		})
	}
	ff, err := mw.CreateFormFile("transcript_file", "transcript.json")
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish multipart construction failed",
			what:  "the transcript_file form part could not be created",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "building a direct Village publish request",
			means: "Village cannot receive the transcript content fixture",
			fix:   "inspect the multipart writer and retry with readable contract fixture bytes",
		})
	}
	if _, err := ff.Write(content); err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish multipart construction failed",
			what:  "the transcript fixture could not be written into the multipart request",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "serializing a direct Village publish request",
			means: "the request body is incomplete and cannot provide valid E2E evidence",
			fix:   "confirm the content fixture is readable and the in-memory multipart buffer is writable",
		})
	}
	if err := mw.Close(); err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish multipart construction failed",
			what:  "the multipart request could not be finalized",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "closing the direct Village publish request body",
			means: "the terminal boundary is missing and Village would receive a malformed body",
			fix:   "inspect the multipart writer and retry after confirming all form parts were written successfully",
		})
	}

	req, err := http.NewRequest(http.MethodPost, villageURL+"/api/v1/transcripts/publish", &body)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish request construction failed",
			what:  "the Village publish HTTP request could not be created",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "after the multipart body was finalized",
			means: "the harness cannot exercise the real publish endpoint",
			fix:   "confirm VILLAGE_URL is a valid http(s) URL and retry",
		})
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish request failed",
			what:  "the multipart publish request did not receive a Village response",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "posting a contract fixture to /api/v1/transcripts/publish",
			means: "publish, scan, and refresh behavior cannot be evaluated",
			fix:   "confirm the harness-owned Village process is healthy and reachable at VILLAGE_URL",
		})
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "direct publish response read failed",
			what:  "the Village publish response body could not be read completely",
			why:   err.Error(),
			where: "internal/e2e/skipgate_e2e_test.go directPublish",
			when:  "after /api/v1/transcripts/publish returned an HTTP response",
			means: "the harness cannot reliably assert the publish outcome",
			fix:   "check the Village connection and retry after confirming the server returns a complete response body",
		})
	}
	return resp.StatusCode, string(out)
}

// writeSandboxConfig writes a peasant config.yaml under the sandbox config dir
// that scopes ingest to the committed claude-code, codex, and cursor fixtures
// (opencode disabled) and keeps all transcript output inside the sandbox, so
// local harness directories and the local data directory are never touched.
// The codex path is CodexFixtureSourcePath() — the sessions/ dir
// ITSELF, since the codex adapter does a strict depth-4 {root}/YYYY/MM/DD walk.
func writeSandboxConfig(t *testing.T, configHome, dataHome string) {
	t.Helper()
	outputBase := filepath.Join(dataHome, string(defaults.AppName), "peasant-sync")
	cfg := fmt.Sprintf(`version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - %q
  opencode:
    enabled: false
  codex:
    enabled: true
    paths:
      - %q
  cursor:
    enabled: true
    paths:
      - %q
output:
  basePath: %q
`, FixtureSourcePath(), CodexFixtureSourcePath(), CursorFixtureSourcePath(), outputBase)
	dir := filepath.Join(configHome, string(defaults.AppName))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("e2e: write sandbox config: %v", err)
	}
}

// --- peasant CLI subprocesses ---

func runPeasant(t *testing.T, bin string, xdgEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), xdgEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("e2e: peasant %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// annJSON / pushJSON mirror cmd/peasant's jsonAnnotResult / jsonPushResult for
// parsing `push --json`.
type annJSON struct {
	Total      int    `json:"total"`
	Created    int    `json:"created"`
	Updated    int    `json:"updated"`
	Skipped    int    `json:"skipped"`
	Retracted  int    `json:"retracted"`
	Errors     int    `json:"errors"`
	Error      string `json:"error,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
}

type pushJSON struct {
	New         int    `json:"new"`
	Updated     int    `json:"updated"`
	Skipped     int    `json:"skipped"`
	Errors      int    `json:"errors"`
	Held        int    `json:"held"`
	EmptyReason string `json:"empty_reason,omitempty"`
	Sessions    []struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		Error     string `json:"error,omitempty"`
	} `json:"sessions"`
	Annotations annJSON `json:"annotations"`
}

func (p pushJSON) transcripts() int { return p.New + p.Updated }

func (p pushJSON) sessionStatus(sessionID string) (status, errMsg string, ok bool) {
	for _, s := range p.Sessions {
		if s.SessionID == sessionID {
			return s.Status, s.Error, true
		}
	}
	return "", "", false
}

func (p pushJSON) sessionStatuses() string {
	parts := make([]string, 0, len(p.Sessions))
	for _, s := range p.Sessions {
		part := s.SessionID + "=" + s.Status
		if s.Error != "" {
			part += "(" + s.Error + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func assertCursorSessionsHeldForNoModel(t *testing.T, opts harnessOptions, p pushJSON) {
	t.Helper()
	for _, sessionID := range []string{CursorFixtureRootSessionID, CursorFixtureAbortedSessionID} {
		status, errMsg, ok := p.sessionStatus(sessionID)
		check(t, opts, ok, "push#1 missing cursor session %s in JSON sessions: %s", sessionID, p.sessionStatuses())
		check(t, opts, status == "error", "push#1 cursor session %s status = %q, want error", sessionID, status)
		check(t, opts, strings.Contains(errMsg, "no model recorded in the session metadata"),
			"push#1 cursor session %s error = %q, want no-model rejection", sessionID, errMsg)
	}
}

func expectedPushSessionIDs(t *testing.T) map[string]struct{} {
	t.Helper()
	ids := make(map[string]struct{})
	for _, index := range loadFixtureIndexes(t) {
		for _, session := range index.Sessions {
			id := session.ID
			if index.Harness == schema.HarnessClaudeCode.String() && session.Kind == "claude-subagent" {
				id = strings.TrimSuffix(filepath.Base(session.Path), defaults.ExtJSONL.String())
			}
			if id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func expectedCursorPushErrorIDs() map[string]struct{} {
	return map[string]struct{}{
		CursorFixtureRootSessionID:    {},
		CursorFixtureAbortedSessionID: {},
	}
}

func validatePushJSON(t *testing.T, p pushJSON) error {
	t.Helper()
	wantSessions := expectedPushSessionIDs(t)
	wantErrors := expectedCursorPushErrorIDs()
	seen := make(map[string]struct{}, len(p.Sessions))
	transcriptErrors := make(map[string]string)
	for _, session := range p.Sessions {
		if _, ok := wantSessions[session.SessionID]; !ok {
			return fmt.Errorf("push JSON contains unexpected session %q; expected fixture sessions are %s", session.SessionID, formatSessionIDSet(wantSessions))
		}
		if _, duplicate := seen[session.SessionID]; duplicate {
			return fmt.Errorf("push JSON contains duplicate session %q; each fixture session must appear exactly once", session.SessionID)
		}
		seen[session.SessionID] = struct{}{}
		if session.Status == "error" {
			if strings.TrimSpace(session.Error) == "" {
				return fmt.Errorf("push JSON marks session %q as error but provides no session error text", session.SessionID)
			}
			transcriptErrors[session.SessionID] = session.Error
		} else if strings.TrimSpace(session.Error) != "" {
			return fmt.Errorf("push JSON provides error text for non-error session %q (status=%q): %q", session.SessionID, session.Status, session.Error)
		}
	}
	fullFixtureResult := sameSessionIDSet(seen, wantSessions)
	cursorRetryResult := sameSessionIDSet(seen, wantErrors)
	if !fullFixtureResult && !cursorRetryResult {
		return fmt.Errorf("push JSON sessions must contain either all %d fixture sessions or exactly the two known Cursor retry sessions; got %d (%s)", len(wantSessions), len(seen), p.sessionStatuses())
	}
	if p.Errors != len(transcriptErrors) {
		return fmt.Errorf("push JSON top-level transcript errors = %d, want %d to match per-session error rows (%s)", p.Errors, len(transcriptErrors), p.sessionStatuses())
	}
	if p.Annotations.Errors != 0 || strings.TrimSpace(p.Annotations.Error) != "" {
		return fmt.Errorf("push JSON annotation errors = %d and error text %q; annotation failures are never an expected partial push", p.Annotations.Errors, p.Annotations.Error)
	}
	if fullFixtureResult && len(transcriptErrors) == 0 {
		return nil
	}
	errorIDs := make(map[string]struct{}, len(transcriptErrors))
	for sessionID := range transcriptErrors {
		errorIDs[sessionID] = struct{}{}
	}
	if !sameSessionIDSet(errorIDs, wantErrors) {
		return fmt.Errorf("push JSON has %d transcript errors, want exactly the two known Cursor no-model errors: %s", len(transcriptErrors), p.sessionStatuses())
	}
	for sessionID := range wantErrors {
		errMsg, ok := transcriptErrors[sessionID]
		if !ok {
			return fmt.Errorf("push JSON is missing known Cursor no-model error for %s: %s", sessionID, p.sessionStatuses())
		}
		lower := strings.ToLower(errMsg)
		if !strings.Contains(lower, "errnomodel") && !strings.Contains(lower, "no model") {
			return fmt.Errorf("push JSON Cursor session %s error = %q, want ErrNoModel/no-model text", sessionID, errMsg)
		}
	}
	return nil
}

func sameSessionIDSet(got, want map[string]struct{}) bool {
	if len(got) != len(want) {
		return false
	}
	for sessionID := range want {
		if _, ok := got[sessionID]; !ok {
			return false
		}
	}
	return true
}

func formatSessionIDSet(ids map[string]struct{}) string {
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func runPeasantPush(t *testing.T, bin string, xdgEnv []string) pushJSON {
	t.Helper()
	cmd := exec.Command(bin, "village", "push", "--json", "--non-interactive")
	cmd.Env = append(os.Environ(), xdgEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var p pushJSON
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		fatalActionable(t, actionableFailure{
			title:  "Peasant push JSON was malformed",
			what:   "the real `village push --json --non-interactive` subprocess did not emit a valid JSON result on stdout",
			why:    err.Error(),
			where:  "internal/e2e/skipgate_e2e_test.go runPeasantPush",
			when:   "parsing the push result before classifying its process exit status",
			means:  "the harness cannot distinguish the two known Cursor no-model fixture failures from an unexpected transcript or annotation failure",
			fix:    "keep stdout reserved for the documented push JSON result and inspect the subprocess stderr below",
			output: []byte(fmt.Sprintf("stdout: %s\nstderr: %s", stdout.String(), stderr.String())),
		})
	}
	if err := validatePushJSON(t, p); err != nil {
		fatalActionable(t, actionableFailure{
			title:  "Peasant push JSON contained an unexpected result",
			what:   "the real push result did not match the committed fixture session set and annotation error contract",
			why:    err.Error(),
			where:  "internal/e2e/skipgate_e2e_test.go runPeasantPush",
			when:   "validating the parsed push JSON before accepting the subprocess outcome",
			means:  "the E2E must not hide missing sessions, additional failures, or annotation errors behind a partial-success allowance",
			fix:    "repair the producer result or update the committed fixture only when the fixture behavior intentionally changes",
			output: []byte(fmt.Sprintf("stdout: %s\nstderr: %s", stdout.String(), stderr.String())),
		})
	}
	if runErr == nil {
		return p
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		fatalActionable(t, actionableFailure{
			title:  "Peasant push subprocess could not start",
			what:   "the real push process returned a non-exit error rather than a normal process exit",
			why:    runErr.Error(),
			where:  "internal/e2e/skipgate_e2e_test.go runPeasantPush",
			when:   "running `village push --json --non-interactive`",
			means:  "a process-start, executable, or environment failure must not be treated as the known Cursor fixture partial result",
			fix:    "confirm the peasant binary is executable and the sandbox XDG environment is usable",
			output: []byte(fmt.Sprintf("stdout: %s\nstderr: %s", stdout.String(), stderr.String())),
		})
	}
	if p.Errors != len(expectedCursorPushErrorIDs()) {
		fatalActionable(t, actionableFailure{
			title:  "Peasant push exited non-zero for an unexpected reason",
			what:   "the real push process exited non-zero but its valid JSON did not report exactly the two known Cursor no-model session errors",
			why:    fmt.Sprintf("process error: %v; parsed result: %s", runErr, p.sessionStatuses()),
			where:  "internal/e2e/skipgate_e2e_test.go runPeasantPush",
			when:   "classifying the non-zero push exit after JSON parsing",
			means:  "the harness cannot safely continue because an unexpected transcript failure may have prevented publication",
			fix:    "inspect the push JSON and stderr; only the committed Cursor ErrNoModel/no-model pair may exit non-zero",
			output: []byte(fmt.Sprintf("stdout: %s\nstderr: %s", stdout.String(), stderr.String())),
		})
	}
	return p
}

// --- Sandbox and local-data guards ---

func assertStateInvariant(t *testing.T, opts harnessOptions, realStateDir string) {
	t.Helper()
	check(t, opts, realStateDir == string(defaults.ResolveStateDirPath()),
		"state invariant: realStateDir %q != live ResolveStateDirPath %q", realStateDir, defaults.ResolveStateDirPath())
}

func assertDBUnderSandbox(t *testing.T, opts harnessOptions, sandbox, dbPath string) {
	t.Helper()
	check(t, opts, strings.HasPrefix(dbPath, sandbox+string(filepath.Separator)),
		"resolved DB path %q is not under SANDBOX %q", dbPath, sandbox)
	if _, err := os.Stat(dbPath); err != nil {
		check(t, opts, false, "expected peasant DB under SANDBOX at %s after ingest: %v", dbPath, err)
	}
}

type realDirSnapshot struct {
	existed bool
	dirMod  time.Time
	dbMod   time.Time
	dbThere bool
}

func snapshotRealDataDir(realDataDir string) realDirSnapshot {
	var s realDirSnapshot
	if fi, err := os.Stat(realDataDir); err == nil {
		s.existed = true
		s.dirMod = fi.ModTime()
	}
	if fi, err := os.Stat(filepath.Join(realDataDir, "peasant.db")); err == nil {
		s.dbThere = true
		s.dbMod = fi.ModTime()
	}
	return s
}

func assertRealDataDirUnchanged(t *testing.T, opts harnessOptions, realDataDir string, before realDirSnapshot) {
	t.Helper()
	after := snapshotRealDataDir(realDataDir)
	check(t, opts, before.existed == after.existed,
		"real data dir existence changed (before=%v after=%v) — sandbox leaked", before.existed, after.existed)
	if before.existed && after.existed {
		check(t, opts, before.dirMod.Equal(after.dirMod),
			"real data dir mtime changed %v → %v — sandbox wrote to the real dir", before.dirMod, after.dirMod)
	}
	check(t, opts, before.dbThere == after.dbThere && before.dbMod.Equal(after.dbMod),
		"real peasant.db changed (there %v→%v, mtime %v→%v)", before.dbThere, after.dbThere, before.dbMod, after.dbMod)
}

// --- SANDBOX DB reads/writes (sqlite) ---

func readSessionIDs(t *testing.T, dbPath string) []string {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var ids []string
	if err := sqlitex.Execute(conn, `SELECT session_id FROM sessions ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { ids = append(ids, stmt.ColumnText(0)); return nil },
	}); err != nil {
		t.Fatalf("e2e: read session ids: %v", err)
	}
	return ids
}

func assertAllSessionsHaveMetrics(t *testing.T, opts harnessOptions, dbPath string) {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()

	var counts []string
	if err := sqlitex.Execute(conn, `
SELECT s.model_harness, COUNT(*), SUM(CASE WHEN m.session_id IS NULL THEN 1 ELSE 0 END)
FROM sessions s
LEFT JOIN session_metrics m ON s.session_id = m.session_id
GROUP BY s.model_harness
ORDER BY s.model_harness`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			counts = append(counts, fmt.Sprintf("%s sessions=%d missing_metrics=%d",
				stmt.ColumnText(0), stmt.ColumnInt(1), stmt.ColumnInt(2)))
			return nil
		},
	}); err != nil {
		t.Fatalf("e2e: read metrics coverage: %v", err)
	}

	var missing []string
	if err := sqlitex.Execute(conn, `
SELECT s.session_id, s.model_harness
FROM sessions s
LEFT JOIN session_metrics m ON s.session_id = m.session_id
WHERE m.session_id IS NULL
ORDER BY s.model_harness, s.session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			missing = append(missing, fmt.Sprintf("%s(%s)", stmt.ColumnText(0), stmt.ColumnText(1)))
			return nil
		},
	}); err != nil {
		t.Fatalf("e2e: read sessions missing metrics: %v", err)
	}

	check(t, opts, len(missing) == 0,
		"ingest produced sessions without metrics; those sessions would be excluded from push candidate queries before the model gate\ncoverage: %s\nmissing: %s",
		strings.Join(counts, "; "), strings.Join(missing, ", "))
}

// supersedeOneAnnotation marks exactly one system annotation superseded by another
// — the local "retire" action the retraction path keys on.
func supersedeOneAnnotation(t *testing.T, dbPath string) {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var ids []string
	if err := sqlitex.Execute(conn, `SELECT id FROM annotations WHERE superseded_by IS NULL ORDER BY id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { ids = append(ids, stmt.ColumnText(0)); return nil },
	}); err != nil {
		t.Fatalf("e2e: list annotations: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("e2e: need >=2 annotations to supersede one, have %d", len(ids))
	}
	if err := sqlitex.Execute(conn, `UPDATE annotations SET superseded_by = ? WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{ids[1], ids[0]}}); err != nil {
		t.Fatalf("e2e: supersede annotation: %v", err)
	}
}

func openSandboxDB(t *testing.T, dbPath string) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatalf("e2e: open sandbox DB %s: %v", dbPath, err)
	}
	if err := sqlitex.Execute(conn, "PRAGMA busy_timeout = 5000", nil); err != nil {
		t.Fatalf("e2e: busy_timeout: %v", err)
	}
	return conn
}

// --- generic infra helpers ---

func readMappedPort(t *testing.T, name, containerPort string) string {
	t.Helper()
	out, err := exec.Command("podman", "port", name, containerPort).Output()
	if err != nil {
		t.Fatalf("e2e: read mapped port %s for %s: %v", containerPort, name, err)
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		t.Fatalf("e2e: unexpected `podman port` output %q", string(out))
	}
	return line[idx+1:]
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("e2e: reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitReady polls fn via a ticker (no raw sleep) until true or timeout.
func waitReady(t *testing.T, what string, fn func() bool) {
	t.Helper()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(startupTimeout)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("e2e: %s not ready after %s (check logs above)", what, startupTimeout)
		case <-ticker.C:
		}
	}
}
