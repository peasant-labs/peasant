---
name: c4-model
description: Describe and draw software architecture with the C4 model, in plain ASCII instead of Mermaid or PlantUML. Carries every C4 term with its sourced definition, the C4 principles, an ASCII notation with one template per diagram type, and a lint that checks that notation. Use when the user asks for a "C4 diagram", "system context diagram", "container diagram", "component diagram", "system landscape", "deployment diagram", "dynamic diagram", or "architecture diagram", asks to "describe the architecture" of a system, repo, or workspace, or asks what a C4 term such as "container" or "component" means.
---

# Describe architecture with the C4 model, in ASCII

The C4 model (Simon Brown, c4model.com) describes a software system as a set of zoom levels:
System Context, Containers, Components, and Code. Each level is a map of one element from the
level above. This skill fixes three things: the vocabulary, the principles, and one ASCII notation that a
script can check. With them, every diagram in the workspace reads the same way.

Draw in ASCII, not Mermaid or PlantUML. ASCII renders everywhere the workspace writes: GitHub
Markdown, issue bodies, Beads records, commit messages, terminals, and agent transcripts. It
never depends on a renderer version.

## When to use it, and when not

Use C4 for the static structure of a software system: who uses it, what it talks to, what runs
inside it, and how the code inside one runnable unit is organised. Use it for a whole polyrepo
(system landscape), one repo (system context and container), or one binary or app (component).

Do not use C4 for business processes, workflows, state machines, domain models, data models, or
UI layout. The C4 FAQ excludes these on purpose. Use a sequence list, a state table, an ER
diagram, or a wireframe instead. Do not use C4 to document a library, framework, or SDK as a
system in its own right. Show a library as a technology label on the container that imports
it, or as a component inside that container.

## Workflow

1. **Pick the diagram type by audience and scope.** Most work needs only levels 1 and 2.

   | Diagram | Scope | Shows | Audience |
   |---|---|---|---|
   | System Landscape | an organisation or workspace | people and software systems | everyone |
   | System Context | one software system | the system, its users, its neighbours | everyone |
   | Container | one software system | the runnable units inside it | technical people |
   | Component | one container | the components inside it | developers |
   | Code | one component | classes, functions, packages | developers, generated only |
   | Dynamic | one feature or story | numbered runtime interactions | everyone |
   | Deployment | one environment | where each container instance runs | technical and ops |

2. **Build the model before the picture.** Write two tables first. Elements: name, type,
   technology, one-sentence description. Relationships: source, target, intent, technology. Ground
   every row in the code, the build files, or the deploy config. Do not draw an element that has
   no row, and do not leave a row out of the picture. The tables are the model; the diagram is a
   view of it. Keep the tables in the same file as the diagram when the diagram is long-lived.

3. **Draw with the notation below.** Load `references/ascii-notation.md` for the full rules and
   one template per diagram type. Load `examples/peasant-labs.md` to see the workspace drawn at
   every level.

4. **Lint the file.** Run `python3 scripts/c4-lint.py <file.md>` from this skill directory. It
   finds every fenced block whose info string is `c4` and checks the notation. Fix every
   finding. Run `python3 scripts/c4-lint.py --self-test` once after any change to the script;
   the fixture in `scripts/testdata/lint-cases.md` must stay green.

5. **Walk the review checklist** in `references/principles.md` before the diagram ships. The lint
   checks form. The checklist checks meaning: does a reader outside the team understand every
   box, every arrow, and every technology choice without the author present?

6. **Place it.** A diagram that describes a repo goes in that repo's `AGENTS.md` or `docs/`. A
   diagram that describes the workspace goes in the workspace guide. A diagram that explains one
   change goes in the issue or PR body. Keep the model tables next to long-lived diagrams so that
   the next edit updates both.

## Notation, quick reference

A diagram is a fenced code block with the info string `c4`. Line 1 is the title. The last section
is the key. The full rules, the closed set of nine type tags, the tags each diagram type may
carry, and one template per diagram type are in `references/ascii-notation.md`.

```
Container diagram: <scope>

+--------------------------+     +--------------------------+
| <name>                   |     | <name>                   |
| [Container: <technology>]|-- <intent> (<technology>) -->| [Container: <technology>]|
| <description.>           |     | <description.>           |
+--------------------------+     +--------------------------+
             |
             | <intent> (<technology>)
             v
+== <name> [Software System] ===================+
|  ...elements inside the boundary...            |
+================================================+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  Arrow = one relationship, read as "source, label (technology), target".
```

- **Element box.** Solid border. Line 1 the name. Line 2 the type tag. Then the description.
- **Type tag.** Square brackets, closed set, technology after the colon for containers and
  components. Use square brackets nowhere else in the body. The key may write `[Type]` to
  explain the tag. Relationship technology goes in parentheses.
- **Boundary.** Double-line border with the name and the tag in the top edge. It holds the next
  level down.
- **Relationship.** One arrow, one direction, one label with the intent and the technology. Do
  not draw two-headed arrows. Number the labels on a dynamic diagram.
- **Width.** Keep to about 100 columns. This is a layout guideline, not a lint rule.

## Hard rules

1. One abstraction level per diagram. A container diagram shows containers, plus the people and
   systems that touch them. It never shows a component. The lint enforces the allowed set per
   diagram type.
2. Every element has a name, a type tag, and a one-sentence description. Every container and
   component names its technology in the tag. Two kinds of element may omit the description:
   a supporting element repeated from a parent diagram, and a container instance on a
   deployment diagram.
3. Every relationship is one arrow, one direction, one intent label. On container, component,
   and deployment diagrams the label also names the technology or protocol.
4. Every diagram has a title of the form `<Type> diagram: <scope>` and a `Key:` section.
5. A container is a runtime unit: an application or a data store that must be running for the
   system to work. It is not a Docker container. Where a container runs belongs on a deployment
   diagram, never on the container diagram.
6. Libraries, frameworks, SDKs, and shared modules are not containers and not software systems.
   In this workspace `schema`, `redact`, and `fairtrade` are libraries. Show them as technology
   labels or as components, never as boxes on a context or container diagram.
7. Do not draw a code diagram by hand. Generate it from the source, or leave it out.
8. Match the diagram to the code. When the two disagree, the diagram is wrong. Re-derive the
   element and relationship tables from the source before you redraw.
9. Every C4 diagram passes `scripts/c4-lint.py` before it ships. A lint finding is a defect in
   the diagram, not a reason to relax the lint.
10. Cite the model, not memory. When a definition matters, quote `references/glossary.md`, which
    cites c4model.com page by page.

## Additional resources

### Reference files

- **`references/glossary.md`** Every C4 term with its definition and its c4model.com source:
  the five abstractions, the seven diagram types, the deployment vocabulary, the notation
  vocabulary, and the modelling vocabulary.
- **`references/principles.md`** The C4 principles with their sources, the verbatim notation
  rules, the diagram review checklist, and the history of the model.
- **`references/ascii-notation.md`** The full ASCII notation: box, boundary, arrow, key, layout
  rules, and one template per diagram type.

### Examples

- **`examples/peasant-labs.md`** This workspace drawn as a system landscape, a system context,
  a container diagram, a component diagram, a dynamic diagram, and a deployment diagram, each
  with its model tables. Every block passes the lint.

### Scripts

- **`scripts/c4-lint.py`** Lints every `c4` fenced block in the given Markdown files. Exit 0 when
  clean, 1 on findings. `--self-test` runs the fixture cases in `scripts/testdata/lint-cases.md`.
  It fails unless every lint code has a case that goes red, every clean case stays green, and
  every name in the script's `REQUIRED_CASES` manifest is present in the fixture.
