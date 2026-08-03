# Network Boundary — Developer Reference

This document maps every outbound network call to its source code location, wire type, trigger mechanism, and redaction pipeline. For the user-facing version, see [NETWORK.md](NETWORK.md).

## Outbound Call Inventory

### Call 1: Transcript Publish

| Attribute | Value |
|-----------|-------|
| **Method** | `POST multipart/form-data` |
| **Endpoint** | `{baseURL}/api/v1/transcripts/publish` |
| **Client** | `internal/push/client.go:73-133` → `VillageClient.Publish()` |
| **Wire type sent** | Part 1: `schema.PublishRequest` (JSON). Part 2: transcript bytes (binary) |
| **Wire type received** | `ingest.PublishResult` |
| **Caller chain** | `cmd/peasant/cmd_push.go:155` → `push.Pipeline.Run()` → `pushSession()` (`pipeline.go:306-439`) |
| **Trigger** | `peasant push` CLI command |
| **Consent gate** | `runPushWizard()` (`cmd_push.go`) — interactive TUI, user selects sessions. **Skipped entirely on the hook path:** a generated Git hook runs `village push --non-interactive --quiet`, so installing the hook is the consent step instead (`internal/githooks`). Any re-derivation of the user-facing document has to carry that. |
| **Auth** | Bearer token in `Authorization` header (`client.go:110`) |

**Redaction pipeline (applied in `pushSession`):**

Read this before changing it: a publish is **multipart**, and the transcript's
text leaves in **both** parts. The metadata part carries it inside
`PublishRequest.Entries` (`contentPreview`, `toolInput`, `toolOutput`); the
transcript part carries it in the content envelope. Redaction that covers one
part covers half the wire — that was the state of this path until the entries
were redacted at the point they are read, and it is the claim to get right if
you are re-deriving [NETWORK.md](NETWORK.md) from this file.

1. Metadata read from disk (`pipeline.go`, `pushSession`)
2. Safety-net redactor re-redacts **metadata** — catches sessions ingested before
   redaction existed, or at a weaker level:
   ```go
   if p.redactor != nil {
       redacted := p.redactor.RedactMetadata(&meta)
       meta = *redacted
   }
   ```
3. Entries read from the store (`p.store.ListEntries`) and **redacted immediately**
   (`push.RedactEntries`, `pipeline.go` step 3b) through
   `redact.RedactJSONDocBytes` — the same function `peasant redact` uses. It runs
   over the *marshalled* entries, so every string value is redacted and a text
   field added to the wire entry later is covered without changing this step.
   Redaction happens at the point the entries are READ rather than at each point
   they are used, so the unredacted entries do not exist further down the
   function and no later consumer can attach them. It **fails closed**: a
   redaction that cannot complete ends that session instead of publishing what it
   failed to redact.
4. Metadata mapped to `PublishRequest` via `MapMetadata()` (`push/mapper.go`),
   with the already-redacted entries attached as `req.Entries`
   - Field visibility controls applied here (config-driven field omission)
5. Transcript part built by `marshalTranscriptContent` (`push/content.go`), which
   redacts the whole serialized document again. Re-redaction is a no-op — the
   canonical placeholders match no rule — so both parts derive from one redaction
   of the entries.
6. Redactor level is resolved by `config.EffectiveRedactionLevel`, with a floor of
   Standard, so redaction never runs below Standard. A configured level the
   version cannot apply does not reach here at all — the command refuses first
   (`config.RedactionLevelSupported`).

**Identifiers survive** redaction because no rule matches their shapes, not
because they are excluded: every string in both parts goes through the redactor.
`TestPushContent_RedactsContentWithoutBreakingIdentifiers` pins that as a
property over the whole document.

**Not covered here:** the village scans only the **transcript** part for secrets
(`ScanForSecrets`, handed `r.FormFile("transcript_file")`); the metadata part gets
schema validation and nothing else. Client-side redaction is therefore the only
thing standing between the entries and the wire — do not weaken step 3 on the
assumption that a server-side scan is behind it.

**Data flow diagram:**
```
metadata.json (disk) → json.Unmarshal → RedactMetadata() ─┐
                                                          ├→ MapMetadata() → multipart "metadata" field
session_entries (DB) → ListEntries() → RedactEntries() ───┤
                                            │             └→ (attached as req.Entries)
                                            └→ marshalTranscriptContent() → multipart transcript part
                                               (redacts the serialized document again; no-op)
```

### Call 2: Annotation Push

| Attribute | Value |
|-----------|-------|
| **Method** | `POST application/json` |
| **Endpoint** | `{baseURL}/api/v1/annotations` |
| **Client** | `internal/push/client.go:138-175` → `VillageClient.UploadAnnotations()` |
| **Wire type sent** | `schema.AnnotationPushRequest` containing `[]schema.AnnotationPushItem` |
| **Wire type received** | `schema.AnnotationPushResponse` |
| **Caller chain** | `cmd/peasant/cmd_push.go:159` → `push.PushAnnotations()` (`push/annotations.go:79-94`) |
| **Trigger** | `peasant push` CLI command (same as transcripts) |
| **Consent gate** | Same push wizard |
| **Auth** | Bearer token (`client.go:153`) |
| **Batching** | 500 annotations per HTTP request (`annotations.go:34`) |

**No redaction applied to annotations.** Annotation values are computed locally (classifier outputs, quality scores). The `reason` field may contain free-text that could theoretically include sensitive content.

### Call 3: Schema Version Query

| Attribute | Value |
|-----------|-------|
| **Method** | `GET` |
| **Endpoint** | `{baseURL}/api/v1/schema/version` |
| **Client** | `internal/push/client.go:179-206` → `VillageClient.GetSchemaVersion()` |
| **Wire type received** | `schema.SchemaVersionResponse` |
| **Status** | **Defined but currently unused** — no caller in `cmd_push.go` |
| **Auth** | Bearer token (`client.go:185`) |

### Call 4: OAuth Login

| Attribute | Value |
|-----------|-------|
| **Method** | Browser redirect (GET) + code exchange (POST) |
| **Endpoints** | `{village}/api/v1/auth/cli/login?port={port}&state={state}` (browser) |
| | `{village}/api/v1/auth/cli/exchange` (POST) |
| **Client** | `internal/auth/login.go:22-74` → `Login()` |
| **Exchange** | `internal/auth/server.go:115-164` → `exchangeCode()` |
| **Wire type sent** | `{"code": "...", "state": "..."}` (exchange body, `server.go:100-103`) |
| **Wire type received** | `APIKey`, `KeyID`, `UserID`, `Username` (`server.go:149-163`) |
| **Local callback** | `127.0.0.1:17249` or ephemeral port (`server.go:383-396`) |
| **Trigger** | `peasant login` CLI command |
| **TLS** | Localhost allows insecure TLS for dev (`server.go:127-133`) |

### Call 5: OAuth Logout (Key Revocation)

| Attribute | Value |
|-----------|-------|
| **Method** | `DELETE` |
| **Endpoint** | `{village}/api/v1/auth/api-keys/{keyId}` |
| **Client** | `internal/auth/logout.go:37-67` → `revokeRemoteKey()` |
| **Trigger** | `peasant logout` CLI command |
| **Auth** | Bearer token with the key being revoked (`logout.go:44`) |

### Call 6: Model Registry Fetch

| Attribute | Value |
|-----------|-------|
| **Method** | `GET` |
| **Endpoint** | `https://models.dev/api.json` (hardcoded, `models_client.go:15`) |
| **Client** | `internal/store/models_client.go:73-91` → `ModelsClient.FetchModels()` |
| **Wire type received** | `modelsDevResponse` (map of providers → models) |
| **Trigger** | Automatic during `peasant ingest` pipeline |
| **Consent** | **None required** — no data sent, public API |
| **Retry** | Up to 2 retries on failure (`models_client.go:75-90`) |
| **Fallback** | `StaticModels()` embedded in binary (`models_client.go:96`) |
| **Body limit** | 10 MB response cap (`models_client.go:127`) |

### Call 7: Local TUI → Local Web Server (NOT external)

| Attribute | Value |
|-----------|-------|
| **Method** | `POST application/json` |
| **Endpoint** | `{serverURL}/api/v1/annotations/batch` |
| **Client** | `internal/tui/session.go:510-551` → `commitPendingAnnotations()` |
| **Destination** | `localhost:8690` (the local `peasant web start` server) |
| **Note** | This is localhost-to-localhost. Not an external network call. |

## Wire Types Reference

All wire types are defined in the `github.com/peasant-labs/schema` module:

| Type | File | Used In |
|------|------|---------|
| `PublishRequest` | `publish.go:13-25` | Call 1 (transcript push) |
| `PublishResponse` | `publish.go:28-35` | Call 1 response |
| `AnnotationPushRequest` | `publish.go:65-67` | Call 2 (annotation push) |
| `AnnotationPushItem` | `publish.go:40-54` | Call 2 payload items |
| `AnnotationPushResponse` | `publish.go:70-76` | Call 2 response |
| `SchemaVersionResponse` | `publish.go:98-104` | Call 3 response |
| `SessionEntry` | `content.go:8-33` | Embedded in `PublishRequest.Entries` |
| `UnifiedMetadata` | `metadata.go:42-60` | Local storage; mapped to `PublishRequest` via `MapMetadata` |
| `QualityMetrics` | (generated) | Embedded in `PublishRequest.Quality` |

## OpenAPI Specs

Generated specs for the wire format live in the `github.com/peasant-labs/schema` module (its `generated/`):

| Spec | Version | File |
|------|---------|------|
| Village API | 0.12.0 | `village-api-0.12.0.json` / `.yaml` |
| Types catalog | 0.9.0 | `types-0.9.0.json` / `.yaml` |
| Local API (peasantlocal) | 0.6.0 | `peasantlocal-api-0.6.0.json` / `.yaml` (localhost only) |

The Schema repository owns generation and freshness checks. Publish a Schema tag
before updating Peasant's module and npm package pins.

## Redaction Pipeline (Push Path)

```
                                    ┌─────────────────────────────┐
                                    │    cmd/peasant/cmd_push.go     │
                                    │                             │
                                    │  1. Build Standard redactor │
                                    │  2. Run push wizard (TUI)   │
                                    │  3. Start pipeline          │
                                    └─────────────┬───────────────┘
                                                  │
                                    ┌─────────────▼───────────────┐
                                    │  push.Pipeline.pushSession  │
                                    │                             │
                                    │  4. Read metadata.json      │
                                    │  5. RedactMetadata()        │──→ context-aware slug redaction
                                    │  6. MapMetadata()           │──→ field visibility omission
                                    │  7. ListEntries() from DB   │
                                    │  8. BuildTranscriptContent()│──→ redacted: RedactJSONDocBytes
                                    │  9. Publish() via HTTP      │
                                    └─────────────────────────────┘
```

**Redaction layers:**

| Layer | What | Where | Level |
|-------|------|-------|-------|
| Ingest-time | **None.** No supported level redacts while ingest writes | `cmd_harvest.go` (`checkIngestRedactionLevel`) | n/a — the only level that did is refused |
| Post-hoc | Explicit `peasant redact` command, rewrites the stored transcript and metadata files | `cmd_redact.go` | User-configured, refused above Standard |
| Push safety-net | Re-redact the metadata STRUCT before upload, over a hand-enumerated field list | `pipeline.go` (`RedactMetadata`) | Configured level, floored at Standard |
| Push outward redaction | Redact all three outward seams as whole documents: the entries where they are read, the assembled metadata request, the transcript content part | `pipeline.go` step 3b, `mapper.go`, `content.go` (`redactJSONDocument`) | Configured level, floored at Standard |
| Field visibility | Omit fields entirely | `mapper.go` (field-visibility control) | Config-driven |

The safety-net covers **metadata**: even if a session was imported with no
redaction, the metadata crossing the network is redacted at Standard or above. It
walks a HAND-ENUMERATED list of fields, so it does not cover a metadata field
nobody added to that list — `Model` is one. What covers those, and covers content
— conversation, tool arguments and tool results — is the whole-document redaction
of each outward seam in the row beneath it. Both rows are load-bearing: the
hand-list runs over the struct before mapping, the document redaction runs over
everything each part actually carries.

Two consequences worth knowing before you rely on any of this:

- `peasant redact` rewrites the stored transcript and metadata **files**, but does
  NOT re-index `session_entries`. Push publishes from those entries, so running it
  alone does not change what a later push sends.
- Maximum is the only level that anonymizes code identifiers, and the parser it
  needs is linked in only when Peasant is built with cgo. Rather than let one
  configuration behave differently depending on how the binary was compiled, this
  version does not support the level on any build: every command refuses to run
  under it, rather than applying a weaker level than the user asked for.

## Privacy-Sensitive Fields Matrix

Fields that can identify the user or their projects:

| Field | In `UnifiedMetadata` | In `PublishRequest` | Redacted? | Field-visibility? | Risk |
|-------|---------------------|--------------------|-----------|--------------------|------|
| `source.filePath` | Yes | Yes | Partial (literal path only, slugs missed before field-visibility controls) | No | HIGH |
| `git.remote` | Yes (ptr) | Yes (ptr) | No | **Yes** (default: omit) | HIGH |
| `git.branch` | Yes (ptr) | Yes (ptr) | No | **Yes** (default: omit) | MEDIUM |
| `git.worktree` | Yes (ptr) | Yes (ptr) | Yes (literal path) | No | MEDIUM |
| `project.hash` | Yes | Yes | No | No | HIGH (unsalted) |
| `project.filePath` | Yes | Yes | Partial (literal path) | **Yes** (default: omit) | HIGH |
| `project.name` | Yes | Yes | Partial (slug form) | No | LOW-MEDIUM |
| `model.hostSlug` | Yes | Yes | No (before field-visibility controls) | **Yes** (default: omit) | HIGH |
| `entries[].contentPreview` | N/A (DB only) | Yes | **Yes — redacted by pattern** | No | HIGH |
| `entries[].toolInput` | N/A (DB only) | Yes | **Yes — redacted by pattern** | No | HIGH |
| `entries[].toolOutput` | N/A (DB only) | Yes | **Yes — redacted by pattern** | No | HIGH |
| `CWD` | Yes (v7+) | **No** (not in PublishRequest) | Yes (RedactText) | N/A | N/A |

## Confirmed Absences

The codebase contains **zero** instances of:
- Telemetry SDKs (Sentry, PostHog, Mixpanel, Amplitude)
- Analytics event tracking
- Crash reporting
- Auto-update / version polling
- Background network daemons
- DNS-based tracking or beacons
- Third-party authentication beyond the village OAuth flow
