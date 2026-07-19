#!/usr/bin/env python3
"""Generate meta-refresh HTML stubs for legacy Hugo URLs.

GitHub Pages doesn't support server-side redirect files. Instead, we
plant static HTML at each old URL that both:
  1. Responds to bots with a canonical link to the new URL
  2. Redirects browsers via <meta http-equiv="refresh">

The stubs land in `docs/site-astro/public/docs/...` — Astro copies
public/ verbatim into dist/, so they end up at their old URL paths.

Usage:
  python3 scripts/generate-redirects.py
"""
from __future__ import annotations

import pathlib
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
# Post-cutover paths.
SRC_ROOT = REPO_ROOT / "docs/site-hugo-old/content/docs"
DST_ROOT = REPO_ROOT / "docs/site/public/docs"
BASE = "/core-agent"

CONCEPTS = {
    "permissions", "providers", "sessions", "mcp", "tools",
    "hooks", "skills", "context-management", "multi-session",
    "otel",
}


def map_to_new(hugo_rel: str) -> str:
    """Map a Hugo-relative content path to a Starlight URL (with base)."""
    p = hugo_rel.removesuffix(".md").removesuffix("/_index")
    if p == "_index":
        return f"{BASE}/"
    if p == "getting-started":
        return f"{BASE}/run/getting-started/"
    if p == "why-core-agent":
        return f"{BASE}/why-core-agent/"
    # Section roots (no trailing content) map to the section landing.
    if p == "reference":
        return f"{BASE}/reference/"
    if p == "cli":
        return f"{BASE}/run/"
    if p == "library":
        return f"{BASE}/embed/"
    if p == "agent-design":
        return f"{BASE}/agent-design/"
    if p == "skills-library":
        return f"{BASE}/run/skills-library/"
    if p == "examples":
        return f"{BASE}/examples/"
    if p.startswith("reference/"):
        rest = p[len("reference/"):]
        first = rest.split("/")[0]
        if first in CONCEPTS:
            return f"{BASE}/concepts/{rest}/"
        return f"{BASE}/reference/{rest}/"
    if p.startswith("cli/"):
        if p == "cli/autonomous/gke-team-scenario":
            return f"{BASE}/use-cases/k8s-triage/"
        return f"{BASE}/run/{p[len('cli/'):]}/"
    if p.startswith("library/"):
        return f"{BASE}/embed/{p[len('library/'):]}/"
    if p.startswith("agent-design/"):
        return f"{BASE}/agent-design/{p[len('agent-design/'):]}/"
    if p.startswith("skills-library/"):
        return f"{BASE}/run/skills-library/{p[len('skills-library/'):]}/"
    if p.startswith("examples/"):
        return f"{BASE}/examples/"
    return f"{BASE}/{p}/"


REDIRECT_HTML = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Moved</title>
<link rel="canonical" href="{new_url}">
<meta http-equiv="refresh" content="0; url={new_url}">
<meta name="robots" content="noindex">
<script>window.location.replace({new_url_json});</script>
</head>
<body>
<p>This page moved to <a href="{new_url}">{new_url}</a>.</p>
</body>
</html>
"""


def old_url_dir(hugo_rel: str) -> pathlib.Path:
    """Given `cli/autonomous/quickstart.md` return the public/ dir where the
    stub's index.html should live to be served at /docs/cli/autonomous/quickstart/."""
    p = hugo_rel.removesuffix(".md")
    if p.endswith("/_index"):
        p = p[: -len("/_index")]
    if p == "_index":
        return DST_ROOT   # /docs/ itself
    return DST_ROOT / p


def main() -> int:
    if DST_ROOT.exists():
        # Wipe any prior generation to keep it clean.
        import shutil
        shutil.rmtree(DST_ROOT)

    generated = 0
    for src in sorted(SRC_ROOT.rglob("*.md")):
        rel = str(src.relative_to(SRC_ROOT))
        new_url = map_to_new(rel)
        stub_dir = old_url_dir(rel)
        stub_dir.mkdir(parents=True, exist_ok=True)
        stub = stub_dir / "index.html"
        import json
        stub.write_text(REDIRECT_HTML.format(
            new_url=new_url,
            new_url_json=json.dumps(new_url),
        ))
        old_path = f"/docs/{rel.removesuffix('.md').removesuffix('/_index')}/".replace(
            "/_index/", "/"
        )
        print(f"  redirect  /docs/{rel}  →  {new_url}")
        generated += 1
    print(f"\ngenerated {generated} redirect stubs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
