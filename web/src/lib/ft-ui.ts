'use client';

/**
 * Per-app typed barrel for the fairtrade `/ui` components used across the web app.
 *
 * The fairtrade `/ui` components spread `{...rest}` onto their underlying DOM
 * element at runtime, so native attributes (`onClick`, `className`, `aria-*`, …)
 * work — but the shipped `.d.ts` declarations omit those pass-through DOM types
 * (and over-require a few props the component already defaults internally). This
 * barrel re-exports the components this app uses, intersected with the right DOM
 * attributes and with the real prop optionality, so call-sites type cleanly
 * without `as any`. (Repo-wide fairtrade type thinness; a future fairtrade fix
 * removes the need for this.)
 *
 * Import fairtrade primitives from `@/lib/ft-ui`, not directly from
 * `@peasant-labs/fairtrade/ui`.
 */

import type {
  AnchorHTMLAttributes,
  ButtonHTMLAttributes,
  ChangeEvent,
  ComponentType,
  ElementType,
  HTMLAttributes,
  InputHTMLAttributes,
  ReactElement,
  ReactNode,
  RefObject,
} from 'react';
import type { Harness } from '@peasant-labs/schema';
import {
  Button as FtButton,
  Card as FtCard,
  Checkbox as FtCheckbox,
  Chip as FtChip,
  EmptyState as FtEmptyState,
  FeedbackPanel as FtFeedbackPanel,
  GroupedMultiSelect,
  Input as FtInput,
  Popover as FtPopover,
  ProviderIcon as FtProviderIcon,
  ProviderName as FtProviderName,
  RedactionReview,
  Skeleton as FtSkeleton,
  Tooltip as FtTooltip,
  WhereDoesThisGo,
  // fairtrade layout-pattern components
  RailShell as FtRailShell,
  RailSection as FtRailSection,
  SplitRail as FtSplitRail,
  StatTile as FtStatTile,
  StatGrid as FtStatGrid,
  GovTile as FtGovTile,
  ProviderBars as FtProviderBars,
  StepWizard as FtStepWizard,
  StepIndicator as FtStepIndicator,
  ConsentDialog as FtConsentDialog,
  ConsentSummary as FtConsentSummary,
  Treemap as FtTreemap,
  squarify,
  MapCanvas as FtMapCanvas,
  CommitGraph as FtCommitGraph,
  DataState as FtDataState,
  ConnectionPill as FtConnectionPill,
  TeachingEmptyState as FtTeachingEmptyState,
} from '@peasant-labs/fairtrade/ui';

/**
 * `Button` renders a `<button>` by default, or a real `<a>` with `as="a"`, so it
 * accepts the DOM attributes of both (`onClick`/`title`/`type` from button,
 * `href`/`target`/`rel` from anchor).
 */
export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    Pick<AnchorHTMLAttributes<HTMLAnchorElement>, 'href' | 'target' | 'rel'> {
  /** Visual treatment (default: secondary). */
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
  /** md = 36px, sm = 28px (default: md). */
  size?: 'md' | 'sm';
  /** Lucide component rendered before the label. */
  icon?: ElementType;
  /** Lucide component rendered after the label. */
  iconRight?: ElementType;
  /** Shows the busy spinner + aria-busy. */
  loading?: boolean;
  /** Toggle button: when defined adds aria-pressed. */
  pressed?: boolean;
  /** Element to render (default: button). */
  as?: 'button' | 'a';
}
export const Button = FtButton as unknown as ComponentType<ButtonProps>;

export interface CardProps extends HTMLAttributes<HTMLElement> {
  /** Render the card as an <a> (one big target) instead of a <div>. */
  link?: boolean;
}
export const Card = FtCard as unknown as ComponentType<CardProps>;

export interface ChipProps extends HTMLAttributes<HTMLSpanElement> {
  /** Semantic tone → .chip-ok / .chip-warn / .chip-err. */
  tone?: 'ok' | 'warn' | 'err';
  /** 'sm' → dense box. */
  size?: 'sm';
  /** Provider/company name → leads with the real brand mark. */
  brand?: string;
  /** Leading lucide icon component (ignored when `brand` is set). */
  icon?: ElementType;
  /** Mark as ui-chrome → lowercases the label. */
  chrome?: boolean;
  /** Render a real remove (x) button. */
  removable?: boolean;
  onRemove?: () => void;
  removeLabel?: string;
  children: ReactNode;
}
export const Chip = FtChip as unknown as ComponentType<ChipProps>;

/** A glossary-style hover/focus tooltip: a described, already-named trigger. */
export interface TooltipProps {
  /** Bubble contents (role=tooltip). */
  content: ReactNode;
  /** A single focusable, already-named trigger element; receives aria-describedby. */
  children: ReactElement;
  /** Bubble id (aria-describedby on the trigger); auto-generated if omitted. */
  id?: string;
}
export const Tooltip = FtTooltip as unknown as ComponentType<TooltipProps>;

/** A titled floating dialog toggled by a trigger button (owns its open state). */
export interface PopoverProps {
  /** Trigger button content (e.g. a label + chevron). */
  children: ReactNode;
  /** Accessible name for the floating dialog. */
  label: string;
  /** Visible heading; defaults to `label`. */
  title?: ReactNode;
  icon?: ComponentType;
  /** The dialog body. */
  content: ReactNode;
  footer?: ReactNode;
  triggerClassName?: string;
  id?: string;
}
export const Popover = FtPopover as unknown as ComponentType<PopoverProps>;

/** A styled native checkbox; the box IS the input. */
export interface CheckboxProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, 'onChange'> {
  checked?: boolean;
  defaultChecked?: boolean;
  onChange?: (checked: boolean, event: ChangeEvent<HTMLInputElement>) => void;
  disabled?: boolean;
  children?: ReactNode;
}
export const Checkbox = FtCheckbox as unknown as ComponentType<CheckboxProps>;

/** A single-line text field with label/hint/error wiring + optional leading icon. */
export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: ReactNode;
  hint?: string;
  error?: string;
  invalid?: boolean;
  /** Leading lucide icon component (wraps the field in `.input-ico`). */
  iconLeft?: ComponentType<{ size?: number }>;
}
export const Input = FtInput as unknown as ComponentType<InputProps>;

/** Left-aligned empty/zero-state block (ringed icon, title, message, action). */
export interface EmptyStateProps {
  icon?: ComponentType<{ size?: number; 'aria-hidden'?: boolean }>;
  title: ReactNode;
  message?: ReactNode;
  action?: ReactNode;
  children?: ReactNode;
  as?: ElementType;
}
export const EmptyState = FtEmptyState as unknown as ComponentType<EmptyStateProps>;

/** A centred inline state surface (loading / error / empty). */
export interface FeedbackPanelProps {
  variant?: 'loading' | 'error' | 'empty';
  title?: ReactNode;
  children?: ReactNode;
  icon?: ComponentType;
  className?: string;
}
export const FeedbackPanel = FtFeedbackPanel as unknown as ComponentType<FeedbackPanelProps>;

export interface SkeletonProps {
  /** aria-label announcing what is loading. */
  label?: string;
  /** Render the leading avatar block + two stacked lines (default: true). */
  avatar?: boolean;
  /** Number of full-width body lines below the row (default: 2). */
  lines?: number;
  className?: string;
}
export const Skeleton = FtSkeleton as unknown as ComponentType<SkeletonProps>;

// These take only their declared props at this app's call-sites — re-export as
// shipped (their declared types are sufficient).
export { GroupedMultiSelect, RedactionReview, WhereDoesThisGo };

/**
 * A single-color real brand mark for a harness (never a generic glyph);
 * see fairtrade's ProviderIcon.jsx for the machine-provenance rule this
 * enforces. `label` makes a standalone mark informative (role="img").
 */
export interface ProviderIconProps extends HTMLAttributes<HTMLSpanElement> {
  harness: Harness;
  /** Explicit px; omit to inherit the contextual icon size. */
  size?: number;
  /** Tint with the provider's accent token instead of inheriting currentColor. */
  accent?: boolean;
  label?: boolean | string;
}
export const ProviderIcon = FtProviderIcon as unknown as ComponentType<ProviderIconProps>;

/** Mark + the harness slug as a plain inline label (no chip border). */
export interface ProviderNameProps extends HTMLAttributes<HTMLSpanElement> {
  harness: Harness;
  /** Tint both the mark and the slug with the provider's accent token. */
  accent?: boolean;
}
export const ProviderName = FtProviderName as unknown as ComponentType<ProviderNameProps>;

// ---------------------------------------------------------------------------
// fairtrade layout-pattern components
//
// The JSDoc-generated `.d.ts` entries use `any` for ReactNode slots (RailShell,
// RailSection, SplitRail) or return `any` instead of JSX.Element.  Each
// component below is re-exported with a hand-written interface that reflects the
// typedef at the bottom of the source `.d.ts` so call-sites get precise prop
// checking.  No DOM-attr widening is applied — layout components consume only
// their declared slot props at these call-sites.
// ---------------------------------------------------------------------------

// -- Shell: RailShell / RailSection / SplitRail -----------------------------

/** Two-column canvas + sticky-rail shell (desktop) / bottom-sheet (mobile). */
export interface RailShellProps {
  /** Optional sticky top toolbar (mono chrome row). */
  toolbar?: ReactNode;
  /** Main content / canvas column (scrolls; min-width:0). */
  children: ReactNode;
  /** Rail contents — compose with <RailSection> children. */
  rail: ReactNode;
  /** Which side the desktop rail sits on (default: 'right'). */
  railSide?: 'left' | 'right';
  /** Bottom-sheet toggle label on mobile (default: 'details'). */
  sheetTitle?: string;
  /** Quiet right-side content in the sheet header (e.g. a count chip). */
  sheetMeta?: ReactNode;
  className?: string;
}
export const RailShell = FtRailShell as unknown as ComponentType<RailShellProps>;

/** One titled hairline section inside a rail; optionally collapsible. */
export interface RailSectionProps {
  /** Mono section header (lowercased by CSS). */
  title?: ReactNode;
  /** Optional Lucide icon shown before the title. */
  icon?: ComponentType;
  /** Quiet right-aligned header content (a count, a glyph). */
  meta?: ReactNode;
  children: ReactNode;
  /** When true, the header is a button that toggles the body. */
  collapsible?: boolean;
  /** Initial open state when collapsible (default: true). */
  defaultOpen?: boolean;
  className?: string;
}
export const RailSection = FtRailSection as unknown as ComponentType<RailSectionProps>;

/** Dual-rail variant: two independently-collapsible columns (outline / filters). */
export interface SplitRailProps {
  /** Content of the left column (e.g. an outline tree). */
  left: ReactNode;
  /** Content of the right column (e.g. filters). */
  right: ReactNode;
  /** Left column header (mono, lowercased; default: 'outline'). */
  leftTitle?: string;
  /** Right column header (mono, lowercased; default: 'filters'). */
  rightTitle?: string;
  leftIcon?: ComponentType;
  rightIcon?: ComponentType;
  leftMeta?: ReactNode;
  rightMeta?: ReactNode;
  className?: string;
}
export const SplitRail = FtSplitRail as unknown as ComponentType<SplitRailProps>;

// -- Stat tiles: StatTile / StatGrid / GovTile / ProviderBars ---------------

/** One KPI tile: mono eyebrow label, big tabular display number, optional sub line. */
export interface StatTileProps {
  /** Metric name (lowercase chrome; e.g. "transcripts"). */
  label: string;
  /** Headline figure (pre-formatted, e.g. "4.2M"). */
  value: ReactNode;
  /** Optional secondary line under the value. */
  sub?: ReactNode;
  icon?: ComponentType<{ className?: string }>;
  className?: string;
}
export const StatTile = FtStatTile as unknown as ComponentType<StatTileProps>;

/** Tile descriptor for a StatGrid row. */
export interface TileSpec {
  /** Stable key for React reconciliation. */
  key: string;
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  icon?: ComponentType<{ className?: string }>;
}

/** Responsive grid of StatTiles with a stable, capped column count. */
export interface StatGridProps {
  tiles: TileSpec[];
  className?: string;
}
export const StatGrid = FtStatGrid as unknown as ComponentType<StatGridProps>;

/** Governance fact tile: mono eyebrow, icon, value with an optional earth-tone accent. */
export interface GovTileProps {
  /** Governance dimension (e.g. "access", "your role"). */
  label: string;
  /** The setting (e.g. "members only", "contributor"). */
  value: ReactNode;
  icon?: ComponentType<{ className?: string }>;
  /** Optional earth-tone accent on the value. */
  tone?: 'amber' | 'teal' | 'olive' | 'clay' | 'mauve';
  className?: string;
}
export const GovTile = FtGovTile as unknown as ComponentType<GovTileProps>;

/** One item in a ProviderBars distribution. */
export interface ProviderDatum {
  /** Provider name (case preserved; e.g. "claude-code"). */
  label: string;
  value: number;
}

/** Labeled horizontal bar distribution — one row per provider (caller pre-sorts). */
export interface ProviderBarsProps {
  data: ProviderDatum[];
  /** Denominator for shares; defaults to the sum of values. */
  total?: number;
  /** Accessible name for the list (lowercase chrome; default: 'provider distribution'). */
  label?: string;
  className?: string;
}
export const ProviderBars = FtProviderBars as unknown as ComponentType<ProviderBarsProps>;

// -- Step wizard: StepWizard / StepIndicator --------------------------------

/** One step descriptor for StepWizard / StepIndicator. */
export interface WizardStep {
  /** Stable id for aria wiring and completed/reachable sets. */
  id: string;
  /** Lowercase chrome label shown under the step's number marker. */
  label: ReactNode;
}

/** Arguments passed to StepWizard's renderStep render-prop. */
export interface WizardBodyArgs {
  step: WizardStep;
  index: number;
  isLast: boolean;
  /** Report whether the step may advance (gates the continue button). */
  setValid: (valid: boolean) => void;
}

/**
 * Uncontrolled wizard: numbered step rail + per-step body slot + sticky action
 * footer.  Advance with continue/submit; jump back to completed steps.
 */
export interface StepWizardProps {
  steps: WizardStep[];
  onComplete?: (args: { completed: Set<string> }) => void;
  /** Render-prop for the active step's body. */
  renderStep?: (args: WizardBodyArgs) => ReactNode;
  /** Index-aligned step bodies (alternative to renderStep). */
  children?: ReactNode[];
  continueLabel?: string;
  submitLabel?: string;
  backLabel?: string;
  'aria-label'?: string;
}
export const StepWizard = FtStepWizard as unknown as ComponentType<StepWizardProps>;

/** Pure/controlled step-progress rail — numbered markers joined by connector lines. */
export interface StepIndicatorProps {
  steps: WizardStep[];
  /** Id of the active step. */
  current: string;
  completed?: Set<string>;
  reachable?: Set<string>;
  onJump?: (id: string) => void;
  'aria-label'?: string;
}
export const StepIndicator = FtStepIndicator as unknown as ComponentType<StepIndicatorProps>;

// -- Consent dialog: ConsentDialog / ConsentSummary -------------------------

/** One row in a ConsentSummary or ConsentDialog axes grid. */
export interface ConsentAxis {
  /** Lucide component for the row's icon chip. */
  icon?: ElementType;
  /** Mono, lowercase axis label (e.g. "identity", "data access"). */
  key: string;
  /** The chosen value (NOT lowercased — may contain user content). */
  value: ReactNode;
  /** Optional "who can see / what changes" note under the value. */
  scope?: ReactNode;
  /** Icon-chip tone: reveal=amber, open=teal, restricted=clay. */
  tone?: 'reveal' | 'open' | 'restricted';
}

/** Reusable aligned grid of consent axes (icon chip + mono key + value + scope). */
export interface ConsentSummaryProps {
  axes: ConsentAxis[];
  /** Optional mono eyebrow above the rows. */
  caption?: string;
  className?: string;
}
export const ConsentSummary = FtConsentSummary as unknown as ComponentType<ConsentSummaryProps>;

/**
 * Scrim + bordered governance modal: head / body (intro + summary + optional
 * consent checkbox) / foot (cancel + amber confirm).  Focus-trapped; Escape and
 * scrim click cancel.
 */
export interface ConsentDialogProps {
  open: boolean;
  /** Mono lowercase head title. */
  title: ReactNode;
  /** Reading-prose lede above the summary. */
  intro?: ReactNode;
  axes?: ConsentAxis[];
  summaryCaption?: string;
  /** Extra content rendered after the summary. */
  children?: ReactNode;
  confirmLabel?: string;
  confirmIcon?: ElementType;
  cancelLabel?: string;
  onCancel: () => void;
  onConfirm: () => void;
  /** Gate the primary button behind the consent checkbox (default: true). */
  requireConsent?: boolean;
  consentLabel?: ReactNode;
  /** Primary button treatment (default: 'primary'). */
  tone?: 'primary' | 'danger';
  busy?: boolean;
  labelId?: string;
  returnFocusRef?: RefObject<HTMLElement>;
}
export const ConsentDialog = FtConsentDialog as unknown as ComponentType<ConsentDialogProps>;

// -- Treemap ----------------------------------------------------------------

/** One datum in a Treemap — tile area proportional to value. */
export interface TreemapDatum {
  /** Stable key (also the default selection token). */
  id: string;
  /** Human label (e.g. "ingest/stream.go"). */
  label: string;
  /** Non-negative sizing weight; tile area ∝ value. */
  value: number;
  /** 0..4 monochrome ramp level (reinforces a second metric). */
  intensity?: number;
  /** Noun for the aria-label (e.g. "lines", "calls"; default: 'lines'). */
  unit?: string;
}

/** Squarified, strict-monochrome treemap; clicking a tile fires onSelect. */
export interface TreemapProps {
  data?: TreemapDatum[];
  onSelect?: (id: string, datum: TreemapDatum) => void;
  height?: number;
  ariaLabel?: string;
  className?: string;
}
export const Treemap = FtTreemap as unknown as ComponentType<TreemapProps>;

/** Lay items out as a squarified treemap filling [0, 0, width, height]. */
export { squarify };

// -- MapCanvas --------------------------------------------------------------

/** One node in a MapCanvas graph. */
export interface MapNode {
  /** Stable key (repo-relative path). */
  id: string;
  label: string;
  kind: 'folder' | 'file';
  /** Lines of code — node width ∝ loc. */
  loc?: number;
  /** 0..4 monochrome coverage ramp (the fill). */
  coverage?: number;
  /** Parent id (forms the tree). */
  parent?: string;
  /** Violation count — rendered as a clay badge. */
  violations?: number;
}

/** One edge in a MapCanvas graph. */
export interface MapEdge {
  from: string;
  to: string;
  /** solid = structure, dashed = activity (default: 'structure'). */
  kind?: 'structure' | 'activity';
  /** Stroke width ∝ weight (never hue; default: 1). */
  weight?: number;
}

/**
 * Interactive code-structure map: pan, zoom, semantic zoom (overview / folders
 * / files), minimap, node search.  Pure: no data fetching, deterministic layout.
 */
export interface MapCanvasProps {
  data?: { nodes: MapNode[]; edges?: MapEdge[] };
  grain?: 'overview' | 'folders' | 'files';
  selectedId?: string;
  onSelect?: (id: string | null, node: MapNode | null) => void;
  height?: number;
  ariaLabel?: string;
  className?: string;
}
export const MapCanvas = FtMapCanvas as unknown as ComponentType<MapCanvasProps>;

// -- CommitGraph ------------------------------------------------------------

/** One commit in a CommitGraph history (newest first). */
export interface Commit {
  id: string;
  /** 0 = main line; >0 = a branch lane. */
  lane: number;
  parents?: string[];
  message: string;
  branch?: string;
  /** A recorded AI session sits behind this commit. */
  session?: boolean;
  /** This commit merged a branch back in. */
  merged?: boolean;
  tip?: boolean;
  /** Pre-formatted relative time label (e.g. "8m ago"). */
  time?: string;
}

/** Lane gutter + selectable commit rows (newest first). */
export interface CommitGraphProps {
  commits: Commit[];
  selectedId?: string;
  onSelect?: (commit: Commit) => void;
  hasMore?: boolean;
  onShowOlder?: () => void;
  label?: string;
  className?: string;
}
export const CommitGraph = FtCommitGraph as unknown as ComponentType<CommitGraphProps>;

// -- Connection state: DataState / ConnectionPill / TeachingEmptyState ------

/**
 * Discriminator wrapper: loading → skeleton; disconnected/error → lost-connection
 * panel; empty → emptyState slot; else → children.
 */
export interface DataStateProps {
  status?: 'live' | 'connecting' | 'disconnected';
  loading?: boolean;
  /** Truthy shows the lost-connection panel; a string overrides the panel body. */
  error?: boolean | string;
  empty?: boolean;
  children?: ReactNode;
  /** Rendered when `empty` is true (e.g. a <TeachingEmptyState/>). */
  emptyState?: ReactNode;
  onRetry?: () => void;
  skeletonRows?: number;
  className?: string;
}
export const DataState = FtDataState as unknown as ComponentType<DataStateProps>;

/** Small glanceable connection indicator: icon + word (state never relies on color alone). */
export interface ConnectionPillProps {
  status?: 'live' | 'connecting' | 'disconnected';
  showNote?: boolean;
  className?: string;
}
export const ConnectionPill = FtConnectionPill as unknown as ComponentType<ConnectionPillProps>;

/** Empty state that TEACHES the mechanism: title, guidance prose, copy-able command chip. */
export interface TeachingEmptyStateProps {
  icon?: ComponentType;
  title: ReactNode;
  body?: ReactNode;
  /** The command to teach (shown in a `$`-prefixed mono chip with a copy button). */
  command?: string;
  /** Privacy line; defaults to "nothing leaves your machine". Pass null to omit. */
  privacy?: ReactNode;
  /** Heading level for the title (default: 'h3'). */
  as?: ElementType;
  className?: string;
}
export const TeachingEmptyState = FtTeachingEmptyState as unknown as ComponentType<TeachingEmptyStateProps>;
