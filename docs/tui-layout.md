# TUI layout and background painting

Every terminal surface in Peasant composes its lines through the layout
primitives in `internal/tui/kit`. Do not pad, place, align, or paint a
background at the surface. A grep gate enforces this rule (see
[The layout gate](#the-layout-gate)).

## Why the rule exists

A theme text role such as `Header`, `Muted`, or `Danger` sets a foreground
color only. It sets no background. lipgloss pads a line to a target width only
when the line is more narrow than the target. A block of lines with different
lengths therefore paints a different background box on each line. The block
then shows a ragged, staircase edge instead of one panel.

Each surface that met this problem wrote the same repair again: measure the
widest line, pad all lines to that width, paint the background, and center the
block if necessary. Each new copy of the repair was a new chance to make an
error. The kit now owns the repair one time.

## The primitives

| Primitive | Use it for |
|---|---|
| `kit.Panel` | A block of lines that must share one background and one width. |
| `kit.Panel.Style` | To put a foreground-only theme role on a filled region. |
| `kit.ScrollStrip` | A horizontal strip that must keep its active item visible. |
| `kit.FitLine` / `kit.FitLineTail` | One plain-text line fitted to a width and styled one time. |
| `kit.FitCell` | One unstyled column cell fitted to a width. |
| `kit.Center` / `kit.CenterOnTheme` | To center content and paint the region around it. |
| `kit.Indent` | A plain-text column offset. |
| `kit.Frame` | The bordered container, with title and footer rows. |

### Panel

Build a panel over the theme, add the lines, then render it:

```go
panel := kit.NewPanel(th)
panel.SetSize(width, 0)          // width 0 measures the widest line
panel.Line(styles.Header, "before you connect")
panel.Line(styles.Muted, "- a village is a shared commons.")
return panel.View()
```

Rules the panel keeps:

- Every rendered line is exactly `ContentWidth` cells wide.
- Every cell carries the panel background token, including the pad cells.
- `Panel.Style` paints the panel background on a foreground-only role. It
  returns a role that already names its own background, such as `Selected`,
  without a change. This keeps a deliberate highlight intact.
- `WithAlign(kit.PanelAlignCenter)` centers each line inside the box.
- `WithBackground(th.Palette.Surface)` makes a raised panel.
- `Rendered` adds content that already carries ANSI styling, such as a child
  component view.

### Scrolling strip

`kit.ScrollStrip` renders a row of items into an exact width. It scrolls the
row so that the active item stays fully visible, and it draws an overflow
marker on each clipped side. The guided settings flow uses it for the step tab
strip. Before this primitive, a narrow terminal clipped the strip from the
right and hid the current step.

`kit.StripWindow` computes the visible item range alone. Use it when you must
know which items are visible without rendering them.

## The layout gate

`internal/tui/gates/layout.go` scans every production Go file under
`internal/tui`, plus `internal/push/wizard.go`, for four signatures:

| Signature | Correct path |
|---|---|
| A hand-rolled space run | `kit.FitLine`, `kit.FitCell`, `kit.Panel`, `kit.Indent` |
| `lipgloss.Place` | `kit.Center` or a centered panel |
| A lipgloss alignment | `kit.Panel` with an alignment |
| A background painted on a style | `kit.Panel.Style` |

`internal/tui/kit`, `internal/tui/theme`, and `internal/tui/gates` are out of
scope. The kit owns the primitives, the theme owns the style bundles, and the
gate describes its own patterns.

`TestLayoutGate_MatchesAllowlistCounts` compares the hits against
`internal/tui/gates/testdata/legacy_allowlist.yaml`. The allowlist pins an
exact hit count for each legacy path. A count that goes up means a new
hand-rolled surface. A count that goes down means the entry is stale, and you
must lower it or delete it.
