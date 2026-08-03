# Peasant Web — Design Contract

> **This is the single source of truth for the dashboard redesign.** Every
> surface agent codes against this document. The v2 transcript viewer
> (`web/src/components/session-detail/v2/`) is the reference implementation —
> it now carries role color and labeled rails, and other surfaces extract
> their patterns from it. When this doc and a component disagree, this doc
> wins.

The goal: make every surface of the local web UI look, feel, and *narrate*
like the v2 transcript viewer — a **monochrome-editorial** system — while
restructuring the workflows so the product's values are legible:

1. **You own your data / local-first.** Nothing leaves the machine until the
   user explicitly chooses. Say so, everywhere it's relevant.
2. **Redaction before share.** Redaction is a prominent, trust-building,
   first-class step — never a quiet checkbox.
3. **The commons is the end goal.** A transcript/project's destination is the
   public-interest commons. The journey runs *toward* it.

Attribution/attestation is present but secondary to the three above.

---

## 1. Visual system (implemented in `globals.css`)

### 1.1 Hard rules (non-negotiable)

- **Radius is `0`. Everywhere.** No `rounded`, `rounded-sm/md/lg/xl/full`.
  All radius tokens are already `0`; never reintroduce rounding. Pills,
  badges, avatars, progress bars, inputs, buttons — all square.
- **No shadows.** No `shadow-sm/md/lg`, no `drop-shadow`. Depth comes from
  hairline borders and `--surface-elev`, not blur.
- **No backdrop blur.** No `backdrop-blur-*`. Surfaces are opaque.
- **No ad-hoc color** — no raw hex, no Tailwind numbered scales (`blue-500`),
  no opacity-tinted structural surfaces. *All* color flows through named
  tokens: monochrome ink, semantic (success/warning/danger), **role**,
  **provider**, and mark. Color is used deliberately to aid comprehension —
  roles, providers, outcomes, status — never as decoration.
- **No opacity-tinted surfaces** for structure (`bg-white/80`,
  `border-stone-200/80`). Use the semantic tokens.
- **Tabular numerals** (`.tabular-nums` / `font-mono`) for every number that
  sits in a column, KPI, duration, token count, count badge, or timestamp.
- **`focus-mono`** utility on every interactive element (replaces shadcn's
  default ring).

### 1.2 Color tokens (use the Tailwind utility, never raw hex/hsl)

Surfaces & ink:

| Utility | Token | Use |
|---|---|---|
| `bg-canvas` | `--canvas` | Page background |
| `bg-surface` | `--surface` | Cards, panels, table bg |
| `bg-surface-elev` | `--surface-elev` | Elevated card (no shadow — slightly lifted bg) |
| `bg-surface-hover` | `--surface-hover` | Row / item hover, subtle fills |
| `text-ink` | `--ink` | Primary text |
| `text-ink-2` | `--ink-2` | Secondary text |
| `text-ink-3` | `--ink-3` | Tertiary text, captions, eyebrows |
| `text-ink-4` | `--ink-4` | Placeholder / disabled |
| `border-rule` | `--rule` | Default hairline border/divider |
| `border-rule-strong` | `--rule-strong` | Emphatic divider, progress fill (neutral) |
| `bg-rail` / `.v2-rail` | `--rail` | Timeline / vertical rails |
| `bg-mark` `text-mark-fg` | `--mark` | **The CTA fill** (= black light / white dark). Primary buttons, active nav, active step. |

Semantic (the *only* permitted color, and only for genuine semantics):

| Utility | Token | Meaning |
|---|---|---|
| `text-danger` `bg-danger` `bg-danger-soft` | `--danger*` | Errors, destructive, failed outcome |
| `text-warning` `bg-warning` `bg-warning-soft` | `--warning*` | Caution, partial outcome, "needs redaction" |
| `text-success` `bg-success` `bg-success-soft` | `--success*` | Done, redacted-&-safe, resolved outcome, contributed |
| `bg-diff-add` / `bg-diff-del` (+ `-text`,`-gutter`,`-accent`) | diff | Diff blocks only |

Role & provider:

| Utility | Token | Use |
|---|---|---|
| `bg-role-user-soft` `text-role-user` `border-role-user` | `--role-user*` | User turn — accent bar, glyph, faint row tint |
| `bg-role-assistant-soft` `text-role-assistant` `border-role-assistant` | `--role-assistant*` | Assistant turn — same |
| `text-provider-claude` … `text-provider-codex` | `--provider-*` | Provider brand marks |

**Usage rule:** role color is applied to the **accent bar, the role glyph, and
the soft row background** — never to body text. This keeps WCAG AA trivial
(body text stays `--ink` on a near-white tint). Tool / system / subagent rows
stay monochrome — only the two conversational roles are colored.

Outcome badges (resolved/partial/failed), share state (private/redacted/
public), provenance — **must** map to `success` / `warning` / `danger` /
`ink` + `-soft` fills, never emerald/amber/red literals.

Charts: use the `--chart-*` CSS vars (`--chart-grid`, `--chart-axis`,
`--chart-tick`, `--chart-line`, `--chart-dot-stroke`, `--chart-subtle`).
Data series default to ink; use semantic tokens only when the series *is* a
semantic dimension (e.g. failure rate → danger).

**Intensity ramp:**

| Utility | Token | Use |
|---|---|---|
| `bg/text/border-intensity-0` | `--intensity-0` | Empty / faintest fill (≈ `surface-hover`) |
| `bg/text/border-intensity-1` | `--intensity-1` | Faint (≈ `rule`). **Dark matter's border is `intensity-1`** — never an ad-hoc opacity. |
| `bg/text/border-intensity-2` | `--intensity-2` | Medium (≈ `rule-strong`) |
| `bg/text/border-intensity-3` | `--intensity-3` | Strong — text-safe: ≥ 4.5:1 on `canvas` in both themes |
| `bg/text/border-intensity-4` | `--intensity-4` | Full (= `ink`) — text-safe on `canvas` |

The ramp is the **single** monochrome 0–4 intensity scale. It applies to
**fill, text, and border** of map nodes, and to heatmap cells. The
component-local heatmap levels (`ActivityHeatmap`'s
`surface → rule → rule-strong → ink-3 → ink` ladder) are this ramp, now
named — new code uses the tokens, never a hand-picked ladder. Use
`intensity-3`/`-4` for any text rendered directly on `canvas` (e.g. dimmed
node labels stop at `intensity-3`, not lighter).

**Canvas graph tokens and delta states:**

| Utility | Token | Use |
|---|---|---|
| `stroke-edge` / `border-edge` | `--edge` | Default hairline graph edge (1px, structure solid / activity dashed) |
| `stroke-edge-strong` / `border-edge-strong` | `--edge-strong` | Emphasis / aggregate edge |
| — | `--danger` | Violations (cycle, wrong-way edge). The only red on the canvas. |

`--edge`/`--edge-strong` sit near `--rule`/`--rule-strong` but are tuned one
step for 1px strokes on `canvas`; do not substitute `rule` tokens for graph
edges. Graph **delta states** (Review's changed slice, and anywhere a graph
shows base-vs-head):

- **NEW** node/edge: `--rule-strong` border + a `NEW` eyebrow tag
  (`.v2-eyebrow`). Never a color.
- **Removed** node/edge: dashed border + `ink-4` label + **no fill**. Never
  opacity, never strikethrough.
- The `diff-*` tokens are **never used on the canvas** — they remain "diff
  blocks only" (§1.2 semantic table stands).

### 1.3 Typography

- **Body & display:** Chivo — `font-[family-name:var(--font-display)]` for
  page titles / numeric heroes; default body is Chivo already.
- **Mono:** JetBrains Mono (`font-mono`) — code, IDs, durations, token counts,
  paths, CLI commands, any tabular number.
- **Eyebrow:** section/group labels use `.v2-eyebrow` (11px, uppercase,
  `0.08em` tracking, `--ink-3`, always sans). Use it for *every* section
  header label instead of ad-hoc `text-xs font-medium uppercase`.
- **Prose:** long-form copy uses `.v2-prose`.
- Scale: titles ~`text-2xl` (display, `tracking-tight`), section headings
  `text-sm font-medium text-ink`, body `text-[13px]`–`text-sm`, meta
  `text-xs text-ink-3`. No body text below 12px except the eyebrow (11px).

### 1.4 Layout idiom (copy from `SessionDetailV2.tsx`)

- **Page container:** `max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6`
  (narrower content surfaces may use `max-w-6xl`/`7xl` — pick one per page and
  keep it). Wrap with `animate-fade-up` on first paint.
- **Top of every page:** `Breadcrumbs` → page title block (display title +
  one-line `text-sm text-ink-3` purpose) → content.
- **Cards/panels:** `border border-rule bg-surface`. Header row:
  `flex items-center justify-between px-5 py-3 border-b border-rule` with a
  `text-sm font-medium text-ink` label; body `px-5 py-4`. Dividers between
  rows: `divide-y divide-rule`. Row hover: `hover:bg-surface-hover`.
- **Two-column with rail:** `grid gap-5 grid-cols-1 lg:grid-cols-[1fr_260px]`,
  rail `sticky top-[108px]`. Use only where a page has genuine navigation/
  filter context — **Map and Review** (the canvas surfaces), the viewer, and
  Contribute. Canvas surfaces may widen the rail to **320px**
  (`lg:grid-cols-[1fr_320px]`); pick one width per page and keep it.
- **Tab strips / step indicators:** square, underline or `bg-mark`
  active state, `--rule` track. No pills.
- **Error pill:** `text-[13px] text-danger px-3 py-2 border border-danger/30 bg-danger-soft`.
- **Empty/loading:** `Skeleton` is square + `animate-shimmer`. Empty states
  are a teaching moment — see §3.
- **Grid background:** the `.bg-grid` + `.grid-snap` treatment is part of the
  brand; the shell already snaps. Don't fight it with full-bleed colored
  blocks.

**Pannable canvases** apply to every pan/zoom graph canvas, including the Map
and Review's changed slice:

- **Zoom controls:** square on-screen buttons (the React Flow controls are
  already squared in `globals.css`). No round anything.
- **Keyboard model (full, required):** arrow keys traverse nodes spatially —
  layer order, then sort order within the layer. `Enter` selects the focused
  node and opens its rail panel. `Shift+Enter` (or `E`) expands/zooms into it.
  `Escape` deselects; pressed again, zooms out one level. Edge violations are
  reachable through the owning node's rail panel — **never hover-only**.
- **No scroll-hijack without a modifier:** plain wheel scrolls the page;
  wheel+modifier (or the on-screen controls) zooms the canvas.
- **Selection outline:** `--rule-strong`. Not a glow, not a shadow, not a
  color.
- **Motion:** `prefers-reduced-motion` is honored for pan/zoom easing
  (instant repositioning when set).
- **Loading:** a square shimmer skeleton (`animate-shimmer`) over the canvas
  region — never a spinner-only or blank canvas.
- **Responsive:** below the two-column breakpoint the rail collapses to a
  **bottom sheet** and the canvas gets a fixed-height viewport.

### 1.5 Motion

`animate-fade-up` / `animate-fade-in` for first paint; `.stagger-1..6` for
lists. 150–300ms transitions (`transition-colors`, `transition-all
duration-200`). Respect `prefers-reduced-motion`. No decorative motion.

---

## 2. Information architecture (the lifecycle)

The top nav is still the lifecycle, read left→right from "on your machine"
outward: understand what's on your machine, review what's changing, then (and
only then) choose what leaves. **3 items only:**

| # | Nav label | Route | Role |
|---|---|---|---|
| 1 | **Map** | `/` (project picker when >1 project; the map otherwise) → `/map/[...]` per-project map | Learn the project. Structure + traceability + time: deterministic layered canvas, grain control, rail (project panel / node panel), time strip. |
| 2 | **Review** | `/review` → `/review/[...]` change detail (branch via `?branch=` — branch names contain slashes) | Judge a change: branch-scoped slice of the map + the recorded work behind it. Caption, changed slice, work rail, footnotes. |
| 3 | **Contribute** | `/share` (the route **keeps its path**; the nav label is **Contribute** — "Share" retires as a label) | The deliberate path out. Choose → Labels → Redact → Submit. |

The **session viewer** keeps its deep-linkable route
(`/projects/[name]/[id]`); it is not a nav item. Map and Review link into it
(scoped entry), and it remains the design-system reference implementation.

> **Navigation exclusions:** Analytics/Insights and Commons are not lifecycle
> nav items. `lib/insights/*` and `lib/quality/{types,utils,literature}.ts`
> remain dependencies of the mounted viewer. "The commons" remains a concept
> in contribution copy, not a page. "Review" means reviewing a code change;
> annotation work keeps the word "annotations."

Rules:

- Route **paths**: `/`, `/map/...`, `/review`, `/review/...`, `/share`, plus
  the viewer's existing `/projects/...` deep-link routes. No `/analytics`, no
  `/commons`, no `/contribute` path — **Contribute is a label, not a route**.
- Active nav item: `text-ink` + a square `bg-mark` underline or left/bottom
  marker; inactive `text-ink-3 hover:text-ink hover:bg-surface-hover`. No
  aqua, no rounded pill.
- Logo `peasant` stays, `font-display`, `text-ink`.
- Connection status: square, `font-mono text-xs`, monochrome dot for
  connected (`bg-ink`/`bg-success`), `text-danger` + `bg-danger-soft` when
  disconnected. No aqua/coral.
- Breadcrumbs reflect the lifecycle and stay consistent across surfaces. When
  the viewer is entered from Map or Review, the breadcrumb carries the origin
  (`Map · ingest › task` / `Review · feat/graph-cache › task`) and returns
  there with selection intact.

---

## 3. Values-driven UX rules (the "workflow" part)

These are requirements, not flavor. Apply per surface.

**Flow design — fast path first (overrides verbosity):**

- **Easiest possible default, customizable at every step.** Every multi-step
  flow must complete on the happy path in the fewest possible actions with
  sane defaults pre-applied. Customization is *progressive disclosure* —
  hidden behind a "Customize" affordance, never forced on the user.
- **Projects are the primary unit of contribution, not sessions.** Users
  choose and send *entire projects* by default; individual sessions are a
  drill-in detail for excluding/tuning, not the primary selection surface.
- **Concise, not a sermon.** Microcopy is one short line, scannable. No
  multi-sentence paragraphs explaining the philosophy in the UI. State the
  local→commons boundary as a single short line or chip *before* the action —
  never a paragraph card. The values are expressed through the *design and
  defaults*, not walls of text.
- **Visible steps, fast by default.** The Contribute flow shows its steps
  explicitly so the user can step in and customize: **Choose → Labels →
  Redact → Submit**. Steps are *visible and navigable*, not hidden behind
  disclosure. But sane defaults make the happy path fast — the user can
  advance straight through (Labels is optional/skippable; Redact defaults to
  **maximum** — every detected pattern stripped). Redaction is safe by
  *default*, not by gate: Submit is reachable as soon as a non-empty selection
  exists, and the user dials redaction *down* deliberately, never up under
  pressure. "Easy" = good defaults + few clicks, **not** fewer visible steps.
- **Choose step starts empty.** Nothing is selected by default; the user opts
  in. Provide an explicit **Select all / Deselect all** control. (A
  `?sessionId=` deep-link still preselects exactly that one session.)
  **Evidence-set deep link:** arriving with
  `?sessions=<id,id,…>` — the `Contribute sessions →` exits from a Review
  change or a Map node — opens Choose **filtered** to those sessions, with a
  one-click **"Select these N sessions"** affordance. Filtered, **not
  preselected**: the opt-in posture survives; one click keeps it fast. The
  single-session `?sessionId=` behavior is the degenerate case. Cold entry
  (no query param) is unchanged: empty, unfiltered.

**Local-first ownership (everywhere):**

- The **Map header** leads with the fact that everything is local: a plain
  statement like "N sessions, on your machine. Nothing has left it." Surface
  the local store path/concept. The user is the owner; the AI vendor is not
  in the loop until *they* decide. The values voice appears on the Map header
  and the Contribute boundary — one line each, never a sermon.
- Anywhere data could leave (the Contribute flow), state the boundary
  explicitly before the action, not after.
- Empty states teach the lifecycle (the existing `EmptyStateTutorial`
  Ingest→Analyze→Share spine — run `peasant ingest`, the map lights up).

**Redaction before share (Contribute surface):**

- The wizard's redaction step is visually the heaviest step — give it the
  most space, a clear "what will be stripped / what remains" preview framing,
  and copy that builds trust ("nothing has left your machine").
- The Submit step always passes *through* redaction in the linear flow, and
  redaction defaults to maximum — safe by default rather than gated by an
  approval click. The step indicator should still make redaction feel
  load-bearing (square, `bg-mark` active, `bg-success` completed, `--rule`
  pending — never emerald).
- Frame redaction as *protecting the contributor*, not as a chore.

**Commons as the end goal (Contribute submit step only — there is no Commons page):**

- The final share step is framed as *contributing to the commons*, not
  "uploading". Name the destination, what the commons gains, and that the
  contributor is seen/attributed. "The commons" is a concept in copy, not a
  navigable surface — never link to `/commons`.

Copy voice: plain, direct, a little defiant on behalf of the user. Never
corporate. "Your work." "On your machine." "The commons." Short sentences.

---

## 4. Per-surface briefs

> These briefs define the current Map / Review / Contribute composition.

- **Foundation — primitives:** `components/ui/*`, `DataTable`,
  `SessionFilterBar`, `Breadcrumbs`, `MultiSelectPopover`, `ProviderIcon`.
  Strip shadcn rounding/shadow/blur; map `primary→mark`,
  `secondary→surface/ink`, `destructive→danger`, borders→`rule`; `focus-mono`;
  tabular-nums on numeric cells. **Public props/APIs unchanged.**
- **Foundation — shell + nav:** §2. Lifecycle nav, **3 items: Map / Review /
  Contribute**; monochrome; square.
- **Map (`/`, `/map/[...]`):** the comprehension cockpit. Header strip carries
  the local-first ledger line (§3). The toolbar provides semantic grain control
  and node search. Canvas per §1.4 *Pannable canvases* + the §1.2 intensity
  ramp/edge tokens: square nodes, hairline edges, traceability coverage owns
  fill/text/border ink — dark matter dims on the ramp, never hides. Rail
  (320px): project panel (identity + coverage + KPI footnote tiles + the
  reverse-chron session list — the guaranteed browse path) or node panel
  (identity, traceability, "shaped by" tasks/commits, footnote metrics).
  Time strip under the canvas: sparkline + branch chips, with a history
  playhead when commit data is available.
  Metrics are footnotes, never headlines; no composite score, ever.
- **Review (`/review`, `/review/[...]`):** the change-review surface. List:
  open branches first,
  `font-mono` numerics, structure column carries caption-grade facts (edge
  deltas, ⚠ violations in danger); plain-stated empty states (no branches /
  not a git repo). Detail: deterministic caption (every fragment drills to
  evidence, no adjectives) → changed slice (same layout positions as the
  full map; delta states per §1.2 — NEW = `rule-strong` border + `NEW`
  eyebrow, removed = dashed + `ink-4` + no fill; `diff-*` never on canvas)
  → work rail (bound/candidate sessions and tasks; unrecorded commits marked
  plainly) → footnotes (output tokens summed, cost only where known). Exits:
  view diff, open in Map, Contribute sessions. **No verdicts, no
  accept/reject machinery, no safety claims.**
- **Contribute (`/share` route, Contribute label):** §3 —
  project-primary; visible steps **Choose → Labels → Redact → Submit**; fast
  by default; Choose starts empty + select-all; evidence-set deep link per §3
  (`?sessions=` filtered-not-preselected); redaction safe-by-default
  (maximum), Submit reachable once a selection exists.
  `RedactionDiffView` is LIVE here (`diff-add/diff-del` tokens).
- **Viewer (`/projects/[name]/[id]`):** `session-detail/v2/` is the
  reference implementation and the destination of every "why" click. It gains
  **scope** (whole session / task / file / change), an origin-aware breadcrumb
  (§2), and a touched-files rail (`font-mono`; edits vs reads visually
  distinguished — edits drive attribution, reads are context). Annotations
  stay minimal: plain chips, no workbench.
- Analytics/Insights, Explore, and the old annotation review queue are not
  mounted lifecycle surfaces; `/review` is exclusively change review.
- **Verify:** production build, automated tests, visual gates, and accessibility
  checks must all pass.

---

## 5. Audit checklist (every agent runs this on its own diff before reporting)

From `web/src` (excluding `components/session-detail/v2/`), the agent's diff
must introduce **zero** of:

```
rounded-(sm|md|lg|xl|2xl|3xl|full)        # radius must stay 0
shadow-(sm|md|lg|xl|2xl)|drop-shadow      # no shadows
backdrop-blur                              # no blur
\b(bg|text|border|ring)-(aqua|emerald|amber|blue|coral|red|green|sky|indigo|rose|teal)-\d   # ad-hoc color
ring-ring|ring-offset                      # use focus-mono
bg-(white|black)\b|/[0-9]{2}\]?\s*$        # opacity-tinted structural surfaces
```

Canvas surfaces only (`web/src/components/map/` and any graph canvas) must
additionally introduce **zero** of:

```
(bg|text|border|stroke|fill)-diff-        # diff-* tokens never on the canvas (§1.2)
(bg|text|border)-(ink|rule)[^-a-z]?.*opacity|opacity-\d+.*dark.matter   # dark matter dims via the intensity ramp, not opacity
```

Also verify, in the agent's surfaces:

- [ ] All numbers in columns/KPIs use `tabular-nums`/`font-mono`
- [ ] Every interactive element has `focus-mono` and `cursor-pointer`
- [ ] Section labels use `.v2-eyebrow`
- [ ] Monochrome ramps use `intensity-0..4` tokens — no hand-picked ladders
- [ ] Graph edges use `edge`/`edge-strong` (violations `danger`); selection
      outline is `rule-strong`, never a glow
- [ ] Canvas keyboard model (§1.4 *Pannable canvases*) implemented in full;
      no scroll-hijack without a modifier
- [ ] Contrast ≥ 4.5:1 in **both** light and dark (tokens already satisfy
      this — don't override them)
- [ ] `prefers-reduced-motion` respected; no emoji as icons (Lucide only)
- [ ] Page uses the §1.4 container + breadcrumb + title idiom
- [ ] Values copy (§3) present where the surface touches the lifecycle
- [ ] `pnpm build` + `pnpm typecheck` clean for touched files; existing
      Vitest tests still pass (`pnpm test`)
- [ ] Component public props/APIs unchanged unless the brief says otherwise

If a deviation is unavoidable, leave a `// DESIGN_SYSTEM: <why>` comment and
flag it in the final report.
