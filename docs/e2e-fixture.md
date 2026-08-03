# E2E transcript fixtures

Committed fixtures under `internal/e2e/testdata/` exercise provider discovery,
indexing, redaction, publication, pull, and rendering. They are scanned by the
always-on `TestFixture_NoSecrets` gate and are also consumed by `make e2e`.

## Provenance and purpose

| Fixture | Provenance | Purpose |
|---------|------------|---------|
| `claude-fixture/` | Fully synthetic, hand-authored root and subagent conversations | Claude discovery, parent-child association, rendering, and Standard-versus-Maximum redaction |
| `codex-fixture/` | Fully synthetic, hand-authored provider-shaped rollouts | Multiple Codex sessions, tool event parsing, and model discovery |
| `cursor-fixture/` | Fully synthetic, hand-authored provider-shaped transcripts | Cursor discovery, indexing, and aborted-turn handling |
| `legacy-*.transcript.*` | Fully synthetic, hand-authored payloads | Legacy content migration and rich-payload rendering |

The Claude, Codex, Cursor, and legacy fixtures are not derived from developer
transcripts. Their conversations, timestamps, branches, paths, message IDs, tool
payloads, identities, and code are all synthetic. The Claude fixture deliberately
contains `computeWidgetTotal`, which verifies that Standard redaction preserves a
benign code identifier while Maximum redaction anonymizes it.

## Claude invariants

The fixture must retain:

- one root transcript and one subagent transcript;
- a shared root session UUID and the `agent-*` subagent filename layout;
- the synthetic project path `/home/user/projects/peasant-e2e` and matching slug;
- assistant model fields so publication is not held by the missing-model gate;
- `computeWidgetTotal` in a fenced Go block;
- the plain rendering snippet pinned by `CurrentRenderSnippet`;
- no real names, addresses, repositories, branches, tracker IDs, workflow prompts,
  credentials, or personal filesystem paths.

Shape pins and the association round-trip scenario live in
`claude-fixture/fixture-index.yaml`. The all-`a` commit hash there is an intentional
synthetic value used to test commit association; it is not a captured commit.

## Safe maintenance

Edit fixtures only with explicitly synthetic, hand-authored source data. Never sample
provider data directories. Preserve the indexed provider shape and keep every value
independent of developer environments and user transcripts.

When fixture structure changes, update `fixture-index.yaml` and the typed constants
in `internal/e2e/fixture.go` together. Preserve deliberate synthetic redaction probes;
do not replace them with credential-like values copied from a real environment.

Run:

```bash
go test -race ./internal/e2e -run 'TestFixture|TestNoSecretsGate'
make e2e
```

The focused tests validate fixture indexing, slug decoding, secret detection, and
the redaction differential. `make e2e` validates the mounted full-stack contract.
