'use client';

import { File, GitBranch, Lock, User, type LucideIcon } from 'lucide-react';
import { AllRedactionCategories, redactionCategoryLabel, type RedactionCategory } from '@peasant-labs/schema';
import { REDACTION_SCOPE_SENTENCE } from '@/lib/share/redaction-policy.generated';

/**
 * What each category covers, in words. The categories and the label each one
 * prints come from the engine (through the schema package); this is only the
 * copy beside them. It restates what the rules of the offered level match, so
 * a rule that moves level moves this copy with it.
 */
const CATEGORY_COPY: Readonly<Record<RedactionCategory, { icon: LucideIcon; covers: string }>> = {
  secrets: { icon: Lock, covers: 'api keys, tokens, passwords and other credentials.' },
  pii: { icon: User, covers: 'emails, phone numbers, card numbers and ip addresses.' },
  paths: { icon: File, covers: 'file paths that show your user name.' },
  project: { icon: GitBranch, covers: 'import paths, image refs and ci project names.' },
};

/** The redaction section's lead: the categories hidden before anything leaves this computer. */
export function RedactionCategories() {
  return (
    <section className="stg-cats" aria-labelledby="stg-cats-title">
      <h2 id="stg-cats-title" className="stg-cats-title">hidden before anything leaves</h2>
      <ul className="stg-cats-list">
        {AllRedactionCategories.map((category) => {
          const { icon: Icon, covers } = CATEGORY_COPY[category];
          return (
            <li key={category} className="stg-cat" data-category={category}>
              <span className="stg-cat-icon" aria-hidden="true"><Icon /></span>
              <span className="stg-cat-name">{category}</span>
              <span className="stg-cat-covers">{covers}</span>
              <span className="stg-cat-label">shows as <code>{redactionCategoryLabel(category)}</code></span>
            </li>
          );
        })}
      </ul>
      <p className="stg-cats-scope">{REDACTION_SCOPE_SENTENCE}</p>
    </section>
  );
}
