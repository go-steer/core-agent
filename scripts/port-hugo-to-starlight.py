#!/usr/bin/env python3
"""Port Hugo/Docsy Markdown pages to Astro Starlight.

Mechanical transforms only:
  1. Rewrite `{{< relref "/docs/foo.md" >}}` → new-IA URL path
     (no /core-agent/ prefix — remark plugin adds base at build time)
  2. Strip Hugo-only frontmatter fields (weight, linkTitle)
  3. Rewrite section index files (_index.md → index.md)
  4. Route content to the new audience-first IA per the plan

Usage:
  python3 scripts/port-hugo-to-starlight.py

Idempotent: safe to re-run when source changes.
"""
from __future__ import annotations

import re
import pathlib
import shutil
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
# Post-cutover paths. Pre-cutover the source was `docs/site/content/docs`
# and the destination was `docs/site-astro/src/content/docs`.
SRC_ROOT = REPO_ROOT / "docs/site-hugo-old/content/docs"
DST_ROOT = REPO_ROOT / "docs/site/src/content/docs"

# URL migration table (from the plan) — Hugo path prefix → Starlight prefix.
# Applied in order; longest-prefix wins.
CONCEPTS = {
    "permissions", "providers", "sessions", "mcp", "tools",
    "hooks", "skills", "context-management", "multi-session",
    "otel",
}


def rewrite_hugo_path(p: str) -> str:
    """Map a Hugo docs-relative path (no leading slash, no .md) to a Starlight URL."""
    if p.startswith("reference/"):
        rest = p[len("reference/"):]
        first = rest.split("/")[0]
        if first in CONCEPTS:
            return f"/concepts/{rest}/"
        return f"/reference/{rest}/"
    if p.startswith("cli/"):
        return f"/run/{p[len('cli/'):]}/"
    if p.startswith("library/"):
        rest = p[len("library/"):]
        return "/embed/" if rest in ("", "_index") else f"/embed/{rest}/"
    if p.startswith("agent-design/"):
        rest = p[len("agent-design/"):]
        return "/agent-design/" if rest == "_index" else f"/agent-design/{rest}/"
    if p.startswith("skills-library/"):
        rest = p[len("skills-library/"):]
        return "/run/skills-library/" if rest == "_index" else f"/run/skills-library/{rest}/"
    if p.startswith("examples/"):
        rest = p[len("examples/"):]
        return "/examples/" if rest == "_index" else f"/examples/{rest}/"
    if p == "getting-started":
        # Ambiguous: audience-split. Default to CLI-audience;
        # authors of embed content should hand-edit as needed.
        return "/run/getting-started/"
    if p == "why-core-agent":
        return "/why-core-agent/"
    if p == "_index":
        return "/"
    return f"/{p}/"


def rewrite_relref(m: re.Match) -> str:
    path = m.group(1).removeprefix("/docs/")
    anchor = ""
    if "#" in path:
        path, anchor = path.split("#", 1)
        anchor = "#" + anchor
    p = path.removesuffix(".md")
    url = rewrite_hugo_path(p)
    # Collapse trailing _index (Hugo section index → Starlight section root).
    if url.endswith("/_index/"):
        url = url[: -len("_index/")]
    elif url.endswith("/_index"):
        url = url[: -len("_index")]
    # Normalize trailing slash exactly once (root path stays "/").
    if url != "/" and not url.endswith("/"):
        url += "/"
    return url + anchor


RELREF_RE = re.compile(r'\{\{<\s*relref\s+"([^"]+)"\s*>\}\}')


def transform_body(src: str) -> str:
    return RELREF_RE.sub(rewrite_relref, src)


def clean_frontmatter(src: str) -> str:
    """Strip Docsy-only fields (weight, linkTitle) from the frontmatter block."""
    lines = src.splitlines(keepends=True)
    out, in_fm, fm_seen = [], False, 0
    for ln in lines:
        if ln.strip() == "---":
            fm_seen += 1
            in_fm = fm_seen == 1
            out.append(ln)
            continue
        if in_fm and (ln.startswith("weight:") or ln.startswith("linkTitle:")):
            continue
        out.append(ln)
    return "".join(out)


def dst_path_for(src_rel: str) -> pathlib.Path | None:
    """Map a source path (relative to docs/site/content/docs/) to Astro content path.
    Returns None for files handled specially (root landing, split getting-started).
    """
    # _index.md → index.md within its directory
    if src_rel.endswith("_index.md"):
        dir_part = src_rel[: -len("_index.md")]
        p = pathlib.Path(dir_part) / "index.md"
    else:
        p = pathlib.Path(src_rel)

    parts = p.parts
    # Route by top-level dir per the URL migration table
    if not parts:
        return None
    head = parts[0]
    tail = parts[1:]

    # Special: root docs section landing → src/content/docs/index.mdx
    # We already have a hand-crafted splash there; skip the auto-port.
    if head == "index.md" and not tail:
        return None
    # Special: top-level getting-started is audience-split; hand-ported.
    if head == "getting-started.md" and not tail:
        return None
    # Special: why-core-agent is straight port (no reroute)
    if head == "why-core-agent.md" and not tail:
        return DST_ROOT / "why-core-agent.md"

    if head == "cli":
        return DST_ROOT.joinpath("run", *tail)
    if head == "library":
        return DST_ROOT.joinpath("embed", *tail)
    if head == "skills-library":
        return DST_ROOT.joinpath("run", "skills-library", *tail)
    if head == "reference":
        # Split: concept-topics move to concepts/, rest stays in reference/
        if len(tail) >= 1:
            name = tail[0].removesuffix(".md")
            if name == "index":
                # reference/_index.md → new reference/index.md (curated,
                # not from the Hugo source since we're splitting the section).
                # Skip auto-port; will be hand-crafted.
                return None
            if name in CONCEPTS:
                return DST_ROOT.joinpath("concepts", *tail)
            return DST_ROOT.joinpath("reference", *tail)
        return None
    if head in ("agent-design", "examples"):
        return DST_ROOT.joinpath(head, *tail)
    return DST_ROOT.joinpath(*parts)


def port_file(src: pathlib.Path, dst: pathlib.Path) -> None:
    content = src.read_text()
    content = clean_frontmatter(content)
    content = transform_body(content)
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_text(content)


def main() -> int:
    ported, skipped = 0, 0
    for src in sorted(SRC_ROOT.rglob("*.md")):
        rel = src.relative_to(SRC_ROOT)
        dst = dst_path_for(str(rel))
        if dst is None:
            print(f"  SKIP  {rel}")
            skipped += 1
            continue
        port_file(src, dst)
        print(f"  port  {rel}  →  {dst.relative_to(REPO_ROOT)}")
        ported += 1
    print(f"\nported {ported}, skipped {skipped}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
