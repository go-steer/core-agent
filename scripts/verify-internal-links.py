#!/usr/bin/env python3
"""Verify every root-relative internal link in the built site resolves.

Walks `docs/site/dist/**/*.html`, extracts every href/src whose URL
starts with the deploy base (default `/core-agent/`), and confirms the
target exists on disk. This is the offline equivalent of running the
site behind a real server and following every link.

Excluded:
  - External URLs (any scheme://)
  - Anchors (#foo)
  - mailto:/tel: etc.
  - URLs pointing outside the deploy base
  - Query strings (asset caching); the path portion is checked

Exits non-zero on any dead link.
"""
from __future__ import annotations

import re
import pathlib
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
DIST = REPO_ROOT / "docs/site/dist"
BASE = "/core-agent"

# href="..." or src="..." — captures the URL. Skips javascript:/mailto:
# by only accepting URLs starting with '/'.
URL_RE = re.compile(r'(?:href|src)="(/[^"#?]*)"')


def target_for(url_path: str) -> tuple[pathlib.Path, pathlib.Path]:
    """Return (dir-style, file-style) candidate paths under dist/.

    Astro emits routes as directories with index.html (`/foo/` →
    `foo/index.html`) but also serves assets directly (`/foo.css`).
    Try both.
    """
    rel = url_path[len(BASE):].lstrip("/").rstrip("/")
    if not rel:
        return DIST / "index.html", DIST / "index.html"
    return DIST / rel / "index.html", DIST / rel


def main() -> int:
    if not DIST.is_dir():
        print(f"error: {DIST} not found — run `npm run build` first",
              file=sys.stderr)
        return 2

    files = sorted(DIST.rglob("*.html"))
    if not files:
        print(f"error: no HTML files under {DIST}", file=sys.stderr)
        return 2

    # Aggregate broken links; a single missing target might be linked
    # from many pages, no point repeating it once per source page.
    broken: dict[str, set[pathlib.Path]] = {}
    checked = 0
    for f in files:
        for m in URL_RE.finditer(f.read_text()):
            url = m.group(1)
            if not url.startswith(BASE + "/") and url != BASE:
                continue
            checked += 1
            dir_target, file_target = target_for(url)
            if dir_target.is_file() or file_target.is_file():
                continue
            broken.setdefault(url, set()).add(f.relative_to(DIST))

    if broken:
        print(f"FAIL: {len(broken)} dead URL(s) referenced from {sum(len(v) for v in broken.values())} page(s):")
        for url in sorted(broken):
            sources = sorted(broken[url])
            print(f"  {url}")
            for src in sources[:5]:
                print(f"      referenced from: {src}")
            if len(sources) > 5:
                print(f"      ...and {len(sources) - 5} more")
        return 1

    print(f"OK: {checked} internal links across {len(files)} pages all resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
