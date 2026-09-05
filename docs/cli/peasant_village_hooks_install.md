## peasant village hooks install

Install a village upload hook for the named git events

### Synopsis

Manage the opt-in Git hooks that upload a repository's recorded sessions to the village.

A managed hook runs 'peasant village push --non-interactive --quiet --timeout 5s --repository <resolved-repository>'
for the repository git is acting on, and nothing else. Explicit path overrides
supplied during installation are included in that same displayed and executed
command. --quiet keeps an ordinary commit or push from filling with push output;
errors and a final result line are still printed. --non-interactive answers the
public-visibility confirmation on your behalf. Hooks honor the configured
push.visibility. Peasant publishes content and, when needed, follows it with an
owner visibility update to converge the configured private or public state. If
visibility convergence or local receipt persistence fails, no terminal local
receipt is recorded, so the next repository-scoped run retries that session.
--timeout caps the WHOLE upload, so a village that accepts a connection and then
stops answering cannot hold git up: the per-request client timeout does not bound
a push, which issues several requests in sequence. Giving up is a warning, not a
failure, and the next commit or push tries again; to finish a large first upload,
run 'peasant village push --repository <repo>' once by hand without the budget.
The upload is synchronous: it needs network access and a valid village login, and
it adds its own latency to the git command.

The scope is a project identity, not a path. When the repository has an origin
remote Peasant can normalize, the identity is derived from it, so every clone of
that remote resolves to the same scope. Otherwise — no origin remote, or an origin
that is not a network remote, such as a local path or a file:// URL — the identity
is the worktree path the sessions were recorded in, which belongs to that directory
alone and is shared with no other clone. 'peasant village hooks status' and
'peasant village push --repository' both report which of the two was actually used.
A repository nested inside another one keeps its own identity and never inherits
the outer repository's remote, or its sessions. Earlier versions did let it
inherit, so work recorded inside a submodule, a clone inside a clone, or a plain
'git init' in a subdirectory was filed under the OUTER repository. Those sessions
keep that older identity, so such a directory can show up as two projects: the
historical sessions under the outer identity and new ones under its own. Nothing
is lost and no migration runs. 'peasant village push --repository <dir>' names any
sessions it finds recorded there that its scope cannot reach, and the one command
that re-derives them without touching any other project. Annotations that cannot
be attributed to a session or to that project are not published by a
repository-scoped push.

Two events are supported, and they are chosen independently:

  post-commit  git runs it after a commit has been created.
  pre-push     git runs it before a push starts. Git has not contacted the remote
               at that point, so this hook can never tell you whether the push
               itself succeeded.

A managed hook always exits successfully, so a failed upload prints an actionable
warning and lets the commit or push carry on.

Peasant automatically installs only into an absent hook slot, and only rewrites
or removes a hook file it wrote itself. An existing file, including a zero-byte
file or a symlink, occupies the slot. Install refuses it and changes nothing.

Whether it also prints a section you can add by hand depends on what git actually
runs there, which for a symlink is the file the link points at, not the link. The
section is POSIX shell, so it is offered ONLY for a slot git runs with a shell and
whose file this repository owns. Otherwise it is withheld with the reason. A
readable script gets an invocation suited to the detected interpreter: a shell
command for a shell target outside the hooks directory, or an unquoted argument
list a non-shell interpreter can start directly. A binary or unreadable target is
offered no invocation it cannot safely execute. Pasting POSIX shell into a hook
git runs with python does not fail to help: it makes that file a syntax error, so
a pre-push policy gate dies and no push succeeds again.

The path reported for each event is the one git would really run: it is obtained
by asking git. Automatic management is refused when that effective path is outside
the selected repository, because such a path is shared by every worktree and
repository configured to use it; a repository-local custom path is supported.
Peasant never sets, unsets, or changes core.hooksPath at any scope.

At least one --event is required: a hook is never installed from a defaulted or
implied choice. Each event is handled independently, so a refusal on one still
leaves an accurate, acted-upon result for the other.

```
peasant village hooks install --event post-commit [--event pre-push] [--dir <repo>] [flags]
```

### Options

```
      --dir string          Repository to act on (default: the working directory) (default ".")
      --event stringArray   Git event to manage; repeat for more than one (post-commit, pre-push)
  -h, --help                help for install
      --timeout duration    Overall time budget the installed hook gives the whole upload before giving up with a warning (default 5s). Raise it if your village is far away or your first push is large; the hook is best effort either way, and the next commit or push retries. Choose it here rather than editing the generated file: an edited hook is no longer one Peasant can rewrite or remove.
```

### Options inherited from parent commands

```
      --config string       Path to config file (default "~/.config/peasant/config.yaml")
      --config-dir string   Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant
      --data-dir string     Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant
      --state-dir string    Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant
```

### SEE ALSO

* [peasant village hooks](peasant_village_hooks.md)	 - Install, inspect, and remove the repository's village upload hooks

###### Auto generated by spf13/cobra on 5-Sep-2026
