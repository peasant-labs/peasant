# OpenCode native-row decode measurement

Developer-only command. Input is an explicit JSON array of `{ "Name": "optional",
"Row": { ... }, "Scope": { ... } }`, using production
`OpenCodeProvenanceRow` and `OpenCodeProvenanceScope` shapes. Names are ignored.
No database, source discovery, or filesystem access beyond this corpus read.

Build identical driver source against baseline and candidate revisions:

```sh
go build -o /tmp/opencode/opencode-rows-baseline ./scripts/profile-opencode-rows
# Build the candidate separately from its own revision.
/tmp/opencode/opencode-rows-baseline -corpus "$CORPUS" -phase parity > /tmp/opencode/rows-baseline-parity.json
/tmp/opencode/opencode-rows-candidate -corpus "$CORPUS" -phase parity > /tmp/opencode/rows-candidate-parity.json
cmp /tmp/opencode/rows-baseline-parity.json /tmp/opencode/rows-candidate-parity.json
```

Parity reports contain only ordinals, complete returned-message/settled/refusal
digests, and counts. Error text and row/name content are never printed. The exact
corpus bytes also have a digest. Matching refusals are not successful work.

Run timing separately, only after parity, without concurrent builds/benchmarks:

```sh
/tmp/opencode/opencode-rows-baseline -corpus "$SUCCESS_CORPUS" -phase timing -repetitions 1000 > /tmp/opencode/rows-baseline-timing.json
/tmp/opencode/opencode-rows-candidate -corpus "$SUCCESS_CORPUS" -phase timing -repetitions 1000 > /tmp/opencode/rows-candidate-timing.json
```

Timing brackets the whole repeated decode loop, including status counting; loading,
hashing, JSON output, and memory-stat sampling are outside it. Counts cover all
repetitions. An ignored entryless marker (a control row this build interprets and
that returns no transcript message) is counted separately and is not a refusal. A
skipped control row of a type this build does not know is still a refusal: the
row is omitted, so timing stays suppressed. Any refusal suppresses
timing/allocation metrics and exits nonzero. Adding the ignored counter changes
the parity digest only for rows it classifies, so a stored parity reference stays
comparable for every other row.
Filter a corpus by successful parity ordinals if needed, then establish parity
again on that exact filtered file before measuring. Repeated tiny fixtures measure
the warmed row decoder, not acquisition, full generation building, or harvest.
Record revisions, Go version, date, corpus digest, exact commands and repeated
measurements. Keep output in scratch storage; this is not a CI threshold/test.
