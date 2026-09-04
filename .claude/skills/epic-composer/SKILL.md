---
name: epic-composer
description: Group follow-up items into vertical epics with prioritized sub-issues and a blocker DAG. Get sign-off on the full set. Write ASD-STE100 bodies with ASCII diagrams. Post to GitHub with sub-issues, blocked-by links, and existing labels. Mirror into Beads only if the project uses it. Use when deferred items or review findings must become an approved set of epics and issues.
---

# Compose, approve, and post epics with their sub-issues

Turn a loose set of follow-up items into vertical epics. Give each epic prioritized sub-issues
and a blocker DAG. Get approval for the full set before you create anything. Then create the
issues, create the epics, wire the relations, and apply labels. Mirror into Beads when the project
uses it.

Beads (`bd`) is optional. Use the Beads steps only when the project has a `.beads/` directory and
the user tracks work there. Without Beads, GitHub is the only record. Collect inputs from issues,
PR review threads, and notes. Skip every `bd` command. Keep the rest unchanged.

## Preconditions

- `gh auth status` succeeds. You can reach each target repository with `-R <owner>/<repo>`.
- The user has named the batch of items. Examples: a deferred list, a follow-up ledger, review
  findings.
- You know the primary repository for each epic.

Stop and ask if authentication, repository access, or the required GitHub permissions are not
available.

## Workflow

1. **Inspect**
   - Read each repository's `AGENTS.md`. Read the code the items touch.
   - Collect every candidate item before you group. Sources: user-deferred items, review findings
     routed to follow-ups, observations recorded during verification.
   - For each item record four facts: the source (issue number, PR thread, or Beads id), the
     repo, one line of substance, and whether a GitHub issue exists.
     ```sh
     bd list --json --limit 5000 | jq -r '.[] | select(.status!="closed") | "\(.id) | \(.title)"'
     bd show <id> | grep -oE 'https://github.com/[^ )]+/issues/[0-9]+'   # empty = no issue yet
     ```
   - Search for existing epics and duplicates in every repo. Search Beads too, if used.
     ```sh
     gh issue list -R <owner>/<repo> --label epic --state open --limit 50 --json number,title
     gh issue list -R <owner>/<repo> --state open --limit 100 --search "<key terms> in:title,body"
     bd list --json --limit 5000 | jq -r '.[] | select(.issue_type=="epic" and .status!="closed") | "\(.id) | P\(.priority) | \(.title)"'
     ```
   - Read the bodies of the adjacent epics. Decide per item: a new epic, or re-home the item into
     an existing epic. Do not create an epic that already exists. Do not duplicate an open issue.

2. **Draft the structure**
   - Group vertically. An epic is one concern a user or operator can name. Examples: "origin
     coverage gaps", "test-infrastructure hardening". Group by the concern the items change. Do
     not group by repo. Do not group by who found the item. A cleanup-only group is allowed at
     the lowest priority.
   - Prioritize the epic and every item. Scale: P0 blocks users now. P1 is next. P2 is planned.
     P3 is when convenient. The epic's priority equals its highest item. Inside an epic, the first
     one or two items matter. The rest are improvements. Give one sentence of reason for each
     call that is not obvious.
   - Order into a DAG. For every pair of items ask: must one finish before the other is correct
     or cheap? Typical arrows: a decision before the work it scopes. A shared helper before its
     callers. A producer-side change before a consumer re-run. A data-integrity fix before a count
     that depends on it. Items with no arrow are independent. Say so.
   - Titles: epics `[P1][Epic] <title>`. Issues `[P2] <title>`.

3. **Refine and approve the structure**
   - Present one context message in ASD-STE100 with an ASCII diagram. Include: the epics, every
     sub-issue with its priority, the DAG with arrows, the re-homing table (existing epic <-
     items), and which items become new issues and which are retitled. End the turn.
   - In the next turn, ask for corrections or explicit approval with AskUserQuestion.
   - Apply the refinements. Present again when a material detail changes. Examples: a new
     grouping, a moved item, a changed priority, a new arrow.
   - Do not write bodies for posting before the structure is approved.

4. **Draft the bodies**
   - Language rules (hard):
     - ASD-STE100. Short sentences. Active voice. One idea per sentence. Imperative steps. Plain
       words. No em-dashes.
     - Use industry-standard names: fail-safe default, schema migration, backfill, closed enum,
       CHECK constraint, trust boundary, discovery scope vs access control, fixture, golden test,
       mutation testing, build tag, REST endpoint, WebSocket topic, OpenAPI contract, evidence,
       provenance.
     - Reuse the codebase's concept vocabulary. The identifiers do not have to match. Grep before
       you name a concept. Keep one shared glossary across all bodies, so the set reads as one
       system.
     - Write cross-repo references as `owner/repo#N`. Example: `acme/schema#82`. GitHub links
       only that form. A bare `schema#82` never links. Same-repo references stay `#N`.
     - Do not put internal task taxonomy in bodies. No Beads ids. No slice, wave, or proposal
       names. No reviewer axis names. Refer to work by substance and by issue numbers.
   - Epic body sections: Why. Concept map (an ASCII diagram of the API surfaces, the
     infrastructure pieces, and the child issues placed on that map). Children table (priority,
     title, blocked by). Definition of done. Related epics.
   - Issue body sections: Problem (grounded with file paths and measured numbers). Scope (what
     changes and what does not). Acceptance (observable outcomes, fixtures named). Diagram.
     Blocked by / Blocks. Related.
   - The issue diagram is narrow. It shows that issue's own change: the exact data flow, the
     before and after, or the failure path. It must be more specific than the epic diagram. It is
     never a rehash.
   - Bodies written before the new issues exist use `NEW-<key>` for their numbers.
   - Delegate the writing to background agents. Give each agent an explicit cheap model. Do not
     use the session's top model unless the user names it. Use one shared brief file, one shared
     glossary file, and one draft file per body.
   - Review every draft. Run the language rules as a checklist. Grep for internal taxonomy, bare
     cross-repo references, and em-dashes. Read at least one epic, one new issue, and one
     addendum in full.

5. **Refine and approve the bodies**
   - Present the drafts to the user. Give the paths. Show the epic bodies inline when they are
     short.
   - Ask for corrections or explicit approval. Apply the refinements. Present again when a
     material detail changes.
   - Do not create anything before the user approves the current drafts. Keep the approved
     scope. If you find an ambiguity after approval, refine and get approval again before you
     change a body.

6. **Create**
   - Order: new issues first. Then substitute placeholders. Then existing-issue addenda. Then
     epics. Then relations. Then labels.
   - Markdown through a shell (hard rule): never pass a body inside a double-quoted argument.
     The shell expands or eats backticks, `$`, `!`, and glob characters. The posted body then
     loses its code spans. Use one of two safe forms.
     1. A file, written with a quoted heredoc, passed with `--body-file`. Best for long bodies.
     2. A single-quoted argument. Plain `'...'` disables all expansion. The only character you
        cannot write inside it is `'` itself. ANSI-C quoting `$'...'` also works. It lets you
        write `\'` for a quote and `\n` for a newline. It interprets every other backslash
        sequence, so keep diagrams out of it.
     ```sh
     # form 1: a file for anything long or containing a single quote
     cat > body.md <<'BODY'
     ## Problem
     The `session_origin` column ...
     BODY
     gh issue create -R <o>/<r> --title '[P2] <title>' --body-file body.md
     # form 2: single quotes, no expansion at all
     gh issue comment <n> -R <o>/<r> --body 'Run `make check`. The `$HOME` path is not expanded.'
     ```
   - Placeholders. After you create the new issues, substitute every `NEW-<key>` in every draft.
     Same repo becomes `#N`. Other repo becomes `owner/repo#N`. Re-post the new issues whose
     bodies held placeholders. Grep all drafts for `NEW-` before you post the epics.
   - Existing issue: retitle it and append the addendum. Build a merged file. Never splice inline.
     ```sh
     { gh issue view <n> -R <o>/<r> --json body --jq .body; printf '\n\n'; cat addendum.md; } > merged.md
     gh issue edit <n> -R <o>/<r> --title '[P3] <title>' --body-file merged.md
     ```
   - Epic: `gh issue create -R <o>/<r> --title '[P2][Epic] <title>' --body-file epic.md --label epic`.
   - Relations. Sub-issues and blocked-by both work across repositories within one owner.
     Verified 2026-08-26: `peasant-labs/schema#82` is a sub-issue of a peasant epic, and
     `peasant-labs/village#48` is blocked by `peasant-labs/peasant#193`. State each cross-repo
     relation in the body too, so it reads without the sidebar.
     ```sh
     CHILD_ID=$(gh api repos/<o>/<r>/issues/<child> --jq .id)
     gh api -X POST repos/<o>/<epic-repo>/issues/<epic>/sub_issues -F sub_issue_id=$CHILD_ID
     EARLIER_ID=$(gh api repos/<o>/<r>/issues/<earlier> --jq .id)
     gh api -X POST repos/<o>/<r>/issues/<later>/dependencies/blocked_by -F issue_id=$EARLIER_ID
     ```
   - Read every posted body back with `gh issue view <n> --json body --jq .body`. Check that
     every code span and diagram survived. A missing backtick or a collapsed `$var` is a posting
     defect. Repost from the file.

7. **Label**
   - List each repo's labels first: `gh label list -R <o>/<r> --json name,description`.
   - Give every sub-issue one or two labels from the existing inventory. Choose by substance.
     `bug`: shipped behaviour is wrong. `enhancement`: new or better behaviour. `documentation`:
     comments or docs only. `question`: a decision to make. A component label (example:
     `tui-kit`): the repo has one and the change touches that component. `good first issue`: only
     for small, self-contained changes. Epics carry `epic`.
   - Apply with `gh issue edit <n> -R <o>/<r> --add-label '<label>'`. The writer of a body labels
     it. The orchestrator checks the mapping against the inventory.
   - If a needed label does not exist, raise it to the user. Give the proposed name and
     description. Wait for the answer. Do the same if you only consider creating a label. Do not
     create a label on your own.

8. **Mirror in Beads (only if the project uses Beads)**
   - Create one Beads epic per GitHub epic. Use the same title and the same priority.
   - Chain `bd dep add <epic> --blocked-by <item>` for every child. Chain
     `bd dep add <later> --blocked-by <earlier>` for every arrow. Set each item's priority.
   - Record each issue URL on its Beads task with `bd comments add`.
   - Leave the user-deferred ledger epic open and untouched.

9. **Report**
   - Send one message. Include: the epics with numbers and priorities, the DAG with numbers, the
     re-homings, the label mapping, and the count of issues created versus retitled. Then stop.

## Hard rules

1. Do not create an epic or an issue before the user approves the current structure (step 3) and
   the current bodies (step 5).
2. Do not create an epic that already exists. Do not duplicate an open issue. Re-home instead.
3. Keep the approved scope. If you find an ambiguity after approval, refine and get approval
   again before you change anything.
4. Every epic and every item carries a priority in its title. Every arrow in the DAG becomes a
   native blocked-by relation. State the arrow in the body too.
5. Pass bodies through a file or a single-quoted argument. Never use a double-quoted argument.
   Read every posted body back.
6. Write cross-repo references as `owner/repo#N`. Put no internal task taxonomy in any body.
7. Take labels from the existing inventory. A new label is a question for the user. Never create
   one on your own.
8. Beads is optional. When used, the mirror carries the same priorities and arrows as GitHub.
