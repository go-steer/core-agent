# Docsy migration — lessons learned (handover note)

Context: migrating a Hugo site from Hextra (or any other theme) to
Docsy v0.15.0+. Bullets in rough order of likelihood-to-trip-you.

## Module setup (where I lost the most time)

- **Only import `github.com/google/docsy`.** There is NO
  `github.com/google/docsy/dependencies` module in v0.15.0+. That
  was an old pattern; Bootstrap + FontAwesome are now transitive
  Hugo modules pulled in by Docsy's own `go.mod`. Importing the
  non-existent dependencies module fails CI with "unknown revision
  dependencies/vX.Y.Z" — the path is wrong, not the version.
- **`google/docsy-example`'s `go.mod` is the source of truth.**
  Always cross-check there before guessing. As of May 2026 it has
  exactly one require: `github.com/google/docsy v0.15.0`.
- `go.mod` lives in the Hugo site root (e.g. `docs/site/go.mod`),
  not the repo root.

## Node deps (yes, even though Docsy ships SCSS as a Hugo module)

- Docsy STILL needs **PostCSS + autoprefixer** on PATH at Hugo
  build time. Hugo Modules ship the SCSS source; the resulting CSS
  is piped through PostCSS for autoprefixing.
- In CI: setup-node@v4 → `npm install` (in the Hugo site dir).
- Minimal `package.json` (devDependencies): `autoprefixer`,
  `postcss`, `postcss-cli`.
- `node_modules/` and `package-lock.json` go in `.gitignore`.

## Hugo version

- **Hugo Extended ≥ 0.146.0.** Docsy uses Hugo's SCSS pipeline
  (the "extended" variant). The non-extended build fails with
  cryptic SCSS errors.

## Content gotchas

- **Strip inline `# Title` H1s from every content page.** Docsy
  (like Hextra) renders frontmatter `title:` as the page heading.
  If you also have `# Title` in the body, the heading renders
  twice. Same fix as Hextra.
- **Don't carry over theme-specific shortcodes.** Hextra's
  `{{< cards >}}` / `{{< card >}}` don't render in Docsy. Hugo
  built-ins like `{{< relref >}}` work everywhere.
- For landing pages, use Docsy's: `{{< blocks/cover >}}`,
  `{{% blocks/lead %}}`, `{{% blocks/section %}}`,
  `{{% blocks/feature %}}`. Note `<` for raw content vs `%` for
  markdown-parsed content — getting this wrong silently fails.

## Menu config (subtle duplicate-entry trap)

- `hugo.yaml`'s `menu.main` and per-page frontmatter `menu.main`
  both populate the top nav. Defining the same entry in both
  produces duplicate menu items (e.g. "Documentation" appearing
  twice).
- Convention: per-page frontmatter for content pages (the page
  knows its own title), `hugo.yaml` for external links only.

## Site structure

- **Root `content/_index.md` is the marketing landing** (cover +
  lead + feature blocks).
- **`content/docs/_index.md` is the docs section landing** (the
  reference index — what's in the sidebar, where to start).
- These are different pages. Don't put your reference index on the
  root or your marketing pitch in the docs section.

## Light/dark mode toggle (off by default)

- Docsy has a three-state theme menu (Light / Dark / Auto) but
  **it's off by default**. Turn on with:
  ```yaml
  params:
    ui:
      showLightDarkModeMenu: true
  ```
- "Auto" honors the user's OS preference via
  `window.matchMedia('(prefers-color-scheme: dark)')`. Explicit
  picks persist in `localStorage` per origin. Implementation
  lives in `assets/js/dark-mode.js` inside the Docsy theme module.

## Inline code styling (the default Bootstrap color is loud)

- Bootstrap 5.3's default `--bs-code-color` is `#d63384` — a
  magenta-pink that's visually alarming against most page content
  and reads as "warning" rather than "this is an identifier."
  Every major dev-doc site (GitHub, Stripe, Vercel, Linear) uses
  a subtle background tint with body-text color instead.
- Override in `docs/site/assets/scss/_styles_project.scss` (Docsy
  auto-includes this file when present):

  ```scss
  :not(pre) > code {
    color: var(--bs-body-color);
    background-color: #eef1f5;  // slate-100
    padding: 0.15em 0.4em;
    border-radius: 6px;
    font-size: 87.5%;
  }
  [data-bs-theme="dark"] :not(pre) > code {
    background-color: #2d333b;  // slate-800-ish
  }
  ```

- **Use solid colors, not rgba transparency.** rgba values vary in
  visibility depending on what's behind the inline code — fine on
  a white doc page, invisible inside a card or hero with its own
  background. Solid colors render the same regardless of context.
- The `:not(pre) > code` selector scopes the rule to inline code;
  fenced code blocks (inside `<pre>`) keep their syntax-
  highlighted styling untouched.

## Investigating Docsy module versions

- GitHub's web /tags view only shows the most recent ~10. For
  submodule-prefix or older versions, use:
  `curl -sf https://api.github.com/repos/google/docsy/tags?per_page=100`.
- If a tag with a slash prefix doesn't appear (e.g.
  `dependencies/v...`), it almost certainly doesn't exist — Docsy
  doesn't use that pattern.
- When in doubt, fetch the canonical example's pinned versions:
  `curl -sf https://raw.githubusercontent.com/google/docsy-example/main/go.mod`.

## hugo.yaml param keys worth knowing

- `params.ui.breadcrumb_disable: false` — breadcrumbs on
- `params.ui.sidebar_menu_compact: false` — full sidebar (not
  compact)
- `params.ui.sidebar_menu_foldable: true` — collapsible sidebar
  sections
- `params.ui.sidebar_search_disable: false` — keeps offline Lunr
  search in the sidebar
- `params.ui.showLightDarkModeMenu: true` — theme toggle in
  navbar (off by default)
- `params.toc.enabled: true` — right-hand "On this page"
- `params.github_repo` + `github_branch` + `github_subdir` — for
  per-page "Edit this page" links
- `params.offlineSearch: true` — bundled Lunr index, no external
  service

## What didn't trip me up but worth flagging

- Docsy supports versioned docs natively (multiple doc trees side
  by side) — turn on with `params.versions:`. Useful for v0.X →
  v1.0 migrations later.
- Migration is mostly a one-shot — content pages with standard
  frontmatter (title, weight) and only Hugo built-in shortcodes
  carry over unchanged. The expensive bits are: theme module
  setup (one debugging round), the landing page rewrite (Hextra
  shortcodes don't carry), the CI workflow (Node + PostCSS), and
  the per-site-design tweaks for inline code + theme toggle.

## Order of operations that saved time

When migrating, do it in this order to keep each commit reviewable
and to surface CI failures one at a time:

1. **Theme + go.mod swap** — push, watch CI build the new theme.
   First failure here surfaces module-import bugs.
2. **Landing page rewrite** — push, eyeball the rendered result on
   pages.github.io.
3. **Theme toggle + inline-code styling + any other params**
   tweaks — push, iterate.

Bundling all three into one commit makes the CI failure surface
ambiguous: when the build breaks, you don't know whether it's the
theme, the shortcodes, or the params.
