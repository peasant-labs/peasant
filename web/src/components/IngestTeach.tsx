import { HardDriveIcon } from "lucide-react";

/**
 * The ONE "run peasant ingest" teach/empty state, shared across every surface
 * that can be empty because nothing has been ingested yet: the Home picker, the
 * Map picker, and a project's Map canvas. Before this, the same idea was written
 * three different ways. One copy, one voice.
 *
 * - `variant="card"` — a bordered, self-contained block for page bodies
 *   (pickers, lists).
 * - `variant="fill"` — fills a flex region (e.g. the Map canvas): no border of
 *   its own, vertically centered.
 *
 * Copy is deliberately plain: what the command does, that it finds your AI
 * coding conversations, and that nothing leaves the machine. No jargon.
 */
export function IngestTeach({
  variant = "card",
  headline = "No AI work recorded yet",
}: {
  variant?: "card" | "fill";
  headline?: string;
}) {
  const inner = (
    <>
      <HardDriveIcon size={28} className="text-ink-4" aria-hidden />
      <div className="max-w-sm text-center">
        <p className="text-sm font-medium text-ink">{headline}</p>
        <p className="mt-1 text-[13px] leading-relaxed text-ink-3">
          In your terminal, run{" "}
          <code className="border border-rule bg-surface-hover px-1 py-0.5 font-mono text-xs">
            peasant ingest
          </code>
          . It scans this computer for your AI coding conversations — Claude
          Code, Codex, and others — and shows what it finds here. Everything
          stays on this machine.
        </p>
      </div>
    </>
  );

  if (variant === "fill") {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-4 px-5 py-8">
        {inner}
      </div>
    );
  }

  return (
    <div className="flex flex-col items-center gap-4 border border-rule bg-surface px-5 py-8 animate-fade-up">
      {inner}
    </div>
  );
}
