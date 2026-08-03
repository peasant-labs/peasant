import { cn } from "@/lib/utils"

/**
 * The ONE skeleton system. Every loading placeholder in the app is built from
 * these four primitives so the pulse, color, and radius are identical
 * everywhere (before this, each surface hand-rolled `h-N bg-surface-hover
 * animate-shimmer` blocks with slightly different geometry).
 *
 * All four share one base: a square (`radius:0`) `bg-surface-hover` block with
 * the token-driven `animate-shimmer` pulse (which `globals.css` freezes to a
 * static fill under `prefers-reduced-motion`). Nothing here introduces color —
 * it all flows through `surface-hover`/`rule`.
 *
 *   import { Skeleton, SkeletonText, SkeletonRow, SkeletonList } from "@/lib/skeleton";
 *
 *   <Skeleton className="h-8 w-64" />            // one bar/block, size via className
 *   <SkeletonText lines={3} />                   // a paragraph of shimmer lines
 *   <SkeletonRow />                              // one list/table row
 *   <SkeletonList rows={5} header />             // a bordered, divided list panel
 */

/**
 * The atom. A single square shimmer block — the loading stand-in for any one
 * element (a title, a number, a canvas). Size and placement come from
 * `className`; everything else is fixed by the system.
 */
function Skeleton({
  as = "div",
  className,
  ...props
}: React.ComponentProps<"div"> & { as?: "div" | "span" }) {
  const Component = as
  return (
    <Component
      data-slot="skeleton"
      className={cn("bg-surface-hover animate-shimmer", className)}
      {...props}
    />
  )
}

/**
 * A block of text lines. Lines are uniform height with a slightly shortened
 * last line so the shape reads as a paragraph, not a rectangle. Use for prose,
 * rail panels, multi-line captions.
 */
function SkeletonText({
  lines = 3,
  className,
  lineClassName,
}: {
  /** How many shimmer lines to draw. */
  lines?: number
  /** Wrapper className (gap/spacing/width). */
  className?: string
  /** Per-line className (height; default `h-4`). */
  lineClassName?: string
}) {
  return (
    <div className={cn("flex flex-col gap-2", className)} aria-hidden>
      {Array.from({ length: lines }).map((_, i) => (
        <Skeleton
          key={i}
          // The last line is shortened so a multi-line block doesn't read as a
          // solid rectangle.
          className={cn("h-4", i === lines - 1 ? "w-2/3" : "w-full", lineClassName)}
        />
      ))}
    </div>
  )
}

/**
 * One list/table row of fixed height — the unit `SkeletonList` repeats. Exposed
 * on its own for places that interleave a shimmer row into a real list.
 */
function SkeletonRow({ className }: { className?: string }) {
  return <Skeleton className={cn("h-12", className)} />
}

/**
 * A bordered, divided list/table panel: an optional header row followed by
 * `rows` body rows — the shape Home, Review, the Map picker, and the route
 * loader all share. Carries the loading ARIA so callers don't re-declare it.
 */
function SkeletonList({
  rows = 5,
  header = true,
  className,
  rowClassName,
  label = "Loading",
}: {
  /** Body rows to draw. */
  rows?: number
  /** Draw a taller header row above the body rows. */
  header?: boolean
  /** Panel wrapper className. */
  className?: string
  /** Per-row className (height; default `h-12`). */
  rowClassName?: string
  /** Accessible label for the busy region. */
  label?: string
}) {
  return (
    <div
      className={cn("border border-rule bg-surface divide-y divide-rule", className)}
      aria-busy="true"
      aria-label={label}
    >
      {header && <Skeleton className="h-10" />}
      {Array.from({ length: rows }).map((_, i) => (
        <SkeletonRow key={i} className={rowClassName} />
      ))}
    </div>
  )
}

export { Skeleton, SkeletonText, SkeletonRow, SkeletonList }
