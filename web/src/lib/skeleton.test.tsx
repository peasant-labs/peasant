import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Skeleton, SkeletonText, SkeletonRow, SkeletonList } from "./skeleton";

// The shared skeleton contract every surface depends on: one pulse class
// (`animate-shimmer`), one fill (`bg-surface-hover`), and consistent geometry.
// If these drift, the per-surface loaders stop matching each other again.

describe("Skeleton primitives", () => {
  it("Skeleton carries the shared shimmer + fill and merges className", () => {
    const { container } = render(<Skeleton className="h-8 w-64" />);
    const el = container.querySelector('[data-slot="skeleton"]')!;
    expect(el).toHaveClass("animate-shimmer", "bg-surface-hover", "h-8", "w-64");
  });

  it("SkeletonText draws N shimmer lines and shortens the last", () => {
    const { container } = render(<SkeletonText lines={3} />);
    const lines = container.querySelectorAll(".animate-shimmer");
    expect(lines).toHaveLength(3);
    // First lines fill; the last is shortened so it reads as a paragraph.
    expect(lines[0]).toHaveClass("w-full");
    expect(lines[2]).toHaveClass("w-2/3");
  });

  it("SkeletonRow is a single shimmer row", () => {
    const { container } = render(<SkeletonRow />);
    const rows = container.querySelectorAll(".animate-shimmer");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveClass("h-12");
  });

  it("SkeletonList renders a busy panel with header + rows", () => {
    const { getByLabelText } = render(<SkeletonList rows={4} />);
    const panel = getByLabelText("Loading");
    expect(panel).toHaveAttribute("aria-busy", "true");
    // header (1) + body rows (4) = 5 shimmer blocks.
    expect(panel.querySelectorAll(".animate-shimmer")).toHaveLength(5);
  });

  it("SkeletonList can drop the header and take a custom label", () => {
    const { getByLabelText } = render(
      <SkeletonList rows={3} header={false} label="Loading projects" />,
    );
    const panel = getByLabelText("Loading projects");
    expect(panel.querySelectorAll(".animate-shimmer")).toHaveLength(3);
  });
});
