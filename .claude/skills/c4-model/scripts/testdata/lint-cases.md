# Lint fixture cases

Each case is one `<!-- case: <name> | expect: <codes> -->` line followed by one `c4` block.
`expect: none` means the block must lint clean. The self-test compares the set of codes the lint
reports with the expected set, exactly. The required case names live in `REQUIRED_CASES` in
`c4-lint.py`; every lint code must have at least one case that goes red.

<!-- case: clean-container | expect: none -->
```c4
Container diagram: sample

+---------------------+                                        +---------------------+
| web                 |-- subscribes to session detail (WebSocket) -->| backend      |
| [Container: React]  |                                        | [Container: Go]     |
| Renders sessions.   |                                        | Serves sessions.    |
+---------------------+                                        +---------------------+
                                                                          |
                                                                          | reads rows (SQL)
                                                                          v
                                                               +---------------------+
                                                               | store               |
                                                               | [Container: SQLite] |
                                                               | Session rows.       |
                                                               +---------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Arrow = one relationship.
```

<!-- case: clean-context-with-elbow | expect: none -->
```c4
System Context diagram: sample

+---------------------+                        +-----------------------------+
| developer           |                        | session stores              |
| [Person]            |                        | [Software System, external] |
+---------------------+                        +-----------------------------+
          |                                                   ^
          | views sessions                                    |
          | (browser)                                         |
          v                                                   |
+---------------------+                                       |
| sample              |-- reads session files (JSONL) --------+
| [Software System]   |
+---------------------+

Key:
  Solid box = element. [Type] = C4 abstraction.
```

<!-- case: missing-title | expect: TITLE -->
```c4
sample containers

+---------------------+                        +---------------------+
| web                 |-- subscribes (WS) --->| backend             |
| [Container: React]  |                        | [Container: Go]     |
+---------------------+                        +---------------------+

Key:
  Solid box = element.
```

<!-- case: missing-key | expect: KEY -->
```c4
Container diagram: sample

+---------------------+                        +---------------------+
| web                 |-- subscribes (WS) --->| backend             |
| [Container: React]  |                        | [Container: Go]     |
+---------------------+                        +---------------------+
```

<!-- case: no-elements | expect: NO_ELEMENTS -->
```c4
System Context diagram: sample

+---------------------+                        +---------------------+
| developer           |-- uses (browser) ---->| sample              |
| Records sessions.   |                        | Serves sessions.    |
+---------------------+                        +---------------------+

Key:
  Solid box = element.
```

<!-- case: bad-tag | expect: TAG -->
```c4
Container diagram: sample

+---------------------+                        +---------------------+
| web                 |-- subscribes (WS) --->| backend             |
| [Containr: React]   |                        | [Container]         |
+---------------------+                        +---------------------+
                                                          |
                                                          | reads rows (SQL)
                                                          v
                                               +---------------------+
                                               | store               |
                                               | [Container: SQLite] |
                                               +---------------------+

Key:
  Solid box = element.
```

<!-- case: level-mix | expect: LEVEL_MIX -->
```c4
System Context diagram: sample

+---------------------+                        +---------------------+
| developer           |-- uses (browser) ---->| backend             |
| [Person]            |                        | [Container: Go]     |
+---------------------+                        +---------------------+

Key:
  Solid box = element.
```

<!-- case: unlabelled-arrows | expect: ARROW_LABEL -->
```c4
System Context diagram: sample

+---------------------+                        +-----------------------------+
| developer           |----------------------->| session stores              |
| [Person]            |                        | [Software System, external] |
+---------------------+                        +-----------------------------+
          |
          |
          v
+---------------------+
| sample              |
| [Software System]   |
+---------------------+

Key:
  Solid box = element.
```

<!-- case: arrow-without-technology | expect: ARROW_TECH -->
```c4
Container diagram: sample

+---------------------+                        +---------------------+
| web                 |-- subscribes -------->| backend             |
| [Container: React]  |                        | [Container: Go]     |
+---------------------+                        +---------------------+
                                                          |
                                                          | reads rows
                                                          v
                                               +---------------------+
                                               | store               |
                                               | [Container: SQLite] |
                                               +---------------------+

Key:
  Solid box = element.
```

<!-- case: dynamic-unnumbered | expect: STEP_NUMBER -->
```c4
Dynamic diagram: publish a session

+---------------------+                        +---------------------+
| developer           |-- opens /share (HTTPS) -->| web              |
| [Person]            |                        | [Container: React]  |
+---------------------+                        +---------------------+

Key:
  Numbered arrow = one interaction, in order.
```

<!-- case: two-headed-arrow | expect: BIDIRECTIONAL -->
```c4
Container diagram: sample

+---------------------+                        +---------------------+
| web                 |<-- syncs (WebSocket) -->| backend            |
| [Container: React]  |                        | [Container: Go]     |
+---------------------+                        +---------------------+

Key:
  Solid box = element.
```

<!-- case: clean-label-with-angle-brackets | expect: none -->
```c4
Container diagram: <system>

+--------------------------+                            +--------------------------+
| <container>              |-- <intent> (<protocol>) -->| <container>              |
| [Container: <technology>]|                            | [Container: <technology>]|
+--------------------------+                            +--------------------------+

Key:
  Placeholders in angle brackets must not hide an arrow from the lint.
```

<!-- case: clean-dynamic-vertical-steps | expect: none -->
```c4
Dynamic diagram: publish a session

+---------------------+
| developer           |
| [Person]            |
+---------------------+
      |             |
      | 1. opens    | 3. confirms the
      | /share      | redaction review
      | (browser)   | (browser)
      v             v
+---------------------+
| web                 |
| [Container: React]  |
+---------------------+

Key:
  Numbered arrow = one interaction, in order. A label may span several shaft lines.
```

<!-- case: single-dash-two-headed-arrow | expect: BIDIRECTIONAL -->
```c4
Container diagram: sample

+---------------------+     +---------------------+
| web                 |<->| backend             |
| [Container: React]  |     | [Container: Go]     |
+---------------------+     +---------------------+

Key:
  Solid box = element.
```
