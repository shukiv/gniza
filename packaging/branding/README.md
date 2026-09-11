# Brand assets

The mark is Gniza's own: three stacked shelves with a spout under the last
one — what is put away, and the one that gives it back. The name in prose is
**Gniza**; `gniza` is also the name of the binaries, the paths and the Go
module.

| File | What it is |
|---|---|
| `badge.svg` | the badge: the white mark on the orange rounded square, 300×300 |
| `mark.svg` | the mark alone, brand orange, transparent, trimmed to the ink |
| `mark-white.svg` | the same shape in white, for a dark or coloured ground |
| `png/badge-{16,32,48,64,128,256,512}.png` | the badge, rasterised |
| `png/mark-{256,512}.png` | the mark, rasterised, transparent |
| `gniza-logo.svg` | the wordmark: mark, name and strapline |

`badge.svg` is the plugin's icon: WHM draws it in the sidebar from
`addon_plugins/gniza.svg`, and cPanel draws the account tile from the 48px
PNG, because cPanel's SVG installer rewrites geometry attributes in custom
artwork.

The same outline is set inline in the WHM plugin's sidebar, in
`internal/webui/templates/layout.html`. There it is filled with
`currentColor` so the badge (`data-brand-mark`) decides both it and the mark on it
with one rule.

## Colours

They are [gniza.app](https://gniza.app)'s, so the plugin and the site are one
product.

| | Hex | Where |
|---|---|---|
| Brand orange | `#F47216` | the mark, and the badge ground |
| Accent | `#EE7A00` | the second word of the wordmark |
| Ink | `#0F172A` | the name |
| Strapline | `#5B6675` | *Backup. Restore. Repeat.* under the name |

## Why the mark is a path and the words are not

The mark is an outline, so it is the same shape on a machine that has none
of our fonts, and a PNG made here matches the SVG a browser draws.

`gniza-logo.svg` sets the words as `<text>` and pins them with `textLength`,
which is what you do when artwork has to hold its shape without the font it
was drawn in. It holds; it is not identical — the fallback face still decides
the letterforms.

## Rebuilding the PNGs

```bash
python3 packaging/branding/build.py     # needs rsvg-convert
```

The SVGs are drawn by hand and the PNGs are committed; the script only
rasterises. Change the artwork in the SVG rather than editing the PNGs.
