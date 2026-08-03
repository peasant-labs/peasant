# Contract Versioning Procedure

The shared transcript contract spans four repositories, but it has one source of
truth: [`github.com/peasant-labs/schema`](https://github.com/peasant-labs/schema).
That repository owns the Go module, generated `@peasant-labs/schema` TypeScript
package, OpenAPI documents, compatibility fixtures, and generation-freshness
gates. Consumer repositories pin published versions; they do not maintain local
wire definitions or regenerate another copy.

## Ownership

| Repository | Contract responsibility |
|---|---|
| **schema** | Defines Go wire types, generated TypeScript contracts, OpenAPI documents, closed sets, validation schemas, and compatibility fixtures. |
| **peasant** | Produces local and push payloads, negotiates the Village push window, and renders the generated contracts in its web app. |
| **village** | Validates publishes, persists and serves compatible payloads, advertises the accepted push window, and consumes generated frontend contracts. |
| **transcript-browser** | Renders `@peasant-labs/schema` payloads and publishes a viewer package for Peasant and Village. |

`@peasant-labs/types` is a deprecated compatibility re-export. Do not add wire
types there or use it as the source for a contract change.

## Version Axes

Keep these independent:

| Version | Purpose | Change trigger |
|---|---|---|
| Schema module and npm package version | Identifies one published contract source tree across Go and TypeScript. | Any published schema-module change. |
| Village API version | Identifies Village HTTP operations and payloads. | A Village API contract change. |
| Local API version | Identifies Peasant local HTTP/WebSocket operations and payloads. | A local API contract change. |
| Push contract version | Identifies the publish envelope and embedded session schema accepted by Village. | A breaking publish-wire change. |
| Metadata schema version | Identifies Peasant's on-disk extracted metadata. | A structural metadata-file change. |
| Database schema version | Identifies a consumer's persisted tables and constraints. | A database migration. |

Village advertises its accepted push interval through
`GET /api/v1/schema/version`. Peasant negotiates that interval before upload.
The upload acceptance floor is separate from Village's stored-content
migrate-on-read floor.

## Required Landing Order

1. **Change Schema first.** Update the canonical Go source, generated contract
   inputs, and fixture corpus in the schema repository. Regenerate all owned
   artifacts and run the repository's generation-freshness, compatibility,
   validation, and breaking-change gates.
2. **Publish one Schema release.** Merge the schema change and tag it before any
   consumer re-pin. The release publishes the Go module and the matching
   `@peasant-labs/schema` package from the same tag.
3. **Update transcript-browser when rendering contracts move.** Pin the new
   `@peasant-labs/schema` version, update adapters only where the generated types
   require it, run package and behavior gates, and publish the viewer before app
   consumers pin it.
4. **Update Village.** Pin the tagged Go module in `backend/go.mod` and the
   generated npm package in `frontend/package.json`. Update validation,
   persistence, and migrate-on-read behavior together. Add a new migration for
   persisted schema changes; never edit a shipped migration.
5. **Update Peasant.** Pin the tagged Go module and generated npm package. Pin a
   new transcript-browser release when required. Bump the push or metadata
   version only when that corresponding wire or on-disk shape changes.
6. **Verify the joined production path.** Run each repository's normal gates,
   then run Peasant's schema-parity and end-to-end suites against the exact
   Village revision selected by CI.
7. **Deploy server compatibility before a new producer.** When a push contract
   changes, deploy Village's accepting implementation before releasing a
   Peasant CLI that emits the new version. Negotiated downgrade behavior must
   remain covered while both versions are supported.

## Compatibility and Immutability

- Released OpenAPI files are frozen in the schema repository. A new API shape
  receives a new generated version; retired files stay byte-immutable.
- The schema repository owns the canonical compatibility corpus. Consumers may
  keep a tagged snapshot for integration tests, but they must not redefine its
  expected wire behavior.
- Closed sets such as harnesses, stop reasons, annotation axes, and licenses are
  generated from Schema. Consumer-specific display aliases must be derived from
  those generated types and validated at input boundaries.
- A contract widening that affects persisted constraints requires a new
  consumer migration and corresponding integration coverage.

## Consumer Checklist

- [ ] Schema change merged and tagged before consumer dependency updates.
- [ ] Go consumers pin the intended `github.com/peasant-labs/schema` version.
- [ ] TypeScript consumers pin the matching `@peasant-labs/schema` version.
- [ ] Generated files are fresh and no local wire definition duplicates Schema.
- [ ] Transcript-browser is rebuilt and published when its public types or
      rendering boundary changes.
- [ ] Village serves and enforces the same contract version.
- [ ] Peasant and Village schema-module pins match in cross-repository tests.
- [ ] Backward-compatible reads and negotiated writes are covered by fixtures
      and production-path integration tests.
- [ ] Shipped migrations and retired specifications remain unchanged.
