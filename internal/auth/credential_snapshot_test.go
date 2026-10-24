package auth

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/credential_observations.yaml
var credentialObservationFixtures []byte

type credentialFixtures struct {
	Generation []struct {
		Name    string   `yaml:"name"`
		Actions []string `yaml:"actions"`
		Changed bool     `yaml:"changed"`
	} `yaml:"generation"`
	Mutations []struct {
		Name         string `yaml:"name"`
		Action       string `yaml:"action"`
		PresentAfter bool   `yaml:"presentAfter"`
	} `yaml:"mutations"`
	Profiles []struct {
		Name          string `yaml:"name"`
		OtherUsername string `yaml:"otherUsername"`
		OtherUserID   string `yaml:"otherUserId"`
	} `yaml:"profiles"`
	URLs []struct {
		Name       string `yaml:"name"`
		Input      string `yaml:"input"`
		Normalized string `yaml:"normalized"`
		Invalid    bool   `yaml:"invalid"`
	} `yaml:"urls"`
}

func loadCredentialFixtures(t *testing.T) credentialFixtures {
	t.Helper()
	var fixtures credentialFixtures
	if err := yaml.Unmarshal(credentialObservationFixtures, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, fixture := range fixtures.Generation {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate fixture name")
		}
		names[fixture.Name] = true
	}
	for _, fixture := range fixtures.URLs {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate fixture name")
		}
		names[fixture.Name] = true
	}
	for _, fixture := range fixtures.Mutations {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate fixture name")
		}
		if fixture.Action != "save" && fixture.Action != "clear" {
			t.Fatalf("unknown mutation %s", fixture.Action)
		}
		names[fixture.Name] = true
	}
	for _, fixture := range fixtures.Profiles {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate fixture name")
		}
		names[fixture.Name] = true
	}
	const required = "unchanged-generation-commits credential-rotation-refuses-old-response account-switch-back-refuses-old-response logout-refuses-old-response logout-login-same-account-refuses-old-response failed-save-before-replacement-preserves-snapshot cross-process-save-waits-for-commit cross-process-clear-waits-for-commit captured-profile-remains-pinned-after-environment-change full-base-path-preserved same-host-different-base-path url-secrets-not-scope nondefault-port-preserved explicit-loopback-development ipv6-default-port insecure-remote-refused relative-url-refused ambiguous-parent-path-refused encoded-separator-refused ambiguous-empty-path-segment-refused unsupported-scheme-refused"
	for _, name := range strings.Fields(required) {
		if !names[name] {
			t.Fatalf("required fixture %s missing", name)
		}
	}
	return fixtures
}

func observationCredentials() Credentials {
	return Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.example/team"}
}

func TestCredentialSnapshotGeneration(t *testing.T) {
	for _, fixture := range loadCredentialFixtures(t).Generation {
		t.Run(fixture.Name, func(t *testing.T) {
			home := t.TempDir()
			creds := observationCredentials()
			if err := SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := CaptureCredentials(context.Background(), home)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range fixture.Actions {
				switch action {
				case "save-same":
					err = SaveCredentialsFrom(&creds, home)
				case "save-other":
					other := creds
					other.UserID = "22222222-2222-4222-8222-222222222222"
					err = SaveCredentialsFrom(&other, home)
				case "clear":
					err = ClearCredentialsFrom(home)
				case "save-invalid-time":
					invalid := creds
					invalid.LinkedAt = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
					if err := SaveCredentialsFrom(&invalid, home); err == nil || !strings.Contains(err.Error(), "marshal credentials") {
						t.Fatalf("save must fail before replacement at JSON encoding: %v", err)
					}
				default:
					t.Fatalf("unknown action %s", action)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			committed := false
			err = snapshot.WithCurrent(context.Background(), func() error { committed = true; return nil })
			if fixture.Changed {
				if !errors.Is(err, ErrCredentialsChanged) || committed {
					t.Fatalf("obsolete generation committed=%v err=%v", committed, err)
				}
			} else if err != nil || !committed {
				t.Fatalf("current generation committed=%v err=%v", committed, err)
			}
		})
	}
}

func TestCredentialSnapshotProfilePinning(t *testing.T) {
	for _, fixture := range loadCredentialFixtures(t).Profiles {
		t.Run(fixture.Name, func(t *testing.T) {
			firstHome, otherHome := t.TempDir(), t.TempDir()
			creds := observationCredentials()
			if err := SaveCredentialsFrom(&creds, firstHome); err != nil {
				t.Fatal(err)
			}
			other := creds
			other.Username, other.UserID = fixture.OtherUsername, fixture.OtherUserID
			if err := SaveCredentialsFrom(&other, otherHome); err != nil {
				t.Fatal(err)
			}
			t.Setenv(defaults.EnvXDGConfigHome.String(), firstHome)
			snapshot, err := CaptureCredentials(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(defaults.EnvXDGConfigHome.String(), otherHome)
			if selected, err := LoadCredentials(); err != nil || selected == nil || *selected != other {
				t.Fatalf("environment did not select the other profile: %v", err)
			}
			if snapshot.Credentials() != creds {
				t.Fatal("captured profile changed after environment selection")
			}
			committed := false
			if err := snapshot.WithCurrent(context.Background(), func() error { committed = true; return nil }); err != nil || !committed {
				t.Fatalf("pinned first profile was not used: committed=%v err=%v", committed, err)
			}
			// Rotating A must still invalidate its snapshot even while B is selected.
			if err := SaveCredentialsFrom(&creds, firstHome); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.WithCurrent(context.Background(), func() error { t.Fatal("rotated pinned profile committed"); return nil }); !errors.Is(err, ErrCredentialsChanged) {
				t.Fatalf("pinned profile generation was ignored: %v", err)
			}
		})
	}
}

func TestNormalizeVillageURL(t *testing.T) {
	for _, fixture := range loadCredentialFixtures(t).URLs {
		t.Run(fixture.Name, func(t *testing.T) {
			got, err := NormalizeVillageURL(fixture.Input)
			if fixture.Invalid {
				if err == nil {
					t.Fatal("ambiguous scope accepted")
				}
				return
			}
			if err != nil || got != fixture.Normalized {
				t.Fatalf("normalized=%q want=%q err=%v", got, fixture.Normalized, err)
			}
		})
	}
}

// A real child process must contend on the OS lock, not an in-memory mutex.
// Its deadline expires while the parent's transaction callback is active.
func TestCredentialCommitGuardCrossProcess(t *testing.T) {
	if home := os.Getenv("PEASANT_CREDENTIAL_GUARD_CHILD"); home != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := CaptureCredentials(ctx, home)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cross-process lock did not protect commit: %v", err)
		}
		fmt.Println("deadline-proved-lock")
		return
	}
	home := t.TempDir()
	creds := observationCredentials()
	if err := SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureCredentials(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = snapshot.WithCurrent(ctx, func() error {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCredentialCommitGuardCrossProcess$")
		cmd.Env = append(os.Environ(), "PEASANT_CREDENTIAL_GUARD_CHILD="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("child: %w: %s", err, out)
		}
		if !strings.Contains(string(out), "deadline-proved-lock") {
			return fmt.Errorf("child did not prove contention: %s", out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCredentialMutationWaitsForCommit(t *testing.T) {
	if home := os.Getenv("PEASANT_CREDENTIAL_MUTATION_CHILD"); home != "" {
		fmt.Println("ready")
		action, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("attempting")
		switch strings.TrimSpace(action) {
		case "save":
			creds := observationCredentials()
			err = SaveCredentialsFrom(&creds, home)
		case "clear":
			err = ClearCredentialsFrom(home)
		default:
			t.Fatalf("unsupported child mutation: %q", action)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, fixture := range loadCredentialFixtures(t).Mutations {
		t.Run(fixture.Name, func(t *testing.T) {
			home := t.TempDir()
			creds := observationCredentials()
			if err := SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := CaptureCredentials(context.Background(), home)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCredentialMutationWaitsForCommit$")
			cmd.Env = append(os.Environ(), "PEASANT_CREDENTIAL_MUTATION_CHILD="+home)
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			done := make(chan struct{})
			var waitErr error
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// One waiter owns reaping from the instant Start succeeds, including all
			// startup/assertion failures. Closed done can be observed by both assertions
			// and cleanup without consuming a one-shot result or calling Wait twice.
			go func() { waitErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				_ = in.Close()
				select {
				case <-done:
					return
				default:
					_ = cmd.Process.Kill()
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("credential mutation child was not reaped before cleanup deadline")
				}
			})
			reader := bufio.NewReader(out)
			if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child startup: %q %v", line, err)
			}
			if err := snapshot.WithCurrent(ctx, func() error {
				if _, err := fmt.Fprintln(in, fixture.Action); err != nil {
					return err
				}
				if line, err := reader.ReadString('\n'); err != nil || line != "attempting\n" {
					return fmt.Errorf("child did not reach mutation: %q %v", line, err)
				}
				// Wait is bounded to prove real save/clear cannot finish before commit.
				select {
				case <-done:
					return fmt.Errorf("%s escaped active commit: %v", fixture.Action, waitErr)
				case <-time.After(100 * time.Millisecond):
					selected, err := LoadCredentialsFrom(home)
					if err != nil || selected == nil || *selected != creds {
						return fmt.Errorf("credentials changed inside active commit during %s: %v", fixture.Action, err)
					}
					return nil
				}
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatal(waitErr)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			selected, err := LoadCredentialsFrom(home)
			if err != nil || (selected != nil) != fixture.PresentAfter {
				t.Fatalf("credential presence after %s: present=%v want=%v err=%v", fixture.Action, selected != nil, fixture.PresentAfter, err)
			}
			if err := snapshot.WithCurrent(ctx, func() error { t.Fatal("old generation installed after concurrent mutation"); return nil }); !errors.Is(err, ErrCredentialsChanged) {
				t.Fatalf("generation was not rotated: %v", err)
			}
		})
	}
}

func TestCredentialSnapshotSignedOut(t *testing.T) {
	_, err := CaptureCredentials(context.Background(), t.TempDir())
	if !errors.Is(err, ErrSignedOut) {
		t.Fatalf("signed-out scope: %v", err)
	}
}
