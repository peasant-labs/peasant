## peasant user show

Show the local owner, claim log, and ownership counts

### Synopsis

Show this data directory's permanent local owner, its ordered claim log, and
how many local sessions and annotations carry an owner.

This reads local state only. It needs no peasant online login and makes no
network request, so a signed-out report is still a successful report.

```
peasant user show [flags]
```

### Options

```
  -h, --help   help for show
      --json   Output as JSON instead of human-readable
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

