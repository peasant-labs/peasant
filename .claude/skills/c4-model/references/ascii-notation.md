# ASCII notation for C4 diagrams

One notation for every C4 diagram in the workspace. It satisfies the c4model.com notation rules.
`scripts/c4-lint.py` checks it. Each rule has one of two reasons. It lets a reader outside the
team decode the diagram, or it lets the lint decide something a reviewer would otherwise check
by eye.

## The block

A diagram is a fenced code block whose info string is exactly `c4`:

    ```c4
    Container diagram: peasant
    ...
    Key:
      ...
    ```

Use `c4`, not `text`, so that the lint can find the block, and so that a block with no title is
still found and flagged. GitHub renders an unknown info string as plain text, which is what the
diagram needs.

## Title

Line 1, always: `<Type> diagram: <scope>`. `<Type>` is one of `System Landscape`,
`System Context`, `Container`, `Component`, `Code`, `Dynamic`, `Deployment`. `<scope>` names the
thing in scope: the workspace, the software system, the container, the feature, or the system
plus environment. Examples: `System Context diagram: peasant`, `Component diagram: peasant
backend`, `Deployment diagram: village, production`.

## Element box

A solid box. Line 1 the name. Line 2 the type tag. Following lines the description: one sentence
in ASD-STE100, wrapped as needed, ending with a full stop. Widths align within a column of
boxes. Two kinds of element may omit the description: a supporting element repeated from a
parent diagram, and a container instance on a deployment diagram.

```
+---------------------------+
| peasant backend           |
| [Container: Go]           |
| Ingests, indexes, and     |
| serves agent sessions.    |
+---------------------------+
```

## Type tags

Reserve square brackets for type tags. The set is closed:

| Tag | Meaning |
|---|---|
| `[Person]` | a human user, named by role |
| `[Software System]` | a software system in scope |
| `[Software System, external]` | a software system outside the scope of this diagram |
| `[Container: <technology>]` | a runnable unit or data store, with its technology |
| `[Component: <technology>]` | a component inside a container, with its technology |
| `[Code: <kind>]` | a code element, with its kind such as struct, interface, function |
| `[Deployment Node: <technology>]` | where something runs, with its technology |
| `[Infrastructure Node: <technology>]` | a load balancer, DNS, firewall, or similar |
| `[Group: <name>]` | an optional grouping, such as a team or a department |

Technology text after the colon is free text, short, and specific: `Go`, `Next.js`, `SQLite`,
`Postgres 16`, `Railway service`. Do not put square brackets in descriptions or labels.
The key may write `[Type]` to explain the tag. Relationship technology goes in parentheses.

Each diagram type may carry only the tags of its level plus its supporting elements. The lint
mirrors this table in its `ALLOWED` set.

| Diagram | Allowed tags |
|---|---|
| System Landscape | Person, Software System, Group |
| System Context | Person, Software System, Group |
| Container | Person, Software System, Container, Group |
| Component | Person, Software System, Container, Component, Group |
| Code | Component, Code |
| Dynamic | Person, Software System, Container, Component, Group |
| Deployment | Deployment Node, Infrastructure Node, Software System, Container, Group |

## Boundary

A double-line box. The name and the type tag sit in the top border. Boundaries hold the elements
of the next level down: a software system boundary holds containers, a container boundary holds
components, a deployment node holds container instances or nested deployment nodes. Indent the
contents by two spaces and leave one blank row inside the top and bottom borders.

```
+== peasant [Software System] ==============================+
|                                                           |
|  +---------------------------+                            |
|  | peasant backend           |                            |
|  | [Container: Go]           |                            |
|  +---------------------------+                            |
|                                                           |
+===========================================================+
```

When an arrow crosses a boundary border, replace the `=` at the crossing column with `|`.

## Relationships

One arrow per relationship. One direction per arrow. Do not draw two-headed arrows (`<-->`,
`<->`). Give two relationships two arrows. The arrow points from the element that makes the
call or holds the dependency to the element that receives it, unless the key says the arrows
show data flow.

Horizontal, label on one line, `--` on each side:

```
|-- subscribes to session detail (WebSocket) -->|
|<-- streams turns (WebSocket) --|
```

Vertical, a shaft of `|` with the label beside it and an arrowhead `v` (down) or `^` (up). The
label starts one to three spaces after the shaft. It may span several shaft lines; the lint
joins them.

```
          |
          | reads session rows
          | (SQL, sqlite3)
          v
```

Draw an elbow when a straight line does not fit. Put the label on the horizontal run of the
elbow:

```
+-- reads session files (JSONL) --+
                                  |
                                  v
```

Labels: an intent in ASD-STE100, present tense, active voice, from the source's point of view.
Then the technology or protocol in parentheses. Give the technology on container, component, and
deployment diagrams. Add it elsewhere when it helps. Dynamic diagram labels begin
with a step number: `-- 1. opens /share (HTTPS) -->`.

## Key

The last section. The line `Key:` followed by indented text. The key explains the notation, not
the elements. This standard key fits every diagram and may be shortened to what the diagram uses:

```
Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

Add a line whenever the diagram uses something beyond that: numbered steps, a data-flow
convention, an abbreviation.

## Layout rules

- Keep to about 100 columns so the diagram reads in a terminal and on GitHub without a
  horizontal scroll. Split the diagram instead of shrinking it. This is a guideline, not a
  lint rule.
- People at the top or the left. The element in scope at the centre. External systems at the
  right or the bottom. Data stores below the things that write to them.
- Flow left to right and top to bottom. Arrows should not cross. Move a box before you cross an
  arrow.
- At most about nine elements per diagram. Above that, split by scope or promote a level.
- Align box widths within a column. Align box tops within a row.
- Keep at least one blank column between a box and an arrow label.
- Same element, same name, same technology on every diagram it appears on.
- ASD-STE100 in every label and description. No em-dashes anywhere.

## Templates

One template per diagram type. Each passes the lint as written. Replace the `<placeholders>`.

### System Landscape

```c4
System Landscape diagram: <organisation or workspace>

+---------------------+                        +---------------------+
| <role>              |                        | <role>              |
| [Person]            |                        | [Person]            |
| <What they do.>     |                        | <What they do.>     |
+---------------------+                        +---------------------+
          |                                               |
          | <intent>                                      | <intent>
          v                                               v
+---------------------+                        +---------------------+
| <system>            |-- <intent> (<tech>) -->| <system>            |
| [Software System]   |                        | [Software System]   |
| <What it does.>     |                        | <What it does.>     |
+---------------------+                        +---------------------+
          |
          | <intent> (<tech>)
          v
+-----------------------------+
| <system>                    |
| [Software System, external] |
| <What it does.>             |
+-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. "external" = outside the scope of this diagram.
  Arrow = one relationship, read as "source, label (technology), target".
```

### System Context

```c4
System Context diagram: <system>

+---------------------+
| <role>              |
| [Person]            |
| <What they do.>     |
+---------------------+
          |
          | <intent> (<tech>)
          v
+---------------------+                        +-----------------------------+
| <system in scope>   |-- <intent> (<tech>) -->| <neighbour>                 |
| [Software System]   |                        | [Software System, external] |
| <What it does.>     |                        | <What it does.>             |
+---------------------+                        +-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. "external" = outside the scope of this diagram.
  Arrow = one relationship, read as "source, label (technology), target".
```

### Container

```c4
Container diagram: <system>

+---------------------+
| <role>              |
| [Person]            |
+---------------------+
          |
          | <intent> (<tech>)
          v
+== <system> [Software System] ======================================================+
|                                                                                    |
|  +--------------------------+                            +--------------------------+
|  | <container>              |-- <intent> (<protocol>) -->| <container>              |
|  | [Container: <technology>]|                            | [Container: <technology>]|
|  | <What it does.>          |                            | <What it does.>          |
|  +--------------------------+                            +--------------------------+
|                                                                       |            |
|                                                                       | <intent>   |
|                                                                       | (<tech>)   |
|                                                                       v            |
|                                                          +--------------------------+
|                                                          | <data store>             |
|                                                          | [Container: <technology>]|
|                                                          | <What it holds.>         |
|                                                          +--------------------------+
|                                                                                    |
+====================================================================================+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  Arrow = one relationship, read as "source, label (technology), target".
```

### Component

```c4
Component diagram: <container>

+--------------------------+
| <other container>        |
| [Container: <technology>]|
+--------------------------+
          |
          | <intent> (<protocol>)
          v
+== <container> [Container: <technology>] ==========================================+
|                                                                                   |
|  +--------------------------+                           +--------------------------+
|  | <component>              |-- <intent> (<call>) ----->| <component>              |
|  | [Component: <technology>]|                           | [Component: <technology>]|
|  | <What it does.>          |                           | <What it does.>          |
|  +--------------------------+                           +--------------------------+
|                                                                                   |
+===================================================================================+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  Arrow = one relationship, read as "source, label (technology), target".
```

### Code

Generate this one. Do not draw it by hand. The template shows the shape a generator should emit.

```c4
Code diagram: <component>

+== <component> [Component: <technology>] ==========================================+
|                                                                                   |
|  +--------------------------+                           +--------------------------+
|  | <TypeName>               |-- <uses> ---------------->| <TypeName>               |
|  | [Code: struct]           |                           | [Code: interface]        |
|  +--------------------------+                           +--------------------------+
|                                                                                   |
+===================================================================================+

Key:
  Generated from source on <date> by <tool>. Do not edit by hand.
  Solid box = code element. Arrow = one dependency, read as "source, label, target".
```

### Dynamic

```c4
Dynamic diagram: <feature or story>

+---------------------+                             +--------------------------+
| <role>              |-- 1. <intent> (<tech>) ---->| <container>              |
| [Person]            |                             | [Container: <technology>]|
+---------------------+                             +--------------------------+
                                                               |
                                                               | 2. <intent> (<tech>)
                                                               v
                                                    +--------------------------+
                                                    | <container>              |
                                                    | [Container: <technology>]|
                                                    +--------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

### Deployment

```c4
Deployment diagram: <system>, <environment>

+== <platform> [Deployment Node: <technology>] =====================================+
|                                                                                   |
|  +== <service or host> [Deployment Node: <technology>] =========================+ |
|  |                                                                              | |
|  |  +--------------------------+                                                | |
|  |  | <container>              |                                                | |
|  |  | [Container: <technology>]|                                                | |
|  |  | <What it does.>          |                                                | |
|  |  +--------------------------+                                                | |
|  |                                                                              | |
|  +==============================================================================+ |
|                                                                                   |
|  +--------------------------------------+                                         |
|  | <load balancer or DNS>               |                                         |
|  | [Infrastructure Node: <technology>]  |                                         |
|  +--------------------------------------+                                         |
|                                                                                   |
+===================================================================================+

Key:
  Double-line box = deployment node, nested where one runs inside another. Solid box =
  a container instance or an infrastructure node. [Type] = C4 abstraction.
```
