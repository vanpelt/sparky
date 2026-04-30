#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "playwright>=1.40",
#   "img2pdf>=0.5",
# ]
# ///
"""
Export a deck-stage HTML deck to a high-fidelity PDF.

Renders each slide in screen mode (so radial-gradient glows, box-shadow blurs,
mask-image, and other effects render correctly — unlike Chrome's --print-to-pdf
path, which flattens those into solid blocks). Stitches the per-slide PNGs into
a single PDF with img2pdf.

Usage:
  uv run prezos/export.py prezos/hivemind-overview
  uv run prezos/export.py prezos/hivemind-overview -o ~/Desktop/deck.pdf

The deck must contain an index.html using <deck-stage>.
Output defaults to <deck>/<deck-name>.pdf next to the source.

First run downloads playwright (~20MB Python pkg). Browser is system Chrome —
no extra browser bundle to install.
"""
import argparse
import sys
import tempfile
from pathlib import Path

import img2pdf
from playwright.sync_api import sync_playwright

DESIGN_W, DESIGN_H = 1920, 1080
SCALE = 2  # device pixel ratio — 2× = retina-quality screenshots


def export_deck(deck_dir: Path, output_path: Path) -> None:
    deck_html = deck_dir / "index.html"
    if not deck_html.exists():
        sys.exit(f"deck not found: {deck_html}")

    with sync_playwright() as p:
        # Use system Chrome — no separate Chromium download needed.
        browser = p.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(
            viewport={"width": DESIGN_W, "height": DESIGN_H},
            device_scale_factor=SCALE,
        )
        page = context.new_page()
        page.goto(f"file://{deck_html.resolve()}", wait_until="load")

        # Fonts must be ready before screenshots, otherwise wordmarks fall back.
        page.wait_for_function("document.fonts.ready.then(() => true)")

        # All <img> elements must finish loading (brain.png, screenshots, etc.).
        page.wait_for_function(
            "Array.from(document.images).every(i => i.complete && i.naturalHeight > 0)"
        )

        # deck-stage must have wired up its slide list.
        page.wait_for_function(
            "document.querySelector('deck-stage') "
            "&& document.querySelector('deck-stage').length > 0"
        )

        # Kill animations and transitions so each screenshot shows the final,
        # deterministic state of the slide instead of a half-animated frame.
        page.add_style_tag(
            content="""
            *, *::before, *::after {
                animation-duration: 0s !important;
                animation-delay: 0s !important;
                animation-iteration-count: 1 !important;
                transition-duration: 0s !important;
                transition-delay: 0s !important;
            }
        """
        )

        # Hide the deck-stage UI overlay (page counter + reset button + tapzones).
        # It lives in the component's shadow DOM, so we have to reach in.
        page.evaluate(
            """
            const ds = document.querySelector('deck-stage');
            if (ds && ds.shadowRoot) {
                const sheet = new CSSStyleSheet();
                sheet.replaceSync('.overlay, .tapzones { display: none !important; }');
                ds.shadowRoot.adoptedStyleSheets = [
                    ...ds.shadowRoot.adoptedStyleSheets, sheet
                ];
            }
        """
        )

        total = page.evaluate("document.querySelector('deck-stage').length")
        deck_name = deck_dir.name
        print(f"exporting {total} slides from '{deck_name}' at "
              f"{DESIGN_W}×{DESIGN_H} (×{SCALE})...")

        with tempfile.TemporaryDirectory(prefix="deck-export-") as tmpdir:
            screenshots: list[str] = []
            for i in range(total):
                page.evaluate(f"document.querySelector('deck-stage').goTo({i})")
                page.wait_for_function(
                    f"document.querySelectorAll('deck-stage > section')[{i}]"
                    f".hasAttribute('data-deck-active')"
                )
                page.wait_for_timeout(150)  # let layout settle after activation

                shot = Path(tmpdir) / f"slide-{i:02d}.png"
                page.screenshot(path=str(shot), full_page=False)
                screenshots.append(str(shot))
                print(f"  [{i + 1:>2}/{total}] {shot.name}")

            browser.close()

            # Page size: 1920×1080 CSS px → 1440×810 PDF points (96→72 dpi).
            page_w_pt = img2pdf.in_to_pt(DESIGN_W / 96)
            page_h_pt = img2pdf.in_to_pt(DESIGN_H / 96)
            with open(output_path, "wb") as f:
                f.write(
                    img2pdf.convert(
                        screenshots,
                        pagesize=(page_w_pt, page_h_pt),
                    )
                )

        size_mb = output_path.stat().st_size / 1024 / 1024
        print(f"\n{output_path} ({size_mb:.1f} MB, {total} pages)")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Export a deck-stage HTML deck to a high-fidelity PDF."
    )
    parser.add_argument(
        "deck",
        help="path to the deck directory (containing index.html)",
    )
    parser.add_argument(
        "-o",
        "--output",
        help="output PDF path (default: <deck>/<deck-name>.pdf)",
    )
    args = parser.parse_args()

    deck_dir = Path(args.deck).resolve()
    if not deck_dir.is_dir():
        sys.exit(f"deck dir not found: {deck_dir}")

    output_path = (
        Path(args.output).resolve()
        if args.output
        else deck_dir / f"{deck_dir.name}.pdf"
    )
    output_path.parent.mkdir(parents=True, exist_ok=True)
    export_deck(deck_dir, output_path)


if __name__ == "__main__":
    main()
