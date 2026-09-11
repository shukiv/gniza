# 0023 — daisyUI for the plugin pages

Status: accepted
Date: 2026-09-11

## Context

The plugin's pages were styled by a hand-written stylesheet of some 1,100
lines: its own tokens, its own buttons, pills, tables, sheets and a rail,
every rule scoped under `.gniza` and every class prefixed `cpr-` (ADR 0009).
It worked, and it looked like nothing else. The operator does not like it,
and asked for daisyUI.

Two things make that harder here than on a page of our own. The page is a
fragment inside WHM's, DirectAdmin's or cPanel's own document, whose
stylesheets style bare `input`, `table`, `h1` and `a:link` and own short
class names like `.btn` and `.alert` — daisyUI's class names. And the
stylesheet is inlined into every page (ADR 0008), so whatever the build
produces is what every server carries.

## Decision

The stylesheet is compiled from `internal/webui/ui/input.css` with Tailwind
4 and daisyUI 5 (`make css`) and committed as `static/app.css`; the Go
build, the release and every server never need Node. CI fails on a stale
copy (`make css-check`).

Tailwind runs with `prefix(cpr)`, so every utility and every daisyUI
component is rendered as `cpr:btn`, `cpr:table`, `cpr:md:grid-cols-2`. ADR
0009's invariant stands with the prefix `cpr:` instead of `cpr-`; the test
that enforces it reads the compiled stylesheet through CSS escapes. daisyUI's
own `prefix` option was tried and rejected: with both prefixes set it emits
no components, and with only its own the utilities go unprefixed.

Everything ours lives in cascade layers — `reset, theme, base, gniza,
components, utilities` — and two rules isolate it from the host:

- The lowest layer, `reset`, sets `all: revert` on the wrapper and every
  element under it. A host that styles elements in its own layer (the
  DirectAdmin skin does, in `@layer elements`) is rolled back to the
  browser's defaults before our layers apply.
- An unlayered rule at the end, `.gniza :where(*):not(#g#g) { all:
  revert-layer }`, outranks any unlayered host rule short of two ids and
  rolls it back to our layers. Unlayered author styles beat layered ones
  whatever their specificity, so without this WHM's `a:link { color:#08c }`
  would colour every link and its `input { … }` would reshape every field
  — measured: 295 property differences against a bare render with the
  compiled sheet alone, 4 (all viewport sizes) with the two rules.

Inline styles still apply — they win the cascade before either rule is
reached — so a meter's `style="width:40%"` and app.js's pinned menus work.
Custom properties are not part of `all`, so the theme's tokens flow.

A few project utilities (`cpr:panel`, `cpr:pill`, `cpr:hint`,
`cpr:stripe-ok`, `cpr:menu-item`, `cpr:sheet`, …) are defined with
`@utility` in `input.css` from daisyUI and Tailwind pieces; they exist so
the templates say what a thing is rather than repeat eight classes.

Themes are daisyUI's `light` and `dark`, keyed on the `data-theme` attribute
the rail's switch already sets on the document; `dark --prefersdark` is the
"system" choice. daisyUI's root colour, scroll-lock, scroll-gutter and
scrollbar components are excluded: the document is the host's. The
typefaces stay Fira Sans and Fira Code, set as the theme's `--font-sans`
and `--font-mono`. Another of daisyUI's themes is one word in `input.css`.

Scripts hook elements by `data-*` attributes (`data-menu`, `data-sheet`,
`data-tablewrap`, `button[aria-pressed]`), not by class, so a class list
can change without the behaviour going with it.

## Consequences

- Markup reads `class="cpr:btn cpr:btn-sm cpr:btn-primary"`. It is longer
  than before and it is daisyUI's vocabulary, which is the point.
- The compiled stylesheet is about 145 KB minified against 56 KB before,
  inlined into every page. Tailwind emits only what the templates, app.js
  and the Go sources use, so it grows with use, not with daisyUI.
- Changing a class in a Go string (the row stripes in `handlers.go`) needs
  `make css`, or the class has no rule. `css-check` catches the forgotten
  build.
- The terminal interface (ADR 0022) is not daisyUI and cannot be; its
  palette is its own.
- The `.gniza` wrapper gets `all: revert`, so nothing inherits from the
  host's chrome: a plugin page looks the same in WHM, DirectAdmin and
  cPanel's Jupiter, and the same is true of any host rule that is added
  later.

## Verifying a change

ADR 0009's recipe still applies — fetch the host's chrome, splice a page
in, serve it — and the measure is now a computed-style diff: render the
same page bare and inside the chrome, and compare `getComputedStyle` for
every element under `.gniza`. Anything but viewport-dependent sizes is a
leak.
