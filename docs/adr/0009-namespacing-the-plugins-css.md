# 0009 — Namespacing the plugin's CSS

Status: accepted; the prefix is `cpr:` since ADR 0023 (daisyUI), and the
stylesheet is compiled rather than written, but the invariant and the
reason for it are unchanged.
Date: 2026-08-31

## Context

The WHM plugin renders a fragment, not a page. WHM supplies the document
around it, which means WHM's stylesheets apply to everything we render.
Four sheets load on every page, and one of them —
`themes/x/style_optimized.css` — is the legacy theme, which claims short,
generic class names:

```css
.actions { background-color:#f3f3f3; border:1px solid #dcdfe3; margin-left:20px }
```

The accounts table used `class="actions"` for the buttons at the end of each
row. Every row therefore drew a grey bordered box across the width of the
cell, which is what the operator saw and reported as "what is this
background behind the buttons".

Scoping our own rules under `.cprest` stops our CSS leaking out. It does
nothing about WHM's leaking in. Two diagnoses before this one were wrong —
hidden inputs, then a stretched form — because they were reasoned from a
screenshot rather than measured.

An intersection of our class names against the class names in WHM's four
sheets found seven collisions on this server: `actions`, `btn`, `error`,
`help`, `muted`, `search`, `sub`. That set is specific to one WHM version
and one theme; another server loads different sheets.

## Decision

Every class the plugin renders carries a `cpr-` prefix. `cprest`, which
scopes the stylesheet, is the single exception. `cp-` was not available:
cPanel's own layout classes (`cp-layout-header` and friends) use it.

Element selectors cannot be namespaced, so WHM's rules for `h1`, `table`,
`input[type=password]` and `a:link` still reach us. Those are countered
explicitly in a reset block, written as `.cprest :where(h1, h2, h3)` so the
reset outranks WHM's bare element selectors without outranking our own
component rules.

`TestEveryRenderedClassIsNamespaced` and
`TestStylesheetOnlyTargetsNamespacedClasses` fail the build if an
unprefixed class appears in a template or the stylesheet. They assert an
invariant about our own source rather than pinning a list of WHM's names,
which would rot with the next cPanel release.

## Verifying a change

WHM's chrome can be reproduced off the server, which is the only way to see
a leak without deploying to production:

```sh
# the exact chrome WHM wraps a plugin in, plus the sheets it loads
ssh root@SERVER '/usr/local/cpanel/3rdparty/bin/perl -I/usr/local/cpanel \
  -e "use Whostmgr::HTMLInterface (); \
      Whostmgr::HTMLInterface::defheader(q{cprest},q{},q{}); \
      print qq{<!--FRAGMENT-->}; \
      Whostmgr::HTMLInterface::deffooter();"' > whmchrome.html

CPREST_DUMP=. go test ./internal/webui/ -run DumpPages   # every page as a fragment
```

Splice each fragment into the chrome, serve the directory, and walk
`document.styleSheets` calling `el.matches(rule.selectorText)` for every
element under `.cprest`. Anything that matches is a rule of WHM's that
applies to us. Note that in a browser with CSS nesting, every `CSSStyleRule`
has a `cssRules` property, so a recursive walk that tests it first will
descend past every rule and find nothing.

## Consequences

The plugin's markup reads a little heavier: `class="cpr-btn cpr-primary"`.
That is the price of rendering inside someone else's stylesheet, and it is
paid once at write time rather than repeatedly at diagnosis time.
