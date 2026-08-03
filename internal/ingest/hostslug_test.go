package ingest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

func TestDeriveHostSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		remote       string
		fallbackPath string
		want         ingest.HostSlug
		wantErr      bool
		errMsg       string
	}{
		// SSH remotes
		{"SSH github", "git@github.com:acme-dev/peasant.git", "", "github.com--acme-dev--peasant", false, ""},
		{"SSH gitlab", "git@gitlab.com:acme-dev/project.git", "", "gitlab.com--acme-dev--project", false, ""},
		{"SSH no .git suffix", "git@github.com:user/repo", "", "github.com--user--repo", false, ""},

		// SSH URL format (with port)
		{"SSH URL with port", "ssh://git@github.com:22/user/repo.git", "", "github.com--user--repo", false, ""},
		{"SSH URL no port", "ssh://git@gitlab.com/acme-dev/project.git", "", "gitlab.com--acme-dev--project", false, ""},

		// HTTPS remotes
		{"HTTPS github", "https://github.com/acme-dev/peasant.git", "", "github.com--acme-dev--peasant", false, ""},
		{"HTTPS gitlab", "https://gitlab.com/acme-dev/project.git", "", "gitlab.com--acme-dev--project", false, ""},
		{"HTTPS no .git suffix", "https://github.com/user/repo", "", "github.com--user--repo", false, ""},
		{"HTTPS with port", "https://github.com:443/user/repo.git", "", "github.com--user--repo", false, ""},

		// Nested paths (e.g. GitLab subgroups)
		{"HTTPS nested path", "https://gitlab.com/group/subgroup/repo.git", "", "gitlab.com--group--subgroup--repo", false, ""},

		// Host lowercasing (via NormalizeRemoteURL)
		{"uppercase host SCP", "git@GITHUB.COM:user/repo.git", "", "github.com--user--repo", false, ""},
		{"uppercase host HTTPS", "https://GITLAB.COM/user/repo.git", "", "gitlab.com--user--repo", false, ""},

		// Fallback (no remote) — format: __peasant-untracked__--{hash8}--{basename}
		// hash8 values computed with zero salt (salt.Salt{}) via HMAC-SHA256.
		{"fallback absolute path", "", "/home/user/dev/my-project", "__peasant-untracked__--630ca830--my-project", false, ""},
		{"fallback tmp", "", "/tmp/scratch", "__peasant-untracked__--37cb0e3c--scratch", false, ""},
		{"fallback deep path", "", "/home/user/dev/org/project", "__peasant-untracked__--24d1dc40--project", false, ""},

		// Fallback with special characters — basename sanitized; unsafe chars become '-'.
		{"fallback spaces", "", "/home/user/My Projects/foo", "__peasant-untracked__--3799b05f--foo", false, ""},
		{"fallback special chars", "", "/home/user/a@b#c/project", "__peasant-untracked__--a54d894b--project", false, ""},

		// git:// remotes (M5)
		{"git protocol", "git://github.com/user/repo.git", "", "github.com--user--repo", false, ""},
		{"git protocol no suffix", "git://gitlab.com/group/project", "", "gitlab.com--group--project", false, ""},
		{"git protocol nested", "git://example.com/org/sub/repo.git", "", "example.com--org--sub--repo", false, ""},

		// file:// remotes (M5) — treated as local paths via deriveHostSlugFromPath.
		{"file protocol", "file:///home/user/repos/myproject", "", "__peasant-untracked__--e89dac26--myproject", false, ""},
		{"file protocol bare repo", "file:///srv/git/repo.git", "", "__peasant-untracked__--d361f793--repo.git", false, ""},

		// Errors
		{"both empty", "", "", "", true, ""},
		{"relative fallback", "", "dev/relative", "", true, "absolute"},
		{"fallback just dot", "", ".", "", true, "absolute"},
	}

	// Salt is not used for slug derivation (only for hash); use zero salt.
	s := salt.Salt{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ingest.DeriveHostSlug(s, tt.remote, tt.fallbackPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("DeriveHostSlug(%q, %q) = %q, want error", tt.remote, tt.fallbackPath, got)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("DeriveHostSlug(%q, %q) error = %q, want containing %q", tt.remote, tt.fallbackPath, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("DeriveHostSlug(%q, %q) unexpected error: %v", tt.remote, tt.fallbackPath, err)
				}
				if got != tt.want {
					t.Errorf("DeriveHostSlug(%q, %q) = %q, want %q", tt.remote, tt.fallbackPath, got, tt.want)
				}
			}
		})
	}
}

func TestDeriveProjectIdentifiers(t *testing.T) {
	t.Parallel()

	// Use a deterministic zero salt for all sub-tests so expected hashes are stable.
	s := salt.Salt{}

	hashOf := func(input string) ingest.ProjectHash {
		ph, err := s.Hash(input)
		if err != nil {
			panic("salt.Hash: " + err.Error())
		}
		return ph
	}

	tests := []struct {
		name         string
		remote       string
		fallbackPath string
		wantHash     ingest.ProjectHash
		wantSlug     ingest.HostSlug
		wantErr      bool
	}{
		{
			// NormalizeRemoteURL("git@github.com:user/repo.git") -> "github.com/user/repo"
			name:         "remote URL hashed using normalized form",
			remote:       "git@github.com:user/repo.git",
			fallbackPath: "/home/user/repo",
			wantHash:     hashOf("github.com/user/repo"),
			wantSlug:     "github.com--user--repo",
		},
		{
			name:         "fallback path hashed when no remote",
			remote:       "",
			fallbackPath: "/home/user/myproject",
			wantHash:     hashOf("/home/user/myproject"),
			wantSlug:     "__peasant-untracked__--157ab3f9--myproject",
		},
		{
			// NormalizeRemoteURL("https://github.com/org/project.git") -> "github.com/org/project"
			name:         "returns typed ProjectHash",
			remote:       "https://github.com/org/project.git",
			fallbackPath: "/tmp/project",
			wantHash:     hashOf("github.com/org/project"),
			wantSlug:     "github.com--org--project",
		},
		{
			name:         "unrecognized remote falls back to path slug and hash",
			remote:       "ftp://example.com/repo.git",
			fallbackPath: "/home/user/repo",
			wantHash:     hashOf("/home/user/repo"),
			wantSlug:     "__peasant-untracked__--cd6caf16--repo",
		},
		{
			name:         "both empty errors",
			remote:       "",
			fallbackPath: "",
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hash, slug, err := ingest.DeriveProjectIdentifiers(s, tt.remote, tt.fallbackPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("DeriveProjectIdentifiers(%q, %q) = (%q, %q, nil), want error",
						tt.remote, tt.fallbackPath, hash, slug)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveProjectIdentifiers(%q, %q) unexpected error: %v",
					tt.remote, tt.fallbackPath, err)
			}
			if hash != tt.wantHash {
				t.Errorf("hash = %q, want %q", hash, tt.wantHash)
			}
			if slug != tt.wantSlug {
				t.Errorf("slug = %q, want %q", slug, tt.wantSlug)
			}
		})
	}
}

// TestDeriveProjectIdentifiers_SSHandHTTPSSameHash verifies that SSH and HTTPS
// URLs for the same repository produce the same ProjectHash (the primary
// project-identity invariant).
func TestDeriveProjectIdentifiers_SSHandHTTPSSameHash(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}

	ssh := "git@github.com:user/repo.git"
	https := "https://github.com/user/repo.git"
	fallback := "/home/user/repo"

	sshHash, _, err := ingest.DeriveProjectIdentifiers(s, ssh, fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers SSH: %v", err)
	}
	httpsHash, _, err := ingest.DeriveProjectIdentifiers(s, https, fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers HTTPS: %v", err)
	}

	if sshHash != httpsHash {
		t.Errorf("SSH and HTTPS for same repo produced different hashes: SSH=%q HTTPS=%q", sshHash, httpsHash)
	}
}

// TestDeriveProjectIdentifiers_DifferentSaltsDifferentHashes verifies that
// two different salts produce different ProjectHashes for the same repo.
func TestDeriveProjectIdentifiers_DifferentSaltsDifferentHashes(t *testing.T) {
	t.Parallel()
	var s1 salt.Salt
	var s2 salt.Salt
	s2[0] = 0xFF // make s2 different from zero salt

	remote := "git@github.com:user/repo.git"
	fallback := "/home/user/repo"

	h1, _, err := ingest.DeriveProjectIdentifiers(s1, remote, fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers s1: %v", err)
	}
	h2, _, err := ingest.DeriveProjectIdentifiers(s2, remote, fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiers s2: %v", err)
	}

	if h1 == h2 {
		t.Errorf("different salts produced same hash %q for remote %q", h1, remote)
	}
}

// TestUntrackedSlugFormat verifies the fixed-width hash prefix format for
// untracked slugs: __peasant-untracked__--{hash8}--{basename}.
func TestUntrackedSlugFormat(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}

	tests := []struct {
		name         string
		fallbackPath string
		wantBasename string // expected basename component in slug
	}{
		{
			name:         "simple project name",
			fallbackPath: "/home/user/dev/myproject",
			wantBasename: "myproject",
		},
		{
			name:         "project with dash",
			fallbackPath: "/home/user/dev/my-project",
			wantBasename: "my-project",
		},
		{
			name:         "project with spaces — sanitized to dashes",
			fallbackPath: "/home/user/project with spaces",
			wantBasename: "project-with-spaces",
		},
		{
			name:         "deep path — only basename used",
			fallbackPath: "/home/user/org/sub/deep/myproject",
			wantBasename: "myproject",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ingest.DeriveHostSlug(s, "", tt.fallbackPath)
			if err != nil {
				t.Fatalf("DeriveHostSlug error: %v", err)
			}

			slugStr := string(got)

			// Verify prefix
			const prefix = "__peasant-untracked__--"
			if !strings.HasPrefix(slugStr, prefix) {
				t.Errorf("slug %q does not start with %q", slugStr, prefix)
				return
			}
			rest := slugStr[len(prefix):]

			// Split on first "--" to extract hash8 and basename components.
			sep := strings.Index(rest, "--")
			if sep == -1 {
				t.Errorf("slug %q: expected hash8--basename after prefix, no '--' found", slugStr)
				return
			}
			gotHash8 := rest[:sep]
			gotBasename := rest[sep+2:]

			// hash8 must be exactly 8 hex characters.
			if len(gotHash8) != 8 {
				t.Errorf("slug %q: hash8 component %q has length %d, want 8", slugStr, gotHash8, len(gotHash8))
			}
			for _, c := range gotHash8 {
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
					t.Errorf("slug %q: hash8 %q contains non-hex character %q", slugStr, gotHash8, string(c))
					break
				}
			}

			// basename must match expected.
			if gotBasename != tt.wantBasename {
				t.Errorf("slug %q: basename = %q, want %q", slugStr, gotBasename, tt.wantBasename)
			}
		})
	}
}

// TestUntrackedSlugDeterminism verifies that the same path and salt always
// produce the same slug (HMAC is deterministic).
func TestUntrackedSlugDeterminism(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}
	path := "/home/user/dev/myproject"

	slug1, err := ingest.DeriveHostSlug(s, "", path)
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	slug2, err := ingest.DeriveHostSlug(s, "", path)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}

	if slug1 != slug2 {
		t.Errorf("non-deterministic: got %q then %q for same path+salt", slug1, slug2)
	}
}

// TestUntrackedSlugDifferentSaltsProduceDifferentHash8 verifies that two
// different salts produce different hash8 prefixes for the same path.
func TestUntrackedSlugDifferentSaltsProduceDifferentHash8(t *testing.T) {
	t.Parallel()
	var s1 salt.Salt
	var s2 salt.Salt
	s2[0] = 0xFF // make s2 different from zero salt

	path := "/home/user/dev/myproject"

	slug1, err := ingest.DeriveHostSlug(s1, "", path)
	if err != nil {
		t.Fatalf("s1 error: %v", err)
	}
	slug2, err := ingest.DeriveHostSlug(s2, "", path)
	if err != nil {
		t.Fatalf("s2 error: %v", err)
	}

	if slug1 == slug2 {
		t.Errorf("different salts produced identical slugs %q for path %q", slug1, path)
	}
}

// --- DeriveProjectIdentifiersWithGit tests ---

// TestDeriveProjectIdentifiersWithGit_RemoteProvided verifies that when a remote
// is already known, the walk-up is skipped and the remote-based hash is used.
func TestDeriveProjectIdentifiersWithGit_RemoteProvided(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}
	remote := "git@github.com:user/repo.git"
	fallback := "/home/user/repo"

	// StubGitResolver with walk-up configured — should NOT be called because
	// remote is non-empty.
	git := &testutil.StubGitResolver{
		WalkUpRemoteResult: [2]string{"git@github.com:other/other.git", "/other"},
	}

	hash, slug, err := ingest.DeriveProjectIdentifiersWithGit(context.Background(), s, git, remote, fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiersWithGit: %v", err)
	}

	// Should match the result from the directly-provided remote, not the walk-up result.
	wantHash, wantSlug, _ := ingest.DeriveProjectIdentifiers(s, remote, fallback)
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
	if slug != wantSlug {
		t.Errorf("slug = %q, want %q", slug, wantSlug)
	}
}

// TestDeriveProjectIdentifiersWithGit_WalkUpFindsRemote verifies that when the
// initial remote is empty, WalkUpRemoteURL is called and its result is used.
func TestDeriveProjectIdentifiersWithGit_WalkUpFindsRemote(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}
	fallback := "/home/user/repo"
	walkedRemote := "git@github.com:walked/repo.git"

	git := &testutil.StubGitResolver{
		WalkUpRemoteResult: [2]string{walkedRemote, "/home/user/repo"},
	}

	hash, slug, err := ingest.DeriveProjectIdentifiersWithGit(context.Background(), s, git, "", fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiersWithGit: %v", err)
	}

	// Should match the walked remote's hash/slug, not the path-based fallback.
	wantHash, wantSlug, _ := ingest.DeriveProjectIdentifiers(s, walkedRemote, fallback)
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
	if slug != wantSlug {
		t.Errorf("slug = %q, want %q", slug, wantSlug)
	}
}

// TestDeriveProjectIdentifiersWithGit_NoRemoteAnywhere verifies that when walk-up
// finds nothing, the function falls back to path-based hashing (same as DeriveProjectIdentifiers).
func TestDeriveProjectIdentifiersWithGit_NoRemoteAnywhere(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}
	fallback := "/home/user/myproject"

	// Walk-up returns empty remote.
	git := &testutil.StubGitResolver{
		WalkUpRemoteResult: [2]string{"", ""},
	}

	hash, slug, err := ingest.DeriveProjectIdentifiersWithGit(context.Background(), s, git, "", fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiersWithGit: %v", err)
	}

	wantHash, wantSlug, _ := ingest.DeriveProjectIdentifiers(s, "", fallback)
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
	if slug != wantSlug {
		t.Errorf("slug = %q, want %q", slug, wantSlug)
	}
}

// TestDeriveProjectIdentifiersWithGit_NilGit verifies that a nil GitResolver
// falls back to path-based hashing without panicking.
func TestDeriveProjectIdentifiersWithGit_NilGit(t *testing.T) {
	t.Parallel()
	s := salt.Salt{}
	fallback := "/home/user/myproject"

	hash, slug, err := ingest.DeriveProjectIdentifiersWithGit(context.Background(), s, nil, "", fallback)
	if err != nil {
		t.Fatalf("DeriveProjectIdentifiersWithGit with nil git: %v", err)
	}

	wantHash, wantSlug, _ := ingest.DeriveProjectIdentifiers(s, "", fallback)
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
	if slug != wantSlug {
		t.Errorf("slug = %q, want %q", slug, wantSlug)
	}
}
