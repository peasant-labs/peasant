{
  description = "Reproducible Go dev environment with build and test support";

  # ============================================================
  # INPUTS
  # ============================================================

  inputs = rec {
    nixpkgs-stable.url = "github:NixOS/nixpkgs/nixos-25.11";
    nixpkgs-unstable.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixpkgs = nixpkgs-unstable;
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
      vendorHash = "sha256-69pFZKIDJDNDysHWso9YbSlLyydjEo6ZZcSLaPwY/go=";

      # Extra CLI tools available in the dev shell
      devTools = pkgs: with pkgs; [
        gopls # LSP
        gotools # goimports, godoc, etc.
        go-tools # staticcheck
        delve # debugger
        ast-grep # structural code search and lint
        golangci-lint # linter suite
        sqlite # CLI for inspecting analytics store
        goreleaser # validate .goreleaser.yml (`goreleaser check`) + local --snapshot builds
        actionlint # lint GitHub Actions workflow YAML (.github/workflows/*.yml)
        charm-freeze # Go-native ANSI terminal screenshot renderer (binary: freeze)
        nodejs_24 # Node.js runtime for the web build and validation scripts
        pnpm # required package manager for the web dependency graph
        typescript
        typescript-language-server
        # protobuf           # protoc
        # protoc-gen-go      # protobuf Go codegen
        # temporal-cli       # Temporal dev server
        # tlaplus18          # TLC model checker
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
              # web/out to a deterministic stub so the nix build never bundles a
              # developer's local front-end build.
              postPatch = ''
                rm -rf web/out
                mkdir -p web/out
                cat > web/out/index.html <<'HTML'
                <!doctype html>
                <title>Peasant — dashboard assets not bundled</title>
                <p>This nix build does not bundle the web dashboard front-end. The CLI works fully; build from source with <code>make build</code> for the embedded UI.</p>
                HTML
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
              preCheck = ''
                export HOME=$(mktemp -d)
                export GIT_CONFIG_NOSYSTEM=1
              '';
              checkPhase = ''
                runHook preCheck
                go test $(go list ./... | grep -vE '/internal/(e2e|ingest)$')
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
              packages = (devTools pkgs);

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
