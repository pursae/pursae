---
title: Coming from the Site Editor
description: Pursae has no WordPress Site Editor (Full Site Editing). What each Site Editor feature is called in Pursae Web Builder, and what happens to a block theme site's templates, template parts, global styles and navigation menus when it is imported.
weight: 894
section: built-in-plugins
---

Pursae does not have WordPress's Site Editor (Full Site Editing, FSE) and is not going to get one:
the whole site — header, footer, the templates of posts, pages, archives, search and the 404
page, the site's colours and fonts — is designed in [Pursae Web Builder](web-builder.md) and its
[Theme Builder](web-builder-theme-builder.md). The block editor stays the editor for posts and pages
that are not built with the builder.

## Where each Site Editor feature lives

| Site Editor (block theme) | Pursae Web Builder |
|---------------------------|--------------------|
| Template parts: Header, Footer | **Header** and **Footer** templates (Appearance › Theme Builder) |
| Other template parts (a sidebar, a comments area…) | a **saved section** (Items › Components) placed in each template, or a section of the template itself |
| Templates: Single, Page, Index / Home, Archive, Category, Tag, Author, Date, Search, 404, Front Page | **Single Post**, **Single Page**, **Single** (custom post types), **Archive**, **Search Results**, **404 Page** and **Front Page** templates |
| The template hierarchy (`single-{post-type}`, `category-{slug}`, `page-{slug}`…) and custom page templates | **Display conditions** on each template: Include / Exclude rules down to one post, page, term or author, with priorities ([which template wins](web-builder-theme-builder.md#display-conditions)) |
| Global Styles: colours, typography, corners | the **site style** (Items › Globals › Site style): one brand colour, font pair, fonts and corners that pages and templates follow ([site style](web-builder.md#site-style)) |
| Global Styles: palette and font presets for custom CSS | **Design tokens** (Settings › Web Builder): five colours and two fonts as CSS custom properties on every page |
| Style variations | the designs of the [theme library](web-builder-theme-library.md) and design kits |
| Navigation block / `wp_navigation` menus, submenus | **Menus** under Appearance › Menus, shown by the **Menu** widget or a navigation section bound to a menu location, with drop-down sub-menus (three levels) |
| Site Logo, Site Title, Site Tagline blocks | the **Site logo**, **Site title** and **Tagline** widgets (Site group) |
| Query Loop, Post Template | the **Posts** widget (a card you design, repeated per post, with its own query) |
| Post Title, Post Content, Post Featured Image, Post Date, Post Terms, Post Author, Comments, Pagination… | the **Post**, **Archive** and **Search** widget groups |
| Template preview with real content | **Preview as** in the editor, and **Preview on the site** |
| Revisions of templates and styles | template revisions, autosaves and the edit lock, as for pages |

## Importing a site that uses a block theme

The WordPress importer (Tools › Import) and the WP to Pursae migration
([migrating from WordPress](operations/migrate-from-wordpress.md)) handle the Site Editor's own post
types like this:

- **Navigation menus** (`wp_navigation`): every published navigation menu becomes a menu under
  Appearance › Menus with its links and submenus. Links to imported posts, pages, categories and
  tags point at the imported items; other links keep their URL. The first one goes to the
  **Primary Menu** location when no menu is assigned there yet. A menu with the same name that
  already exists is left alone, so importing again adds nothing. Page lists and other blocks
  without a fixed link are left out.
- **Templates, template parts, global styles and fonts** (`wp_template`, `wp_template_part`,
  `wp_global_styles`, `wp_font_family`, `wp_font_face`): not imported. The report counts them in
  one line and links to the Theme Builder. Recreate the header and footer there (or start from a
  design of the theme library) and set the site's colours and fonts.
- **Synced patterns** (`wp_block`) import as patterns, and posts and pages import as usual.

A quick start for a block theme site:

1. Import the content.
2. Turn on Pursae Web Builder (Plugins); the site switches to **Hello Pursae**, whose header
   already shows the site logo or title and the Primary Menu.
3. Under Appearance › Theme Builder, add a **Header** and a **Footer** (or import a whole design
   from **Items › Components › Browse theme library** in the editor), then the **Single Post**,
   **Archive** and other templates you need.
4. In the editor, set the brand colour, fonts and corners under **Items › Globals** and click
   **Save as site style**: new pages and templates follow it.

## For developers

The block editor's Site Editor routes exist so that block editor bundles do not fail, but they
hold nothing: `wp/v2/templates` and `wp/v2/template-parts` return empty lists,
`wp/v2/templates/lookup` an empty object, `wp/v2/global-styles/*` empty settings and styles
(read only). The active theme reports `is_block_theme: false` and the `block-templates` theme
support as `false`. Theme Builder templates are reached through the Web Builder REST namespace
(`web-builder/v2`, [contract §11.8](web-builder-contract.md#118-template-rest)), not through
`wp/v2/templates`.
