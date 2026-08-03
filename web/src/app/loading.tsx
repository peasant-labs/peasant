import { Skeleton, SkeletonList } from "@/lib/skeleton";

// Root-level route-change skeleton. Built from the shared skeleton primitives so
// the route transition pulses identically to every in-page loader. Mirrors the
// shared page idiom every real surface uses (container → breadcrumb → title
// block → a bordered list/canvas panel) so the transition reads as "this page is
// loading" rather than a phantom dashboard. The old KPI-card grid matched no
// current page rather than showing stale analytics-home copy.
export default function Loading() {
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      {/* Breadcrumb bar */}
      <Skeleton className="h-4 w-40" />

      {/* Title block: display title + one-line purpose */}
      <div className="space-y-2">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-4 w-96 max-w-full" />
      </div>

      {/* A bordered list/canvas panel — the body shape Home, Review and the Map
          picker all share. */}
      <SkeletonList rows={5} />
    </div>
  );
}
