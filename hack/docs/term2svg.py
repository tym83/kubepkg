#!/usr/bin/env python3
"""Draws terminal screenshots (SVG) from the transcripts capture.sh writes.

    term2svg.py <transcript.txt> <out.svg> [title]

A transcript is "$ command" lines followed by output. Long lines are cut at
MAX_COLS, so a screenshot stays readable on a page; the text blocks on the
page carry the full output.
"""
import sys
from xml.sax.saxutils import escape

MAX_COLS = 110
FONT = 13
LINE = 18
CHAR = 7.83  # advance of a 13px monospace glyph
PAD = 16
BAR = 30

BG = "#1e1f29"
BAR_BG = "#2a2c3a"
FG = "#d6d8e0"
DIM = "#8b8fa3"
PROMPT = "#7ee787"
CMD = "#ffffff"


def main():
    src, out = sys.argv[1], sys.argv[2]
    title = sys.argv[3] if len(sys.argv) > 3 else "kubepkg"
    with open(src, encoding="utf-8") as f:
        lines = f.read().rstrip("\n").split("\n")
    lines = [l.expandtabs(8) for l in lines]
    cols = min(MAX_COLS, max(len(l) for l in lines))
    width = int(PAD * 2 + cols * CHAR)
    height = BAR + PAD * 2 + LINE * len(lines)

    body = []
    y = BAR + PAD + FONT
    for l in lines:
        if len(l) > cols:
            l = l[: cols - 1] + "…"
        if l.startswith("$ "):
            body.append(
                f'<text x="{PAD}" y="{y}"><tspan fill="{PROMPT}">$</tspan>'
                f'<tspan fill="{CMD}" font-weight="bold"> {escape(l[2:])}</tspan></text>'
            )
        else:
            body.append(f'<text x="{PAD}" y="{y}" fill="{FG}">{escape(l)}</text>')
        y += LINE

    dots = "".join(
        f'<circle cx="{18 + i * 20}" cy="{BAR // 2}" r="6" fill="{c}"/>'
        for i, c in enumerate(["#ff5f57", "#febc2e", "#28c840"])
    )
    svg = f"""<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}" role="img" aria-label="{escape(title)}">
<rect width="{width}" height="{height}" rx="8" fill="{BG}"/>
<path d="M0 8a8 8 0 0 1 8-8h{width - 16}a8 8 0 0 1 8 8v{BAR - 8}h-{width}z" fill="{BAR_BG}"/>
{dots}
<text x="{width / 2}" y="{BAR // 2 + 4}" fill="{DIM}" font-family="-apple-system, Segoe UI, sans-serif" font-size="12" text-anchor="middle">{escape(title)}</text>
<g font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, monospace" font-size="{FONT}" xml:space="preserve">
{chr(10).join(body)}
</g>
</svg>
"""
    with open(out, "w", encoding="utf-8") as f:
        f.write(svg)


if __name__ == "__main__":
    main()
