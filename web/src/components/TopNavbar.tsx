"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Moon, Search, Settings, Sun, type LucideIcon } from "lucide-react";
import { useTheme } from "@/hooks/useTheme";
import { Button } from "@/lib/ft-ui";
import { isSectionActive, visibleNavSections } from "@/lib/nav/sections";
import { useServerCapabilities } from "@/contexts/ServerCapabilitiesContext";
import { OPEN_COMMAND_PALETTE_EVENT } from "@/components/command/CommandPalette";

/** The glyph a nav section's header link leads with. */
const SECTION_ICONS: Partial<Record<string, LucideIcon>> = { settings: Settings };

/**
 * The local app header: one row, the `peasant` home link on the left, then
 * search, the nav sections other than home, and the theme toggle on the right.
 *
 * The links come from the section registry (lib/nav/sections.ts): a nav
 * section shows once this app has a page for it, and `home` is the brand link.
 * Analytics, changes and the code map are route-only sections, so nothing here
 * links to them; their routes still resolve. Connection state is not in the
 * header: the offline notice under it speaks only when the app is unreachable.
 */
export function TopNavbar() {
  const pathname = usePathname();
  const { theme, toggle } = useTheme();
  const { capabilities } = useServerCapabilities();
  const links = visibleNavSections(capabilities).filter((section) => section.href !== "/");
  const nextTheme = theme === "light" ? "dark" : "light";

  return (
    <header className="flex h-[var(--nav-h)] items-center justify-between gap-3 border-b border-rule bg-surface px-4 grid-snap lg:px-8">
      <Link href="/" className="self-center focus-mono cursor-pointer" aria-label="Peasant home">
        <span className="font-[family-name:var(--font-display)] text-xl font-semibold text-ink">
          peasant
        </span>
      </Link>

      <div className="flex min-w-0 items-center gap-2 sm:gap-3">
        {/* Search: the visible way into the command palette, word plus shortcut. */}
        <Button
          size="sm"
          icon={Search}
          onClick={() => window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT))}
          title="search & jump (⌘K)"
          aria-label="Open the command palette (Command or Control + K)"
        >
          search
          <kbd className="kbd normal-case">⌘K</kbd>
        </Button>

        {links.map((section) => (
          <Button
            key={section.id}
            as="a"
            href={section.href}
            size="sm"
            variant="ghost"
            icon={SECTION_ICONS[section.id]}
            title={section.title}
            aria-current={isSectionActive(section, pathname) ? "page" : undefined}
          >
            {section.label}
          </Button>
        ))}

        {/* Theme: small and icon-only; the label names the mode it switches to. */}
        <Button
          size="sm"
          variant="ghost"
          icon={theme === "light" ? Moon : Sun}
          onClick={toggle}
          aria-label={`Switch to ${nextTheme} mode`}
          title={`Switch to ${nextTheme} mode`}
        />
      </div>
    </header>
  );
}
