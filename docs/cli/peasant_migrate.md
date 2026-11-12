## peasant migrate

Upgrade the store schema, then convert file-backed sessions to the harmonized content model

### Synopsis

Upgrade the local store to the schema this build needs, then convert
file-backed native sessions to the harmonized content model in place.

The schema phase runs first. It is the one explicit store upgrade: no
other command changes the schema of a store that holds data. It prints
each step, asks before a long step unless --confirm is given, holds the
data directory lock, and reports each step's duration. Ctrl-C rolls the
running step back and keeps the finished ones. --schema-only stops
after this phase. With --dry-run it prints the upgrade plan for a store
that is behind and changes nothing.

Phase 0 preflights read-only (--dry-run stops here): counts per phase,
pending intents and superseded generations to discard, sampled
field/blob mismatches, free disk, and an advisory no-other-writer note.
Phase 1 drains pending intents, staged directories, and superseded
generations. Phase 2 converts each file-backed native session under its
exclusive lock, shadow-verifying the new rows against the legacy readers
before any old row is deleted. Phase 3 consolidates the search index in
one transaction. Phase 4 sweeps flagged sessions and reports the
retirement preconditions. Phase 5 (documented, offline) runs optimize
and VACUUM.

Run with --dry-run first: it reports the per-phase counts and changes
nothing.

```
peasant migrate [flags]
```

### Options

```
      --confirm       Skip interactive confirmation (for scripts/CI)
      --dry-run       Report the schema upgrade plan and per-phase counts without changing anything
  -h, --help          help for migrate
      --json          Output results as JSON
      --limit int     Convert at most this many sessions with work (0 = all); re-run to continue where this pass stopped
      --schema-only   Upgrade the store schema and stop before converting any session
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

* [peasant](peasant.md)	 - Developer observability for AI coding agent sessions

