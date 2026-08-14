package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const releaseTagName = "v9.8.7-rc1"

type releaseTagScenario string

const (
	releaseTagAnnotatedReachable   releaseTagScenario = "annotated-reachable"
	releaseTagAnnotatedLocalOther  releaseTagScenario = "annotated-reachable-local-other"
	releaseTagMissing              releaseTagScenario = "missing"
	releaseTagLightweight          releaseTagScenario = "lightweight"
	releaseTagPeeledRecordMissing  releaseTagScenario = "peeled-record-missing"
	releaseTagPeeledRecordMismatch releaseTagScenario = "peeled-record-mismatch"
	releaseTagAnnotatedUnreachable releaseTagScenario = "annotated-unreachable"
)

func (scenario releaseTagScenario) valid() bool {
	switch scenario {
	case releaseTagAnnotatedReachable, releaseTagAnnotatedLocalOther, releaseTagMissing, releaseTagLightweight, releaseTagPeeledRecordMissing, releaseTagPeeledRecordMismatch, releaseTagAnnotatedUnreachable:
		return true
	default:
		return false
	}
}

type releaseTagVerificationCase struct {
	ID                           string             `yaml:"id"`
	Scenario                     releaseTagScenario `yaml:"scenario"`
	RewriteLocalTag              bool               `yaml:"rewrite_local_tag"`
	RewriteLocalTagToOtherCommit bool               `yaml:"rewrite_local_tag_to_other_commit"`
	OmitPeeledRecord             bool               `yaml:"omit_peeled_record"`
	ReplacePeeledRecord          bool               `yaml:"replace_peeled_record"`
	WantSuccess                  bool               `yaml:"want_success"`
	WantOutput                   string             `yaml:"want_output"`
}

type releaseTagVerificationFixture struct {
	Cases []releaseTagVerificationCase `yaml:"cases"`
}

//go:embed testdata/workflows/release_tag_verification.yaml
var releaseTagVerificationFixtureBytes []byte

func loadReleaseTagVerificationFixture(t *testing.T) releaseTagVerificationFixture {
	t.Helper()

	var fixture releaseTagVerificationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(releaseTagVerificationFixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("release tag verification: parse fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("release tag verification: fixture must have exact EOF: %v", err)
	}
	if len(fixture.Cases) != 7 {
		t.Fatalf("release tag verification: fixture has %d cases, want complete seven-scenario inventory", len(fixture.Cases))
	}

	seenIDs := make(map[string]struct{}, len(fixture.Cases))
	seenScenarios := make(map[releaseTagScenario]struct{}, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.ID) == "" || !testCase.Scenario.valid() || strings.TrimSpace(testCase.WantOutput) == "" {
			t.Fatalf("release tag verification: fixture case %d is incomplete: %+v", index, testCase)
		}
		if _, exists := seenIDs[testCase.ID]; exists {
			t.Fatalf("release tag verification: duplicate fixture id %q", testCase.ID)
		}
		seenIDs[testCase.ID] = struct{}{}
		if _, exists := seenScenarios[testCase.Scenario]; exists {
			t.Fatalf("release tag verification: duplicate scenario %q", testCase.Scenario)
		}
		seenScenarios[testCase.Scenario] = struct{}{}
		switch testCase.Scenario {
		case releaseTagAnnotatedReachable:
			if !testCase.RewriteLocalTag || testCase.RewriteLocalTagToOtherCommit || !testCase.WantSuccess || testCase.OmitPeeledRecord || testCase.ReplacePeeledRecord {
				t.Fatalf("release tag verification: reachable annotated scenario must reproduce a local rewrite and succeed: %+v", testCase)
			}
		case releaseTagAnnotatedLocalOther:
			if testCase.RewriteLocalTag || !testCase.RewriteLocalTagToOtherCommit || !testCase.WantSuccess || testCase.OmitPeeledRecord || testCase.ReplacePeeledRecord {
				t.Fatalf("release tag verification: alternate local commit scenario must preserve the remote annotated tag and succeed: %+v", testCase)
			}
		case releaseTagPeeledRecordMissing:
			if !testCase.OmitPeeledRecord || testCase.ReplacePeeledRecord || testCase.WantSuccess {
				t.Fatalf("release tag verification: missing peeled-record scenario must withhold the record and fail: %+v", testCase)
			}
		case releaseTagPeeledRecordMismatch:
			if testCase.OmitPeeledRecord || !testCase.ReplacePeeledRecord || testCase.WantSuccess {
				t.Fatalf("release tag verification: mismatched peeled-record scenario must replace the record and fail: %+v", testCase)
			}
		default:
			if testCase.RewriteLocalTag || testCase.RewriteLocalTagToOtherCommit || testCase.OmitPeeledRecord || testCase.ReplacePeeledRecord || testCase.WantSuccess {
				t.Fatalf("release tag verification: rejection scenario has incompatible controls: %+v", testCase)
			}
		}
	}
	return fixture
}

func TestReleaseTagVerificationUsesRemoteAnnotatedTag(t *testing.T) {
	fixture := loadReleaseTagVerificationFixture(t)
	repositoryRoot := releaseWorkflowRepoRoot(t)
	helper := filepath.Join(repositoryRoot, "scripts", "verify-release-tag")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("release tag verification: locate git: %v", err)
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			runner, developCommit := prepareReleaseTagRepository(t, testCase.Scenario)
			remoteTagOID := ""
			localTagOID := ""
			if testCase.RewriteLocalTag || testCase.RewriteLocalTagToOtherCommit {
				remoteTagRecord := strings.Fields(runGit(t, runner, "ls-remote", "origin", "refs/tags/"+releaseTagName))
				if len(remoteTagRecord) != 2 {
					t.Fatalf("release tag verification: remote tag record = %q, want one OID and ref", remoteTagRecord)
				}
				remoteTagOID = remoteTagRecord[0]
			}
			if testCase.RewriteLocalTag {
				runGit(t, runner, "update-ref", "refs/tags/"+releaseTagName, developCommit)
				localTagOID = developCommit
			}
			if testCase.RewriteLocalTagToOtherCommit {
				runGit(t, runner, "config", "user.name", "Release Test")
				runGit(t, runner, "config", "user.email", "release-test@example.invalid")
				runGit(t, runner, "commit", "--allow-empty", "-m", "checkout-only commit")
				localTagOID = strings.TrimSpace(runGit(t, runner, "rev-parse", "HEAD"))
				runGit(t, runner, "update-ref", "refs/tags/"+releaseTagName, localTagOID)
			}
			if localTagOID != "" {
				if got := strings.TrimSpace(runGit(t, runner, "cat-file", "-t", "refs/tags/"+releaseTagName)); got != "commit" {
					t.Fatalf("release tag verification: checkout-local rewrite precondition type = %q, want commit", got)
				}
			}

			command := exec.Command("bash", helper, releaseTagName)
			command.Dir = runner
			command.Env = os.Environ()
			if testCase.OmitPeeledRecord || testCase.ReplacePeeledRecord {
				wrapperDirectory := installGitLsRemoteWrapper(t, realGit)
				command.Env = append(command.Env,
					"PATH="+wrapperDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
					"REAL_GIT="+realGit,
				)
				if testCase.OmitPeeledRecord {
					command.Env = append(command.Env, "OMIT_PEELED_RECORD=1")
				}
				if testCase.ReplacePeeledRecord {
					command.Env = append(command.Env, "REPLACEMENT_PEELED_OID="+developCommit)
				}
			}

			output, runErr := command.CombinedOutput()
			if testCase.WantSuccess && runErr != nil {
				t.Fatalf("release tag verification: helper failed: %v\n%s", runErr, output)
			}
			if !testCase.WantSuccess && runErr == nil {
				t.Fatalf("release tag verification: helper unexpectedly succeeded\n%s", output)
			}
			if !strings.Contains(string(output), testCase.WantOutput) {
				t.Fatalf("release tag verification: output %q does not contain %q", output, testCase.WantOutput)
			}
			if localTagOID != "" {
				if !strings.Contains(string(output), developCommit) {
					t.Fatalf("release tag verification: success output %q does not identify remote target %s", output, developCommit)
				}
				if got := strings.TrimSpace(runGit(t, runner, "rev-parse", "refs/tags/"+releaseTagName)); got != localTagOID {
					t.Fatalf("release tag verification: helper changed checkout-local tag from %s to %s", localTagOID, got)
				}
				verificationRef := "refs/release-verification/tags/" + releaseTagName
				if got := strings.TrimSpace(runGit(t, runner, "cat-file", "-t", verificationRef)); got != "tag" {
					t.Fatalf("release tag verification: dedicated verification ref type = %q, want remote annotated tag", got)
				}
				if got := strings.TrimSpace(runGit(t, runner, "rev-parse", verificationRef)); got != remoteTagOID {
					t.Fatalf("release tag verification: dedicated verification ref OID = %s, want exact remote tag object %s", got, remoteTagOID)
				}
			}
		})
	}
}

func prepareReleaseTagRepository(t *testing.T, scenario releaseTagScenario) (runner string, developCommit string) {
	t.Helper()

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	producer := filepath.Join(root, "producer")
	runner = filepath.Join(root, "runner")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "--initial-branch=develop", producer)
	runGit(t, producer, "config", "user.name", "Release Test")
	runGit(t, producer, "config", "user.email", "release-test@example.invalid")
	runGit(t, producer, "commit", "--allow-empty", "-m", "develop release commit")
	developCommit = strings.TrimSpace(runGit(t, producer, "rev-parse", "HEAD"))
	runGit(t, producer, "remote", "add", "origin", remote)
	runGit(t, producer, "push", "origin", "develop")

	switch scenario {
	case releaseTagAnnotatedReachable, releaseTagAnnotatedLocalOther, releaseTagPeeledRecordMissing:
		runGit(t, producer, "tag", "--no-sign", "-a", releaseTagName, "-m", "release tag", developCommit)
		runGit(t, producer, "push", "origin", "refs/tags/"+releaseTagName)
	case releaseTagLightweight:
		runGit(t, producer, "tag", "--no-sign", releaseTagName, developCommit)
		runGit(t, producer, "push", "origin", "refs/tags/"+releaseTagName)
	case releaseTagPeeledRecordMismatch, releaseTagAnnotatedUnreachable:
		runGit(t, producer, "commit", "--allow-empty", "-m", "unmerged release commit")
		unreachableCommit := strings.TrimSpace(runGit(t, producer, "rev-parse", "HEAD"))
		runGit(t, producer, "tag", "--no-sign", "-a", releaseTagName, "-m", "unreachable release tag", unreachableCommit)
		runGit(t, producer, "push", "origin", "refs/tags/"+releaseTagName)
	case releaseTagMissing:
	default:
		t.Fatalf("release tag verification: unsupported scenario %q", scenario)
	}

	runGit(t, root, "clone", "--no-tags", remote, runner)
	return runner, developCommit
}

func installGitLsRemoteWrapper(t *testing.T, realGit string) string {
	t.Helper()

	directory := t.TempDir()
	wrapper := filepath.Join(directory, "git")
	contents := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "ls-remote" ] && { [ "${OMIT_PEELED_RECORD:-}" = "1" ] || [ -n "${REPLACEMENT_PEELED_OID:-}" ]; }; then
  output="$("${REAL_GIT}" "$@")"
  while IFS= read -r line; do
    case "$line" in
      *'^{}')
        if [ "${OMIT_PEELED_RECORD:-}" = "1" ]; then
          continue
        fi
        ref="${line#*$'\t'}"
        printf '%%s\t%%s\n' "$REPLACEMENT_PEELED_OID" "$ref"
        continue
        ;;
    esac
    printf '%%s\n' "$line"
  done <<<"$output"
  exit 0
fi
exec "%s" "$@"
`, realGit)
	if err := os.WriteFile(wrapper, []byte(contents), 0o700); err != nil {
		t.Fatalf("release tag verification: write git wrapper: %v", err)
	}
	return directory
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("release tag verification: git %s failed in %s: %v\n%s", strings.Join(arguments, " "), directory, err, output)
	}
	return string(output)
}
