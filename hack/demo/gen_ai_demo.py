#!/usr/bin/env python3
"""Generate docs/images/ai-demo.gif — the "ask an AI about your mesh" demo.

The transcript uses the real output wording of `dwxctl reach` / the
`mesh_reachability` MCP tool (see pkg/reach), so the demo is representative of
what the tools actually produce, not a mock-up. Regenerate with:

    python3 hack/demo/gen_ai_demo.py

Requires Pillow and a DejaVu Sans Mono font (both present on CI ubuntu runners).
"""
import os
from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
OUT = os.path.join(os.path.dirname(__file__), "..", "..", "docs", "images", "ai-demo.gif")

# GitHub-dark palette.
BG, HEADER, TEXT, DIM = "#0d1117", "#161b22", "#c9d1d9", "#8b949e"
GREEN, RED, YELLOW, BLUE = "#3fb950", "#f85149", "#d29922", "#58a6ff"

# (text, color). "" is a blank spacer line. The reach reasons match pkg/reach.
LINES = [
    ('you ▸ why can\'t cluster "west" reach this cluster?', BLUE),
    ("", TEXT),
    ("claude ▸ calling mesh_reachability …", DIM),
    ("", TEXT),
    ("  ✓  east    Reachable", GREEN),
    ("         policy permits east to every protected destination", DIM),
    ("  ✗  west    Blocked", RED),
    ("         every protected dest is default-deny; no rule permits west", DIM),
    ("  !  south   Unreachable", YELLOW),
    ("         peer not connected; the tunnel is not established", DIM),
    ("", TEXT),
    ("claude ▸ west is Blocked — its tunnel is up, but a MeshNetworkPolicy", TEXT),
    ("         makes 10.0.0.0/24 default-deny and no rule allows west.", TEXT),
    ("         fix: add west to the policy's ingress `from`.", GREEN),
    ("         — grounded in mesh_reachability, no guessing", DIM),
]

PAD, LH, HEADER_H = 24, 30, 44
FS = 19
W = 900
H = HEADER_H + PAD * 2 + LH * len(LINES)

font = ImageFont.truetype(FONT, FS)
header_font = ImageFont.truetype(FONT, 15)


def frame(n_visible, cursor=True):
    """Render a frame revealing the first n_visible body lines."""
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    # Title bar with traffic-light dots.
    d.rectangle([0, 0, W, HEADER_H], fill=HEADER)
    for i, c in enumerate(("#ff5f56", "#ffbd2e", "#27c93f")):
        d.ellipse([PAD + i * 22, HEADER_H // 2 - 6, PAD + i * 22 + 12, HEADER_H // 2 + 6], fill=c)
    d.text((PAD + 86, HEADER_H // 2 - 8), "dwx-mcp · datawerx-mesh   (read-only)", font=header_font, fill=DIM)

    y = HEADER_H + PAD
    for i in range(n_visible):
        text, color = LINES[i]
        d.text((PAD, y), text, font=font, fill=color)
        if cursor and i == n_visible - 1 and text:
            w = d.textlength(text, font=font)
            d.text((PAD + w + 2, y), "▌", font=font, fill=TEXT)
        y += LH
    return img


frames, durations = [], []
# Reveal one line per frame; skip a frame's pause on blank spacers.
for n in range(1, len(LINES) + 1):
    frames.append(frame(n))
    durations.append(120 if LINES[n - 1][0] == "" else 600)
# Hold the full transcript, then loop.
frames.append(frame(len(LINES), cursor=False))
durations.append(3200)

frames[0].save(
    os.path.normpath(OUT),
    save_all=True,
    append_images=frames[1:],
    duration=durations,
    loop=0,
    optimize=True,
)
print(f"wrote {os.path.normpath(OUT)}  ({len(frames)} frames, {W}x{H})")
