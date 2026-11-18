## peasant user

Inspect the local owner and claim local sessions

### Synopsis

Inspect this data directory's permanent local owner and its append-only
claim log.

A claim assigns local sessions and annotations to the signed-in peasant online
account. Claims append a new entry; they never rewrite earlier entries. Pulled
transcripts and pulled annotations are foreign and are never claimed.

```
peasant user [flags]
```

### Options

```
  -h, --help   help for user
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
* [peasant user claim](peasant_user_claim.md)	 - Claim local sessions and annotations for the signed-in account
* [peasant user show](peasant_user_show.md)	 - Show the local owner, claim log, and ownership counts

