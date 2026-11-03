## peasant sessions search

Search indexed session text by relevance

### Synopsis

Search ranked full-text over indexed transcript text, already redacted at
ingest and potentially preview-capped. FTS matches whole tokens, not substrings
or regular expressions. Quote multi-word queries. Session ids and project hashes
resolve directly, ignoring scope filters with a warning.

At most 500 rows are returned per page; a larger --limit is clamped. Unlike sessions list,
--limit 0 is refused
and subagent sessions are not hidden. A date-only --until covers that whole day.
Output is local and receives no publish-time redaction pass. --json preserves the
search payload (including score) alongside warnings and explicit empty reasons.

```
peasant sessions search <query> [flags]
```

### Options

```
      --harness string         Filter by harness ({claude-code, gemini-cli, codex, opencode, cursor, strike, pi})
  -h, --help                   help for search
      --json                   Output one JSON document, with score and CLI metadata
      --limit string           Maximum ranked rows per page (integer >= 1; values above 500 are clamped; 0 is refused) (default "20")
      --offset string          Ranked rows to skip (integer >= 0) (default "0")
      --project string         Filter by project using the sessions list match (git remote URL or directory basename)
      --sessions-from string   Read session ids from a file, or from standard input when the value is -. Accepts one id per line, a JSON array of ids, or the JSON array that a --json listing prints
      --since string           Only sessions starting after this time (positive Go duration (e.g. 30m, 1h30m), integer d, w, mo (months), y, YYYY-MM-DD (UTC), or RFC3339)
      --until string           Only sessions starting before this time; a date includes that whole day (positive Go duration (e.g. 30m, 1h30m), integer d, w, mo (months), y, YYYY-MM-DD (UTC), or RFC3339)
```

### Options inherited from parent commands

```
      --config string       Path to config file (default "~/.config/peasant/config.yaml")
      --config-dir string   Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant
      --data-dir string     Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant
      --state-dir string    Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant
  -v, --version             Print the version
```

### SEE ALSO

* [peasant sessions](peasant_sessions.md)	 - Manage sessions

