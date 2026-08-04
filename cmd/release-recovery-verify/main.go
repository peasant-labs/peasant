package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/peasant-labs/peasant/internal/releaserecovery"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}

type runConfig struct {
	getenv     func(string) string
	httpClient *http.Client
	now        func() time.Time
	stdout     io.Writer
}

type runOption func(*runConfig)

func run(args []string, options ...runOption) error {
	config := runConfig{
		getenv:     os.Getenv,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
		stdout:     os.Stdout,
	}
	for _, option := range options {
		option(&config)
	}
	if len(args) == 0 {
		return fmt.Errorf("release recovery command is missing; why: no mode was provided; impact: verification cannot run; fix: use preflight, pre-publish, release-absent, or publishers-disabled")
	}
	if args[0] == "publishers-disabled" {
		if len(args) != 2 {
			return fmt.Errorf("publishers-disabled requires one .goreleaser.yml path; why: got %d arguments; impact: external publisher safety cannot be checked; fix: pass the immutable release-source config path", len(args)-1)
		}
		if err := releaserecovery.VerifyPublishersDisabled(args[1]); err != nil {
			return err
		}
		fmt.Fprintln(config.stdout, "::notice::AUR and Homebrew uploads are disabled in the immutable release source")
		return nil
	}

	verifier, err := releaserecovery.NewVerifier(releaserecovery.Config{
		APIURL:     config.getenv("GITHUB_API_URL"),
		Token:      config.getenv("GH_TOKEN"),
		HTTPClient: config.httpClient,
		Now:        config.now,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch args[0] {
	case "release-absent":
		if len(args) != 1 {
			return fmt.Errorf("release-absent accepts no arguments; why: got %d extra arguments; impact: the target is ambiguous; fix: configure the fixed recovery through environment only", len(args)-1)
		}
		if err := verifier.VerifyReleaseAbsent(ctx); err != nil {
			return err
		}
		fmt.Fprintln(config.stdout, "::notice::no draft or published GitHub Release exists for v0.1.0")
		return nil
	case "pre-publish":
		if len(args) != 1 {
			return fmt.Errorf("pre-publish accepts no arguments; why: got %d extra arguments; impact: the target is ambiguous; fix: use the reviewed environment contract", len(args)-1)
		}
		runID, err := requiredInt64(config.getenv, "RECOVERY_RUN_ID")
		if err != nil {
			return err
		}
		if err := verifier.VerifyPrePublish(ctx, releaserecovery.RecoveryRunInput{RunID: runID, HeadSHA: config.getenv("RECOVERY_HEAD_SHA")}); err != nil {
			return err
		}
		fmt.Fprintln(config.stdout, "::notice::immutable tag, sole dispatch, ruleset, and Release absence re-verified immediately before publication")
		return nil
	case "preflight":
		if len(args) != 1 {
			return fmt.Errorf("preflight accepts no arguments; why: got %d extra arguments; impact: the recovery inputs are ambiguous; fix: use the reviewed environment contract", len(args)-1)
		}
		input, err := preflightInput(config.getenv)
		if err != nil {
			return err
		}
		if err := verifier.VerifyPreflight(ctx, input); err != nil {
			return err
		}
		fmt.Fprintln(config.stdout, "::notice::immutable v0.1.0 recovery evidence verified")
		return nil
	default:
		return fmt.Errorf("unknown release recovery command %q; why: the mode is not part of the reviewed interface; impact: verification cannot run; fix: use preflight, pre-publish, release-absent, or publishers-disabled", args[0])
	}
}

func preflightInput(getenv func(string) string) (releaserecovery.PreflightInput, error) {
	runID, err := requiredInt64(getenv, "RECOVERY_RUN_ID")
	if err != nil {
		return releaserecovery.PreflightInput{}, err
	}
	attempt, err := requiredInt(getenv, "RECOVERY_RUN_ATTEMPT")
	if err != nil {
		return releaserecovery.PreflightInput{}, err
	}
	actorID, err := requiredInt64(getenv, "RECOVERY_ACTOR_ID")
	if err != nil {
		return releaserecovery.PreflightInput{}, err
	}
	e2eRunID, err := requiredInt64(getenv, "E2E_RUN_ID")
	if err != nil {
		return releaserecovery.PreflightInput{}, err
	}
	releaseE2ERunID, err := requiredInt64(getenv, "RELEASE_E2E_RUN_ID")
	if err != nil {
		return releaserecovery.PreflightInput{}, err
	}
	return releaserecovery.PreflightInput{
		Repository:              getenv("RECOVERY_REPOSITORY"),
		EventName:               getenv("RECOVERY_EVENT"),
		Ref:                     getenv("RECOVERY_REF"),
		RunID:                   runID,
		RunAttempt:              attempt,
		Actor:                   getenv("RECOVERY_ACTOR"),
		ActorID:                 actorID,
		ConfirmationTag:         getenv("CONFIRM_TAG"),
		ConfirmationSHA:         getenv("CONFIRM_COMMIT"),
		RecoveryHeadSHA:         getenv("RECOVERY_HEAD_SHA"),
		ConfirmationRecoverySHA: getenv("CONFIRM_RECOVERY_COMMIT"),
		E2ERunID:                e2eRunID,
		ReleaseE2ERunID:         releaseE2ERunID,
	}, nil
}

func requiredInt64(getenv func(string) string, name string) (int64, error) {
	value := getenv(name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("release recovery environment %s is invalid; why: %q is not a positive integer; impact: a GitHub receipt cannot be bound; fix: pass the exact reviewed run or actor ID", name, value)
	}
	return parsed, nil
}

func requiredInt(getenv func(string) string, name string) (int, error) {
	value, err := requiredInt64(getenv, name)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}
