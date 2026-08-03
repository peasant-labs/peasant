package ingest_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestNormalizeRemoteURL(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		input   string
		want    string
		wantErr bool
	}

	cases := []tc{
		// SCP format
		{
			name:  "SCP GitHub",
			input: "git@github.com:user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "SCP no .git suffix",
			input: "git@github.com:user/repo",
			want:  "github.com/user/repo",
		},
		{
			name:  "SCP GitLab subgroups",
			input: "git@gitlab.com:group/subgroup/repo.git",
			want:  "gitlab.com/group/subgroup/repo",
		},
		{
			name:  "SCP uppercase host — lowercased",
			input: "git@GITHUB.COM:user/repo.git",
			want:  "github.com/user/repo",
		},

		// HTTPS format
		{
			name:  "HTTPS with .git",
			input: "https://github.com/user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "HTTPS uppercase host",
			input: "https://GitHub.COM/user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "HTTPS no .git suffix",
			input: "https://github.com/user/repo",
			want:  "github.com/user/repo",
		},
		{
			name:  "HTTPS with auth credentials stripped",
			input: "https://user:pass@github.com/user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "HTTPS GitLab subgroups",
			input: "https://gitlab.com/group/subgroup/repo.git",
			want:  "gitlab.com/group/subgroup/repo",
		},

		// HTTP format
		{
			name:  "HTTP basic",
			input: "http://github.com/user/repo",
			want:  "github.com/user/repo",
		},
		{
			name:  "HTTP with .git",
			input: "http://github.com/user/repo.git",
			want:  "github.com/user/repo",
		},

		// SSH URL format
		{
			name:  "SSH with port 22",
			input: "ssh://git@gitlab.com:22/group/sub/repo",
			want:  "gitlab.com/group/sub/repo",
		},
		{
			name:  "SSH with port 443 and .git",
			input: "ssh://git@github.com:443/user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "SSH no port",
			input: "ssh://git@github.com/user/repo.git",
			want:  "github.com/user/repo",
		},

		// git:// format
		{
			name:  "git:// with .git",
			input: "git://github.com/user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "git:// no .git suffix",
			input: "git://github.com/user/repo",
			want:  "github.com/user/repo",
		},

		// Same repo via SSH and HTTPS must produce same canonical form
		{
			name:  "canonical SSH == HTTPS (github)",
			input: "git@github.com:user/repo.git",
			want:  "github.com/user/repo",
		},
		{
			name:  "canonical SSH == HTTPS (same want as above via HTTPS)",
			input: "https://github.com/user/repo.git",
			want:  "github.com/user/repo",
		},

		// Error cases
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "file:// URL",
			input:   "file:///home/user/repo",
			wantErr: true,
		},
		{
			name:    "unrecognized format — bare hostname",
			input:   "not-a-url",
			wantErr: true,
		},
		{
			name:    "unrecognized format — ftp scheme",
			input:   "ftp://github.com/user/repo",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ingest.NormalizeRemoteURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizeRemoteURL(%q): expected error, got %q", tc.input, got)
				}
				// Verify the error is actionable (non-empty, contains context)
				if err != nil && strings.TrimSpace(err.Error()) == "" {
					t.Errorf("NormalizeRemoteURL(%q): error message is empty", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRemoteURL(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeRemoteURL(%q):\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRepoNameFromRemote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "SCP remote",
			input: "git@gitlab.com:sample-org/widget-service.git",
			want:  "widget-service",
		},
		{
			name:  "HTTPS remote",
			input: "https://github.com/acme/my-project.git",
			want:  "my-project",
		},
		{
			name:  "SSH URL",
			input: "ssh://git@github.com:22/user/repo.git",
			want:  "repo",
		},
		{
			name:  "GitLab subgroup",
			input: "git@gitlab.com:group/subgroup/repo.git",
			want:  "repo",
		},
		{
			name:  "empty remote",
			input: "",
			want:  "",
		},
		{
			name:  "unparseable remote",
			input: "ftp://invalid/whatever",
			want:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ingest.RepoNameFromRemote(tc.input)
			if got != tc.want {
				t.Errorf("RepoNameFromRemote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestNormalizeRemoteURL_SameRepoProducesSameOutput verifies that SSH and HTTPS
// clones of the same repository produce identical canonical output. This is the
// primary invariant consumed by DeriveProjectIdentifiers.
func TestNormalizeRemoteURL_SameRepoProducesSameOutput(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		ssh   string
		https string
	}{
		{
			ssh:   "git@github.com:user/repo.git",
			https: "https://github.com/user/repo.git",
		},
		{
			ssh:   "git@gitlab.com:group/subgroup/repo.git",
			https: "https://gitlab.com/group/subgroup/repo.git",
		},
	}

	for _, p := range pairs {
		p := p
		t.Run(p.ssh, func(t *testing.T) {
			t.Parallel()
			sshOut, err := ingest.NormalizeRemoteURL(p.ssh)
			if err != nil {
				t.Fatalf("SSH variant %q: unexpected error: %v", p.ssh, err)
			}
			httpsOut, err := ingest.NormalizeRemoteURL(p.https)
			if err != nil {
				t.Fatalf("HTTPS variant %q: unexpected error: %v", p.https, err)
			}
			if sshOut != httpsOut {
				t.Errorf("SSH and HTTPS produce different output:\n  SSH  %q → %q\n  HTTPS %q → %q",
					p.ssh, sshOut, p.https, httpsOut)
			}
		})
	}
}
