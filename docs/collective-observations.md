# Collective metadata deadlines

Saved `village collectives list` and `village contributions list` read local,
account-scoped evidence without contacting Village. Explicit metadata fetches
run synchronously and install only independently complete datasets.

`PEASANT_COLLECTIVE_FETCH_TIMEOUT` configures the metadata operation deadline for
the local web server and the `village collectives` / `village contributions`
commands. Use a Go duration such as `2s` or `90s`. The default is `2m`; supported
values range from `100ms` through `10m`, inclusive. An explicitly empty, malformed
or out-of-range value refuses server startup or command setup with corrective
guidance. Unset the variable to use the default. Restart the local web server
after changing it; the server captures the setting at startup.

For example, a caller that needs a short interactive metadata budget can start
the normal foreground server with:

```sh
PEASANT_COLLECTIVE_FETCH_TIMEOUT=2s peasant web start --foreground --no-browser
```

This deadline does not change content upload/download deadlines, publication
consent, remote authorization, or the HTTP response contract. A deadline returns
the actual incomplete fetch report; a catalog committed before a later history
timeout remains installed. It does not claim that the invocation rolled back or
that retained older history was refreshed. A remote server's HTTP 504 is a remote
failure, not proof that this local deadline expired.
