# daily-gen

Generates weekly work-log markdown (`YYYYMMDD-YYYYMMDD.md`, Mon–Sun) from git
history across configured projects, filtered to your author identities.

## Usage

```sh
cd generator
go run .              # or: ./run.sh
go run . path/to/other-config.json
go run . -n           # dry run: report what would change without writing
go run . -all         # refetch and re-render every week since firstWeekStart
```

Output lands in `outputDir` (default `..`, i.e. the `daily/` folder), plus an
auto-generated `INDEX.md` summary table. Projects are collected in parallel.
Files created by older versions used a Mon–Fri name even when they contained
weekend activity; affected files are migrated to the accurate Mon–Sun name the
next time that week is rendered.

By default only **recent weeks** are fetched and re-rendered: from the previous
week's Monday, extended back to the newest week file on disk (so gaps from
skipped runs are backfilled). Older files are left untouched; use `-all` to
rebuild everything. A week's file is only rewritten when its derived content
changed (it reports `N written, M unchanged`).

## Config (`config.json`)

Copy `config.example.json` to `config.json` and fill in your values
(`config.json` is gitignored).

| key                 | meaning                                                        |
| ------------------- | -------------------------------------------------------------- |
| `authors`           | git author emails counted as "you" (OR-matched)               |
| `githubAuthor`      | `gh --author` value, default `@me`                             |
| `firstWeekStart`    | `YYYYMMDD` Monday that anchors "Week 1"                        |
| `outputDir`         | where `.md` files are written (relative to this config)        |
| `preferredBranches` | branch-guess tie-break order, default `["qa"]`                 |
| `projects`          | `[{ "name": "<heading>", "path": "<repo, relative to config>" }]` |

Add a project by appending to `projects` — paths are resolved relative to
`config.json`.

## Manual notes are preserved

Each `(day, project)` cell's commit list lives inside a marker block:

```md
### ULC

<!-- daily-auto:start ULC 20260601 -->
(on qa branch)

PRs:

- #46 opened & merged: feat: appointment creation dialog (qa <- feat/create-app-dialog) (+812 -120)

commits:

- #152ebc8 fix: debounce only on search input (+12 -8)
<!-- daily-auto:end ULC 20260601 -->

write whatever you want here, it survives re-runs
```

Anything **outside** the marker block (prose, edits to the `(on … branch)`
guess, extra notes) is kept verbatim. Only the lines between the markers are
regenerated. The `(… is my first week)` intro appears only in the Week 1 file,
and is yours to edit once the file exists.

## How the auto block is derived

- **Commits**: non-merge commits by you on that day, across all branches/remotes
  (stash excluded), grouped by author date. Each links to the commit on GitHub
  and shows its diff size `(+A -D)` from `git log --numstat` (binary files
  count as 0). PR numbers link to GitHub too.
- **Week stats**: a `stats:` line under each week title (commits, +/- lines,
  PRs opened/merged), also collected into `INDEX.md`.
- **Day activity span**: a `(commit activity HH:MM - HH:MM)` line under each day
  header — first to last commit author time that day, handy for timesheets.
- **PRs**: your PRs via `gh pr list --author <githubAuthor>` (server-side
  filtered to `updated:>=` the first week; it warns if the 300-PR page fills
  up), anchored to the day they were opened/merged/closed, with `(base <- head)`
  and total `(+A -D)`. This catches squash/rebase-merged PRs that leave no
  merge commit.
- **`(on <branch> branch)`**: best-guess — the branch containing the most of
  that cell's commits (one `git log` per ref, mapped hash -> branches); ties
  prefer `preferredBranches`. It's a guess; edit it freely, your edit is
  outside the markers so it sticks.
