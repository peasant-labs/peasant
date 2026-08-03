package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPeasantBinSeamUsesInjectedCommand(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-peasant")
	script := "#!/bin/sh\necho fake-peasant \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake peasant: %v", err)
	}

	setenv(t, envPeasantBin, fake)
	bin := buildPeasant(t)
	out, err := exec.Command(bin, "sentinel").CombinedOutput()
	if err != nil {
		t.Fatalf("run injected peasant wrapper: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "fake-peasant sentinel"; got != want {
		t.Fatalf("wrapper output = %q, want %q", got, want)
	}
}

func TestPeasantBinWrapperQuotesInjectedCommand(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake peasant")
	script := "#!/bin/sh\necho quoted-peasant \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake peasant: %v", err)
	}

	wrapper, err := writePeasantBinWrapper(t.TempDir(), shellQuote(fake)+" --fixed")
	if err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	out, err := exec.Command(wrapper, "sentinel arg").CombinedOutput()
	if err != nil {
		t.Fatalf("run quoted wrapper: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "quoted-peasant --fixed sentinel arg"; got != want {
		t.Fatalf("wrapper output = %q, want %q", got, want)
	}
}

func TestPeasantBinSeamUnsetBuildsGoBinary(t *testing.T) {
	setenv(t, envPeasantBin, "")
	bin := buildPeasant(t)
	out, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("built peasant --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "peasant") {
		t.Fatalf("built peasant --help output missing command name\n%s", out)
	}
}

func TestPeasantBinSeamInvalidCommandFatalIsActionable(t *testing.T) {
	if getenv(envPeasantBinFatalHelper) == "1" {
		setenv(t, envPeasantBin, "/definitely/not/a/peasant")
		_ = buildPeasant(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestPeasantBinSeamInvalidCommandFatalIsActionable")
	cmd.Env = append(os.Environ(), envAssignment(envPeasantBinFatalHelper, "1"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid PEASANT_BIN helper unexpectedly succeeded\n%s", out)
	}
	output := string(out)
	for _, want := range []string{
		"PEASANT_BIN validation failed",
		"what:",
		"why:",
		"where:",
		"when:",
		"means:",
		"fix:",
		"/definitely/not/a/peasant",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("fatal output missing %q\n%s", want, output)
		}
	}
}

func TestExternalStackConfigValidation(t *testing.T) {
	valid := externalStackConfig{
		dsn:           "postgres://peasant:peasant@127.0.0.1:5432/peasant?sslmode=disable",
		minioEndpoint: "http://127.0.0.1:9000",
		bucket:        "peasant-e2e-transcripts-123",
		villageURL:    "http://127.0.0.1:8080",
	}

	cfg, err := validateExternalStackConfig(externalStackConfig{})
	if err != nil {
		t.Fatalf("all-unset external stack returned error: %v", err)
	}
	if cfg.engaged {
		t.Fatal("all-unset external stack engaged; want self-provision mode")
	}

	cfg, err = validateExternalStackConfig(valid)
	if err != nil {
		t.Fatalf("valid external stack returned error: %v", err)
	}
	if !cfg.engaged {
		t.Fatal("valid external stack did not engage")
	}
	if externalStackEngagedMarker == "" || !strings.Contains(externalStackEngagedMarker, "external-stack engaged") {
		t.Fatalf("external stack marker = %q, want positive engaged marker", externalStackEngagedMarker)
	}

	spaced := valid
	spaced.dsn = " " + valid.dsn + " "
	spaced.minioEndpoint = " " + valid.minioEndpoint + "/ "
	spaced.bucket = " " + valid.bucket + " "
	spaced.villageURL = " " + valid.villageURL + "/ "
	cfg, err = validateExternalStackConfig(spaced)
	if err != nil {
		t.Fatalf("spaced external stack returned error: %v", err)
	}
	if cfg.dsn != valid.dsn || cfg.minioEndpoint != valid.minioEndpoint || cfg.bucket != valid.bucket || cfg.villageURL != valid.villageURL {
		t.Fatalf("normalized external stack = %#v, want %#v", cfg, valid)
	}

	_, err = validateExternalStackConfig(externalStackConfig{dsn: valid.dsn, villageURL: valid.villageURL})
	if err == nil || !strings.Contains(err.Error(), "partial external stack") {
		t.Fatalf("partial external stack error = %v, want actionable partial-set error", err)
	}

	badBucket := valid
	badBucket.bucket = "bad/bucket"
	_, err = validateExternalStackConfig(badBucket)
	if err == nil || !strings.Contains(err.Error(), envS3Bucket.String()) {
		t.Fatalf("invalid S3_BUCKET error = %v, want named bucket error", err)
	}

	bad := valid
	bad.villageURL = "127.0.0.1:8080"
	_, err = validateExternalStackConfig(bad)
	if err == nil || !strings.Contains(err.Error(), envVillageURL.String()) {
		t.Fatalf("invalid VILLAGE_URL error = %v, want named URL error", err)
	}
}

func TestExternalStackFromEnv(t *testing.T) {
	setenv(t, envDatabaseURL, "")
	setenv(t, envS3Endpoint, "")
	setenv(t, envS3Bucket, "")
	setenv(t, envVillageURL, "")
	cfg, err := externalStackFromEnv()
	if err != nil {
		t.Fatalf("all-unset env returned error: %v", err)
	}
	if cfg.engaged {
		t.Fatal("all-unset env engaged external stack; want self-provision mode")
	}

	setenv(t, envDatabaseURL, "postgres://peasant:peasant@127.0.0.1:5432/peasant?sslmode=disable")
	setenv(t, envS3Endpoint, "http://127.0.0.1:9000")
	setenv(t, envS3Bucket, "peasant-e2e-transcripts-123")
	setenv(t, envVillageURL, "http://127.0.0.1:8080")
	cfg, err = externalStackFromEnv()
	if err != nil {
		t.Fatalf("complete external stack env returned error: %v", err)
	}
	if !cfg.engaged {
		t.Fatal("complete external stack env did not engage")
	}

	setenv(t, envS3Endpoint, "")
	_, err = externalStackFromEnv()
	if err == nil || !strings.Contains(err.Error(), "all four must be set together") {
		t.Fatalf("partial external stack env error = %v, want actionable all-or-none error", err)
	}
}

// fakeBucketChecker injects a BucketExists result so the preflight keeps DI unit
// coverage without a live MinIO (the seam migrated from a fake mc text-exec to a
// typed S3 client interface; *minio.Client satisfies bucketChecker in production).
type fakeBucketChecker struct {
	gotBucket string
	exists    bool
	err       error
}

func (f *fakeBucketChecker) BucketExists(_ context.Context, bucket string) (bool, error) {
	f.gotBucket = bucket
	return f.exists, f.err
}

func TestExternalStackBucketPreflightListsConfiguredBucket(t *testing.T) {
	checker := &fakeBucketChecker{exists: true}
	failure := externalStackBucketPreflight(context.Background(), checker, "peasant-e2e-transcripts-123")
	if failure != nil {
		t.Fatalf("bucket preflight returned failure: %#v", failure)
	}
	if want := "peasant-e2e-transcripts-123"; checker.gotBucket != want {
		t.Fatalf("preflight checked bucket %q, want %q", checker.gotBucket, want)
	}
}

func TestExternalStackBucketPreflightFailureIsActionable(t *testing.T) {
	failure := externalStackBucketPreflight(context.Background(),
		&fakeBucketChecker{err: fmt.Errorf("bucket missing")}, "wrong-bucket")
	if failure == nil {
		t.Fatal("bucket preflight succeeded; want actionable failure")
	}
	for _, want := range []string{
		"external stack bucket preflight failed",
		"S3_BUCKET wrong-bucket",
		"bucket missing",
		"checking injected external stack",
		"externalStackBucketPreflight",
		"wrong-bucket",
	} {
		joined := strings.Join([]string{failure.title, failure.what, failure.why, failure.where, failure.when, failure.means, failure.fix, string(failure.output)}, "\n")
		if !strings.Contains(joined, want) {
			t.Fatalf("failure missing %q\n%#v", want, failure)
		}
	}
}

func TestExternalStackBucketPreflightMissingBucketIsActionable(t *testing.T) {
	failure := externalStackBucketPreflight(context.Background(),
		&fakeBucketChecker{exists: false}, "absent-bucket")
	if failure == nil {
		t.Fatal("bucket preflight succeeded for a non-existent bucket; want actionable failure")
	}
	joined := strings.Join([]string{failure.title, failure.what, failure.why, failure.fix}, "\n")
	for _, want := range []string{"does not exist", "absent-bucket", "create bucket"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing-bucket failure missing %q\n%#v", want, failure)
		}
	}
}

func TestTranscriptBucketSeams(t *testing.T) {
	now := time.Unix(123, 456)
	first := uniqueNameAt("transcripts", 42, now)
	second := uniqueNameAt("transcripts", 42, now.Add(time.Nanosecond))
	if first == second {
		t.Fatalf("generated transcript buckets were not unique: %q", first)
	}
	if !strings.HasPrefix(first, e2eInfraNamePrefix+"transcripts-") {
		t.Fatalf("generated transcript bucket = %q, want %s transcript prefix", first, e2eInfraNamePrefix)
	}
	const historicalFixedBucket = "peasant-transcripts"
	if first == historicalFixedBucket || second == historicalFixedBucket {
		t.Fatalf("generated transcript bucket reused fixed bucket %q", historicalFixedBucket)
	}
	if static := "peasant-e2e-transcripts-static"; isStaleE2EInfraName(static, now, time.Hour) {
		t.Fatalf("static malformed bucket %q parsed as stale infra name", static)
	}
	if err := validateS3BucketName(first); err != nil {
		t.Fatalf("generated transcript bucket did not validate: %v", err)
	}
}

func TestInfraReaperTargetsOnlyStalePeasantE2EContainers(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	old := now.Add(-2 * staleE2ETTL)
	recent := now.Add(-staleE2ETTL / 2)
	oldPG := uniqueNameAt("pg", 101, old)
	oldRelease := uniqueNameAt("release-ubuntu-22-04", 202, old)
	recentMinIO := uniqueNameAt("minio", 303, recent)
	oldRunningVillage := uniqueNameAt("village", 404, old)
	names := staleStoppedE2EInfraNames(strings.Join([]string{
		oldPG + "\tExited (0) 25 hours ago",
		"unrelated\tExited (0) 25 hours ago",
		" " + recentMinIO + " \tExited (0) 10 minutes ago",
		"x-peasant-e2e-village",
		"peasant-e2e-transcripts-static",
		oldRunningVillage + "\tUp 25 hours",
		oldRelease + "\tCreated",
	}, "\n"), now, staleE2ETTL)
	wantNames := []string{oldPG, oldRelease}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("stale infra names = %v, want %v", names, wantNames)
	}

	args := podmanReapE2EInfraArgs(names)
	wantArgs := []string{"rm", "-fv", oldPG, oldRelease}
	if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("reap args = %v, want %v", args, wantArgs)
	}
	if args := podmanReapE2EInfraArgs(nil); args != nil {
		t.Fatalf("empty reap args = %v, want nil", args)
	}
}

func TestSeededBaselineCounts(t *testing.T) {
	assertSeededBaselineCounts(t, harnessOptions{assert: true}, seededZeroContentBaselineBeforePush, seededZeroContentBaselineBeforePush)
	assertSeededBaselineCounts(t, harnessOptions{assert: false},
		seededBaselineCounts{transcripts: 1, annotations: 2, s3Objects: 3},
		seededZeroContentBaselineBeforePush)
}

func TestSeededBaselineMismatchFatalIsActionable(t *testing.T) {
	if getenv(envBaselineFatalHelper) == "1" {
		assertSeededBaselineCounts(t, harnessOptions{assert: true},
			seededBaselineCounts{transcripts: 1},
			seededZeroContentBaselineBeforePush)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSeededBaselineMismatchFatalIsActionable")
	cmd.Env = append(os.Environ(), envAssignment(envBaselineFatalHelper, "1"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("baseline mismatch helper unexpectedly succeeded\n%s", out)
	}
	if output := string(out); !strings.Contains(output, "seeded baseline before push#1") {
		t.Fatalf("baseline fatal output missing push#1 context\n%s", output)
	}
}

func TestRefreshRejectsExternalStack(t *testing.T) {
	if getenv(envRefreshExternalFatalHelper) == "1" {
		stack := harnessStack{external: true}
		stack.requireRefreshable(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRefreshRejectsExternalStack")
	cmd.Env = append(os.Environ(), envAssignment(envRefreshExternalFatalHelper, "1"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("external refresh helper unexpectedly succeeded\n%s", out)
	}
	output := string(out)
	for _, want := range []string{"refresh external stack failed", "what:", "why:", "where:", "when:", "means:", "fix:"} {
		if !strings.Contains(output, want) {
			t.Fatalf("refresh fatal output missing %q\n%s", want, output)
		}
	}
}

func TestRefreshRejectsMissingDatabaseHandle(t *testing.T) {
	if getenv(envRefreshMissingDBFatalHelper) == "1" {
		stack := harnessStack{
			minioEndpoint: "http://127.0.0.1:9000",
			bucket:        "test-bucket",
			village:       &villageProcess{},
		}
		stack.requireRefreshable(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRefreshRejectsMissingDatabaseHandle")
	cmd.Env = append(os.Environ(), envAssignment(envRefreshMissingDBFatalHelper, "1"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("missing-database refresh helper unexpectedly succeeded\n%s", out)
	}
	output := string(out)
	if !strings.Contains(output, "refresh self-provisioned stack failed") ||
		!strings.Contains(output, "databaseSet=false") ||
		!strings.Contains(output, "fix:") {
		t.Fatalf("missing-database refresh fatal was not actionable\n%s", output)
	}
}
