## peasant user claim

Claim local sessions and annotations for the signed-in account

### Synopsis

Claim this data directory's local sessions and annotations for the signed-in
peasant online account.

By default only unclaimed artifacts are claimed. --all is an explicit
reassignment of every local session and annotation; it appends a new claim
entry and never rewrites earlier entries. Pulled transcripts and pulled
annotations are foreign and are never claimed.

```
peasant user claim [flags]
```

### Options

```
      --all    Reassign every local session and annotation, not only unclaimed ones
  -h, --help   help for claim
      --json   Output as JSON instead of human-readable
  -y, --yes    Confirm --all without asking on a terminal
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

* [peasant user](peasant_user.md)	 - Inspect the local owner and claim local sessions

