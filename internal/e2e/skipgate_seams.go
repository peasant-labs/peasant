package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	minioUser, minioPassword = "minioadmin", "minioadmin"

	e2eInfraNamePrefix = "peasant-e2e-"

	externalStackEngagedMarker = "external-stack engaged"
)

type harnessOptions struct {
	assert bool // false (demo mode) downgrades assertions to logs
}

type villageBinaries struct {
	server     string
	setupDemo  string
	backendDir string // for reading the village's contract fixtures (village-scan)
}

type harnessStack struct {
	dsn           string
	db            *sql.DB
	minioEndpoint string
	bucket        string
	villageURL    string
	village       *villageProcess
	external      bool
}

type externalStackConfig struct {
	dsn           string
	minioEndpoint string
	bucket        string
	villageURL    string
	engaged       bool
}

type villageProcess struct {
	bin        string
	dsn        string
	s3Endpoint string
	bucket     string
	url        string
	cmd        *exec.Cmd
}

type seededBaselineCounts struct {
	transcripts int
	annotations int
	s3Objects   int
}

// village-setup-demo seeds users and credentials only. Before push#1, the
// transcript, annotation, and transcript-object stores must still be empty.
var seededZeroContentBaselineBeforePush = seededBaselineCounts{}

type actionableFailure struct {
	title  string
	what   string
	why    string
	where  string
	when   string
	means  string
	fix    string
	output []byte
}

func fatalActionable(t *testing.T, failure actionableFailure) {
	t.Helper()
	msg := fmt.Sprintf("e2e: %s\nwhat: %s\nwhy: %s\nwhere: %s\nwhen: %s\nmeans: %s\nfix: %s",
		failure.title, failure.what, failure.why, failure.where, failure.when, failure.means, failure.fix)
	if len(failure.output) > 0 {
		msg += fmt.Sprintf("\noutput:\n%s", failure.output)
	}
	t.Fatal(msg)
}

func buildPeasant(t *testing.T) string {
	t.Helper()
	if raw := strings.TrimSpace(getenv(envPeasantBin)); raw != "" {
		return validateInjectedPeasantBin(t, raw)
	}
	out := filepath.Join(t.TempDir(), "peasant")
	if output, err := exec.Command("go", "build", "-o", out, "github.com/peasant-labs/peasant/cmd/peasant").CombinedOutput(); err != nil {
		t.Fatalf("e2e: build peasant: %v\n%s", err, output)
	}
	return out
}

func validateInjectedPeasantBin(t *testing.T, raw string) string {
	t.Helper()
	wrapper, err := writePeasantBinWrapper(t.TempDir(), raw)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "PEASANT_BIN validation failed",
			what:  "could not create a wrapper for the injected peasant command",
			why:   err.Error(),
			where: "internal/e2e/skipgate_seams.go buildPeasant",
			when:  "resolving PEASANT_BIN before the harness starts",
			means: "the e2e harness cannot run the caller-provided installed binary",
			fix:   "set PEASANT_BIN to an executable peasant binary plus optional arguments, for example /usr/bin/peasant or 'podman exec -i distro /usr/bin/peasant'; shell operators are not supported",
		})
	}
	cmd := exec.Command(wrapper, "--help")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalActionable(t, actionableFailure{
			title:  "PEASANT_BIN validation failed",
			what:   "the injected peasant command did not execute successfully",
			why:    err.Error(),
			where:  "internal/e2e/skipgate_seams.go buildPeasant",
			when:   "running PEASANT_BIN with --help before the harness starts",
			means:  "the e2e harness would test the wrong thing or fail later with an opaque subprocess error",
			fix:    fmt.Sprintf("set PEASANT_BIN to a working peasant binary/command visible from this environment; command was %q", raw),
			output: out,
		})
	}
	t.Logf("e2e: using injected PEASANT_BIN command: %s", raw)
	return wrapper
}

func writePeasantBinWrapper(dir, command string) (string, error) {
	argv, err := splitCommandLine(command)
	if err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("PEASANT_BIN is empty after trimming whitespace")
	}
	path := filepath.Join(dir, "peasant-bin-wrapper")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("exec")
	for _, arg := range argv {
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
	}
	b.WriteString(" \"$@\"\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil {
		return "", fmt.Errorf("write wrapper %s: %w", path, err)
	}
	return path, nil
}

func splitCommandLine(command string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inSingle, inDouble, escaped, sawToken := false, false, false, false
	for _, r := range strings.TrimSpace(command) {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			sawToken = true
		case r == '\\' && !inSingle:
			escaped = true
			sawToken = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			sawToken = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			sawToken = true
		case (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble:
			if sawToken {
				args = append(args, cur.String())
				cur.Reset()
				sawToken = false
			}
		default:
			cur.WriteRune(r)
			sawToken = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("PEASANT_BIN has a trailing escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("PEASANT_BIN has an unterminated quote")
	}
	if sawToken {
		args = append(args, cur.String())
	}
	return args, nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func uniqueName(kind string) string {
	return uniqueNameAt(kind, os.Getpid(), time.Now())
}

func uniqueNameAt(kind string, pid int, ts time.Time) string {
	return fmt.Sprintf("%s%s-%d-%d", e2eInfraNamePrefix, kind, pid, ts.UnixNano())
}

func externalStackFromEnv() (externalStackConfig, error) {
	cfg := externalStackConfig{
		dsn:           strings.TrimSpace(getenv(envDatabaseURL)),
		minioEndpoint: strings.TrimSpace(getenv(envS3Endpoint)),
		bucket:        strings.TrimSpace(getenv(envS3Bucket)),
		villageURL:    strings.TrimSpace(getenv(envVillageURL)),
	}
	return validateExternalStackConfig(cfg)
}

func validateExternalStackConfig(cfg externalStackConfig) (externalStackConfig, error) {
	cfg.dsn = strings.TrimSpace(cfg.dsn)
	cfg.minioEndpoint = strings.TrimRight(strings.TrimSpace(cfg.minioEndpoint), "/")
	cfg.bucket = strings.TrimSpace(cfg.bucket)
	cfg.villageURL = strings.TrimRight(strings.TrimSpace(cfg.villageURL), "/")

	set := 0
	for _, value := range []string{cfg.dsn, cfg.minioEndpoint, cfg.bucket, cfg.villageURL} {
		if value != "" {
			set++
		}
	}
	if set == 0 {
		return externalStackConfig{}, nil
	}
	if set != 4 {
		return externalStackConfig{}, fmt.Errorf("partial external stack: %s set=%t, %s set=%t, %s set=%t, %s set=%t; all four must be set together or all must be unset",
			envDatabaseURL, cfg.dsn != "", envS3Endpoint, cfg.minioEndpoint != "", envS3Bucket, cfg.bucket != "", envVillageURL, cfg.villageURL != "")
	}
	if err := validatePostgresDSN(cfg.dsn); err != nil {
		return externalStackConfig{}, fmt.Errorf("%s is invalid: %w", envDatabaseURL, err)
	}
	if err := validateHTTPURL(envS3Endpoint.String(), cfg.minioEndpoint); err != nil {
		return externalStackConfig{}, err
	}
	if err := validateS3BucketName(cfg.bucket); err != nil {
		return externalStackConfig{}, fmt.Errorf("%s is invalid: %w", envS3Bucket, err)
	}
	if err := validateHTTPURL(envVillageURL.String(), cfg.villageURL); err != nil {
		return externalStackConfig{}, err
	}
	cfg.engaged = true
	return cfg, nil
}

func validatePostgresDSN(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("scheme %q is not postgres or postgresql", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("missing database name")
	}
	return nil
}

func validateS3BucketName(bucket string) error {
	if bucket == "" {
		return fmt.Errorf("missing bucket name")
	}
	if strings.ContainsAny(bucket, "/\\ \t\n\r") {
		return fmt.Errorf("contains whitespace or path separators")
	}
	if strings.HasPrefix(bucket, "-") || strings.HasSuffix(bucket, "-") {
		return fmt.Errorf("must not start or end with '-'")
	}
	return nil
}

func validateHTTPURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is invalid: parse URL: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s is invalid: scheme %q is not http or https", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s is invalid: missing host", name)
	}
	return nil
}

// bucketChecker is the minimal S3 capability externalStackBucketPreflight needs.
// *minio.Client (built by newMinioClient) satisfies it in production; unit tests
// inject a fake, so the preflight keeps DI unit coverage without a live MinIO.
type bucketChecker interface {
	BucketExists(ctx context.Context, bucketName string) (bool, error)
}

func externalStackBucketPreflight(ctx context.Context, client bucketChecker, bucket string) *actionableFailure {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return &actionableFailure{
			title: "external stack bucket preflight failed",
			what:  fmt.Sprintf("BucketExists could not check S3_BUCKET %s", bucket),
			why:   err.Error(),
			where: "internal/e2e/skipgate_seams.go externalStackBucketPreflight",
			when:  "checking injected external stack before running the harness",
			means: "the harness might publish to or inspect a different bucket than the injected village server",
			fix:   "check that the injected MinIO endpoint is reachable and the credentials can list buckets",
		}
	}
	if !exists {
		return &actionableFailure{
			title: "external stack bucket preflight failed",
			what:  fmt.Sprintf("S3_BUCKET %s does not exist on the injected MinIO endpoint", bucket),
			why:   "BucketExists returned false",
			where: "internal/e2e/skipgate_seams.go externalStackBucketPreflight",
			when:  "checking injected external stack before running the harness",
			means: "the harness might publish to or inspect a different bucket than the injected village server",
			fix:   fmt.Sprintf("create bucket %s on the injected MinIO endpoint, or pass the bucket used by the injected village server", bucket),
		}
	}
	return nil
}

func requireTranscriptBucket(t *testing.T, bucket string) string {
	t.Helper()
	bucket = strings.TrimSpace(bucket)
	if err := validateS3BucketName(bucket); err != nil {
		t.Fatalf("e2e: transcript bucket is invalid: %v", err)
	}
	return bucket
}

type e2eInfraName struct {
	name      string
	kind      string
	pid       int
	timestamp time.Time
}

func parseE2EInfraName(name string) (e2eInfraName, bool) {
	name = strings.TrimSpace(name)
	rest, ok := strings.CutPrefix(name, e2eInfraNamePrefix)
	if !ok {
		return e2eInfraName{}, false
	}
	lastDash := strings.LastIndex(rest, "-")
	if lastDash < 0 {
		return e2eInfraName{}, false
	}
	tsRaw := rest[lastDash+1:]
	rest = rest[:lastDash]
	pidDash := strings.LastIndex(rest, "-")
	if pidDash < 0 {
		return e2eInfraName{}, false
	}
	kind := rest[:pidDash]
	pidRaw := rest[pidDash+1:]
	if kind == "" {
		return e2eInfraName{}, false
	}
	pid, err := strconv.Atoi(pidRaw)
	if err != nil || pid <= 0 {
		return e2eInfraName{}, false
	}
	tsNano, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || tsNano <= 0 {
		return e2eInfraName{}, false
	}
	return e2eInfraName{
		name:      name,
		kind:      kind,
		pid:       pid,
		timestamp: time.Unix(0, tsNano),
	}, true
}

func isStaleE2EInfraName(name string, now time.Time, ttl time.Duration) bool {
	parsed, ok := parseE2EInfraName(name)
	return ok && parsed.timestamp.Before(now.Add(-ttl))
}

func isStoppedPodmanStatus(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return false
	}
	for _, prefix := range []string{"created", "configured", "exited", "dead", "removing"} {
		if strings.HasPrefix(status, prefix) {
			return true
		}
	}
	return false
}

func staleStoppedE2EInfraNames(podmanPSOutput string, now time.Time, ttl time.Duration) []string {
	var names []string
	for _, line := range strings.Split(podmanPSOutput, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		status := strings.TrimSpace(fields[1])
		if isStoppedPodmanStatus(status) && isStaleE2EInfraName(name, now, ttl) {
			names = append(names, name)
		}
	}
	return names
}

func podmanReapE2EInfraArgs(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	args := []string{"rm", "-fv"}
	return append(args, names...)
}

func (s *harnessStack) requireRefreshable(t *testing.T) {
	t.Helper()
	if s.external {
		fatalActionable(t, actionableFailure{
			title: "refresh external stack failed",
			what:  "refresh was requested for an injected external stack",
			why:   "DATABASE_URL/S3_ENDPOINT/S3_BUCKET/VILLAGE_URL identify endpoints but do not provide the harness-owned database, MinIO, and Village process handles required for a deterministic reset",
			where: "internal/e2e/skipgate_seams.go harnessStack.requireRefreshable",
			when:  "resetting the warm stack between distro iterations",
			means: "the driver would leave stale village state between package installs",
			fix:   "call refresh only on the provision-once stack returned by provisionHarnessStack with external-stack env unset",
		})
	}
	if s.db == nil || s.minioEndpoint == "" || s.bucket == "" || s.village == nil {
		fatalActionable(t, actionableFailure{
			title: "refresh self-provisioned stack failed",
			what:  "the stack descriptor is missing database, MinIO, or village handles",
			why:   fmt.Sprintf("databaseSet=%t minioEndpoint=%q bucket=%q villageSet=%t", s.db != nil, s.minioEndpoint, s.bucket, s.village != nil),
			where: "internal/e2e/skipgate_seams.go harnessStack.requireRefreshable",
			when:  "resetting the warm stack between distro iterations",
			means: "the driver cannot prove the next distro starts from a clean baseline",
			fix:   "use the harnessStack returned by provisionHarnessStack without modifying its handle fields",
		})
	}
}

func assertSeededBaselineCounts(t *testing.T, opts harnessOptions, got, want seededBaselineCounts) {
	t.Helper()
	check(t, opts, got.transcripts == want.transcripts,
		"seeded baseline before push#1: village transcripts = %d, want %d", got.transcripts, want.transcripts)
	check(t, opts, got.annotations == want.annotations,
		"seeded baseline before push#1: village annotations = %d, want %d", got.annotations, want.annotations)
	check(t, opts, got.s3Objects == want.s3Objects,
		"seeded baseline before push#1: S3 objects = %d, want %d", got.s3Objects, want.s3Objects)
}

func check(t *testing.T, opts harnessOptions, ok bool, format string, args ...any) {
	t.Helper()
	if ok {
		return
	}
	if opts.assert {
		t.Fatalf(format, args...)
	}
	t.Logf("[demo] NOT asserted: "+format, args...)
}
