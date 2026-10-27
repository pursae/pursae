# Starter

A minimal WordPress Go theme package: the smallest complete theme that still looks good. Use it as
the starting point for your own package (copy the directory, change `slug`, `name` and the styles).

- `theme.json` registers the Sidebar widget area, the Primary and Footer menu locations, the
  stylesheet, the navigation script, an editor stylesheet, a color palette and a Customizer section
  with an accent color, a sidebar toggle, a content width and a footer text.
- `templates/` follows the template hierarchy: `index`, `single`, `page`, `archive`, `search`,
  `404` plus the parts `header`, `footer`, `sidebar`, `comments`, `searchform`, `content`,
  `content-single`, `content-page` and `content-none`.
- `assets/` holds `style.css`, `editor-style.css`, `js/navigation.js` and `screenshot.png`.

Build the package with `tools/build-themes.sh` (it writes `examples/themes/dist/starter.zip`) and
install it in Appearance › Themes › Add Theme › Upload Theme. See `docs/theme-package.md`.
