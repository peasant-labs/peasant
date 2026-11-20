{
  description = "Reproducible Go dev environment with build and test support";

  # ============================================================
  # INPUTS
  # ============================================================

  inputs = rec {
    nixpkgs-stable.url = "github:NixOS/nixpkgs/nixos-25.11";
    nixpkgs-unstable.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixpkgs = nixpkgs-unstable;
    # ast-grep 0.51.2 is newer than both channel pins; keep it on its own input
    # so an ordinary `nix flake update` does not move the guard's required tool.
    nixpkgs-ast-grep.url = "github:NixOS/nixpkgs/34209e4f67c988d6a21b19f4ae37672fce52c7da";
    flake-utils.url = "github:numtide/flake-utils";
  };

  # ============================================================
  # OUTPUTS
  # ============================================================

  outputs =
    inputs@{ self
    , nixpkgs
    , nixpkgs-stable
    , nixpkgs-unstable
    , nixpkgs-ast-grep
    , flake-utils
    , ...
    }:
    let
      # ==========================================================
      # PROJECT CONFIGURATION — edit this section for your project
      # ==========================================================

      # Package metadata
      pname = "peasant";

      # Version is derived from the git short rev: this is a -dev/source build,
      # NOT a tagged release (tagged release binaries come from goreleaser with
      # -X ...version=<tag>). self.shortRev exists only for a clean tree; fall
      # back to dirtyShortRev, then a literal, so `nix build` works on a dirty
      # working tree too. This string is injected into internal/defaults.version
      # via ldflags below (shortRev + ldflags).
      version = self.shortRev or self.dirtyShortRev or "dev";

      # Go package attribute (e.g., go, go_1_24)
      # Set to null to use the default Go version from nixpkgs
      goAttr = null;

      # Vendor hash for buildGoModule. Recompute when go.mod/go.sum changes:
      # set this to nixpkgs.lib.fakeHash, run `nix build`, copy the reported `got:`
      # hash back here.
      vendorHash = "sha256-yNMvBl0XNx6IIjNc1FmtuKpimpgThZmICbqhIciBBq0=";

      # Extra CLI tools available in the dev shell
      devTools = pkgs: with pkgs; [
        gopls # LSP
        gotools # goimports, godoc, etc.
        go-tools # staticcheck
        delve # debugger
        golangci-lint # linter suite
        sqlite # CLI for inspecting analytics store
        goreleaser # validate .goreleaser.yml (`goreleaser check`) + local --snapshot builds
        actionlint # lint GitHub Actions workflow YAML (.github/workflows/*.yml)
        charm-freeze # Go-native ANSI terminal screenshot renderer (binary: freeze)
        nodejs_26 # Node.js runtime for the web build and validation scripts
        pnpm # required package manager for the web dependency graph
        typescript
        typescript-language-server
        # protobuf           # protoc
        # protoc-gen-go      # protobuf Go codegen
        # temporal-cli       # Temporal dev server
        # tlaplus18          # TLC model checker
      ] ++ pkgs.lib.optionals pkgs.stdenv.isLinux [
        pkgs.util-linux # persistent-inode flock for the focused built-binary journey
        pkgs.gnutar # integrity-verified registry artifact inventory and extraction
        pkgs.chromium # explicit CHROME_PATH; the runner never downloads a browser
      ];

      # Native build dependencies (C libraries, system packages)
      # gcc is required for tree-sitter CGo bindings (github.com/tree-sitter/go-tree-sitter)
      nativeBuildDeps = pkgs: with pkgs; [
        gcc
        # git: internal/gitops tests exec a real `git` (init/commit/merge in
        # throwaway repos, with repo-local identity) — provided here so they run
        # in the hermetic checkPhase instead of being excluded from it.
        git
        # pkg-config
        # openssl
        # sqlite
      ];

      # Extra check commands run during `nix build` after go test
      extraCheckPhase = ''
        # go vet ./...
        # staticcheck ./...
      '';

      # Files to install alongside the binary (relative to src)
      extraInstallPhase = ''
        # mkdir -p $out/share/policies
        # cp authz/policies/*.rego $out/share/policies/
      '';

      # ==========================================================
      # IMPLEMENTATION — you shouldn't need to edit below here
      # ==========================================================

      mkOutputs = nixpkgs-channel:
        flake-utils.lib.eachDefaultSystem (system:
          let
            pkgs = import nixpkgs-channel {
              inherit system;
              config.allowUnfree = true;
            };

            astGrepPkgs = import nixpkgs-ast-grep { inherit system; };
            ast-grep = assert astGrepPkgs.ast-grep.version == "0.51.2"; astGrepPkgs.ast-grep;

            goPackage =
              if goAttr != null
              then pkgs.${goAttr}
              else pkgs.go;

            # ----------------------------------------------------------
            # Build
            # ----------------------------------------------------------

            package = pkgs.buildGoModule {
              inherit pname version;
              src = ./.;
              inherit vendorHash;

              # Build ONLY the peasant CLI. The default `./...` enumeration would
              # also build the dev/CI-only command cmd/gen-mock-redactions,
              # which is not part of the shipped binary.
              subPackages = [ "cmd/peasant" ];

              # Build the same way the release binaries do: CGO disabled, so the
              # output is a static, portable binary. tree-sitter (the cgo-only
              # Maximum-redaction backend) is therefore NOT linked; `peasant
              # … --redaction maximum` returns the actionable hard error from
              # the public redaction module (redact.MaximumAvailable == false), consistent with
              # the goreleaser/distro builds. -race in checkPhase is removed
              # below for the same reason (the race detector requires cgo).
              env.CGO_ENABLED = 0;

              # Inject the version into internal/defaults.version (matches the
              # release ldflags) and strip debug info for a smaller binary.
              ldflags = [
                "-s"
                "-w"
                "-X github.com/peasant-labs/peasant/internal/defaults.version=${version}"
              ];

              # The binary embeds web/out via `//go:embed all:web/out`
              # (embed.go). A committed placeholder (web/out/.gitkeep) already
              # satisfies it, but `src = ./.` would also capture any stale local
              # `make web` output, making the build non-deterministic. Reset
              # web/out to the placeholder alone so the nix build never bundles
              # a developer's local front-end build; the binary then serves
              # internal/htmlpage's not-bundled notice in place of the local app.
              postPatch = ''
                rm -rf web/out
                mkdir -p web/out
                touch web/out/.gitkeep
              '';

              nativeBuildInputs = nativeBuildDeps pkgs;

              # No -race: the data-race detector requires cgo, and this build is
              # CGO_ENABLED=0. The authoritative race-enabled FULL suite runs in
              # `make check` / CI (incl. the CGO=0 leg).
              #
              # Two packages remain excluded because they are environment-dependent
              # INTEGRATION suites that cannot run in nix's hermetic build sandbox
              # (they pass in `make check` / CI, which is the real gate):
              #   - internal/e2e: resolves its testdata via runtime.Caller, which
              #     buildGoModule's `-trimpath` rewrites to a module-relative path
              #     that does not exist at test time → fixture-not-found.
              #   - internal/ingest: a broad integration surface — real `git`
              #     across most of its test files plus host-path assumptions
              #     (e.g. /home/... slug resolution) — not yet audited for
              #     sandbox-hermeticity; git alone does not make it green.
              # internal/gitops now RUNS here: its tests exec `git` but are
              # otherwise hermetic (self-created temp repos, repo-local identity),
              # so providing `git` (nativeBuildDeps) is sufficient. preCheck sets a
              # writable HOME + GIT_CONFIG_NOSYSTEM so git never reads the builder's
              # system config (reproducibility).
              # This checkPhase is a packaging sanity gate (including Peasant's
              # CGO=0 public-module redaction seam), not a substitute
              # for the full suite.
              #
              # -timeout=20m: Go's default per-package timeout is 10 minutes, and
              # the release runner (2 vCPU arm64, so `go test` runs 2 packages
              # and 2 parallel tests at a time) has run internal/api past it.
              # The SQLite-backed packages are bound by disk sync, so their time
              # varies several-fold between runner VMs of the same class: across
              # two runs of one commit internal/api took 377s and >600s, and
              # internal/store 118s and 429s. 20 minutes allows more time than
              # Go's 10-minute default, but the run recorded as >600s was not
              # observed completing, so it is unknown whether 20 minutes is
              # enough for it. It is no guarantee of a Go-level failure either:
              # the job's 30-minute wall limit also covers setup and the other
              # packages, so a package can start too late for -timeout to fire
              # first, and the job can be killed without a goroutine dump. When
              # the timeout does fire first, the dump names the hung test.
              #
              # -skip TestStopWebLeavesSameNamedExecutableAtAnotherPathAlone:
              # the test copies the test binary and executes the copy while
              # other parallel tests fork, so the exec can fail with ETXTBSY
              # ("text file busy", golang/go#22315). That is a test-harness race,
              # not a packaging defect, and it fails this gate at random. The
              # test still runs in `make check` and CI. Remove this skip when
              # peasant-labs/peasant#967 makes the copy-then-exec step retry.
              preCheck = ''
                export HOME=$(mktemp -d)
                export GIT_CONFIG_NOSYSTEM=1
              '';
              checkPhase = ''
                runHook preCheck
                go test -timeout=20m \
                  -skip '^TestStopWebLeavesSameNamedExecutableAtAnotherPathAlone$' \
                  $(go list ./... | grep -vE '/internal/(e2e|ingest)$')
                ${extraCheckPhase}
                runHook postCheck
              '';

              postInstall = extraInstallPhase;

              meta = with pkgs.lib; {
                description = "Local-first coding-agent transcript analytics, redaction, and publishing CLI";
                homepage = "https://github.com/peasant-labs/peasant";
                # Apache-2.0 — a standard SPDX license, so this is nixpkgs' own
                # `licenses.asl20` attrset (free = true) rather than a custom one.
                # Keep this in sync with the committed LICENSE and the license ids
                # declared in .goreleaser.yml (nfpm / AUR / Homebrew cask).
                license = licenses.asl20;
                mainProgram = "peasant";
              };
            };

            # ----------------------------------------------------------
            # Development Shell
            # ----------------------------------------------------------

            devShell = pkgs.mkShell {
              name = "${pname}-dev";
              inputsFrom = [ package ];
              packages = (devTools pkgs) ++ [ ast-grep ];

              shellHook = ''
                echo "Go $(go version | cut -d' ' -f3) dev shell"
                export CGO_ENABLED=1
                source .envrc.local
              '';
            };

          in
          {
            packages.default = package;
            packages.${pname} = package;

            devShells.default = devShell;

            # Quick check: nix flake check
            checks.build = package;
          }
        );
    in
    mkOutputs nixpkgs;
}
