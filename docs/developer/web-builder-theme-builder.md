---
title: Theme Builder
description: Design the site's header, footer, single, archive, search, front page and 404 templates in Pursae Web Builder, and choose where each one shows with display conditions.
weight: 891
section: built-in-plugins
---

The Theme Builder designs the parts of the site that every page shares — the header and footer —
and the templates that show the site's content: single posts, pages, custom post type entries,
the blog and other archives, search results, the front page and the 404 page. Templates are
designed in the same editor as pages ([Pursae Web Builder](web-builder.md)), filled with the
site's real content at request time, and shown by the bundled **Hello Pursae** theme. Display
conditions decide which template appears where.

The field-by-field rules are in the [contract, section 11](web-builder-contract.md#11-theme-builder).
Pursae has no WordPress Site Editor; [coming from the Site Editor](web-builder-site-editor.md) maps its
features to the Theme Builder.

## Before you start

- **Turn on Pursae Web Builder** (Plugins › Pursae Web Builder › Activate). Activating it switches
  the site to **Hello Pursae** and remembers the previous theme; a notice on the Dashboard,
  Plugins and Themes screens says so and offers **Switch back**. Deactivating the plugin later puts
  the previous theme back (unless you picked another theme in between, or chose Hello Pursae
  yourself before turning the plugin on).
- On a site that already had the plugin on before the Theme Builder existed, nothing switches by
  itself: the same screens show a one-time **Switch to Hello Pursae** notice.
- Templates need the `edit_theme_options` capability (administrators). Authors and editors can
  build pages, but never open, publish or delete templates.
- Templates show only while Hello Pursae is the active theme. With any other theme, builder pages
  keep working as before (a full-page canvas) and templates are ignored.

## Managing templates

**Web Builder › Theme Builder** lists every template with its type, its display conditions in
words ("Entire Site", "Category: News", "Excluded: Page 'About'"), its priority, author and date.
The tabs filter by type; row actions are **Edit with Pursae Web Builder**, **Edit Conditions**,
**Preview**, **Duplicate**, **Disable** / **Enable** and **Trash**. When Hello Pursae is not the
active theme the screen offers to activate it.

**Add New Template** asks for the type, a name and a starter design, then opens the editor. The
types:

| Type | Fills | Used on | Starts with the condition |
|------|-------|---------|---------------------------|
| Header | the top of every page | all views | Entire Site |
| Footer | the bottom of every page | all views | Entire Site |
| Single Post | the main content | posts | All posts |
| Single Page | the main content | pages | All pages |
| Single | the main content | entries of one custom post type (chosen when you create it) | All entries of that type |
| Front Page | the main content | the front page | Front Page |
| Archive | the main content | the blog, category, tag, author, date and post type archives | All Archives |
| Search Results | the main content | search results | Search Results |
| 404 Page | the main content | the "not found" page | 404 Page |

A template is **active when it is published**; a draft (or a disabled template) is never shown.
Header and footer templates fill their own place; all the other types compete for the main
content, and only on the views they are made for — a Single Post template never shows on an
archive, even with the condition "Entire Site".

## Designing a template

A new template opens with its starter design (or empty, with **Blank**). The top bar shows the
template type, a **Display Conditions** button and a **Preview as** picker.

**Dynamic widgets.** Besides the usual sections and widgets, the widget panel has four groups whose
widgets show the site's content:

| Group | Widgets |
|-------|---------|
| Site | Site logo, Site title, Tagline, Menu, Search form, Copyright |
| Post | Post title, Post content, Post excerpt, Featured image, Post info, Post terms, Post date, Post author, Author box, Post navigation, Comments, and **Posts** (a loop) |
| Archive | Archive title, Archive description, Pagination |
| Search | Search query |

Drop one into a layout cell (or onto the page, which wraps it in a one-cell container) and style it
like any text or image. Each has its own settings: heading level, link to the post or the site,
date format, image size and ratio, which fields "Post info" lists and their separator, the
taxonomy of "Post terms", excerpt length, a prefix ("Posted on", "Search results for:"), and a text
to show when the value is empty.

**Real content while you edit.** The editor never invents sample content: it draws the widgets with
the post, archive or search chosen in **Preview as** (by default the newest post for a Single
Post, the front page or newest page for a Single Page, the blog for an Archive, a search for "a"
for Search Results). The post content is shown read-only. Changing the preview never changes where
the template is used.

**Menus.** A navigation or footer section can take its links from a menu location (**Links** ›
*Primary Menu* or *Footer Menu*) and its logo from the site (**Logo** › *Site logo*, the custom
logo or, without one, the site title). The links are then read-only in the editor: edit them under
Appearance › Menus, and every page follows. The **Menu** widget does the same in a layout cell,
as a row, a column or a menu button, with the width below which a row turns into a menu button.
Sub-menus open as drop-downs on hover and keyboard focus (three levels, the third indented in its
drop-down); in the menu button's panel and a column they sit indented under their parent.

**Post loops.** The **Posts** widget is a grid of cards. You design one card with post widgets
(featured image, title, excerpt, date…); the site repeats it for each post. Its **Query** panel
picks the source:

- **Posts of the page** — the request's own posts: the blog, the category being viewed, the
  search results. Use this in Archive and Search Results templates; it is the only source with
  pagination (numbers or previous / next).
- **Custom query** — a post type, how many (1–24), order, terms (for example only the category
  "News"), an author, how many to skip, and whether to leave out the post being viewed.
- **Related posts** — entries of the same type that share a term with the post being viewed.

Set **Text when empty** for the case where nothing matches. Loops show published posts only (plus
private ones to users who may read them).

## Display conditions

**Display Conditions** opens a list of rules. Each rule is **Include** or **Exclude** plus a place
on the site, narrowed step by step:

- **Entire Site**
- **Singular** › *All Singular*, *Front Page*, or a post type (Posts, Pages, a custom type) › all
  of them, a *Specific item*, entries *In* a category or tag, *Child of* an entry (hierarchical
  types), *By author*
- **Archives** › *All Archives*, *Posts Page*, *Date Archives*, *Search Results*, *Author
  Archives* (all or a specific author), a post type archive, *Categories* or *Tags* (all or a
  specific term)
- **404 Page**

Specific items, terms and authors are found by typing part of their name.

Include rules add views, Exclude rules take them out again. The first time you publish a template
without an Include rule, the dialog opens first; a template without one is not shown anywhere.

**Which template wins.** On every page the most specific matching rule decides:

1. a specific item, term or author (most specific);
2. Front Page and 404 Page;
3. In a term, Child of, By author;
4. a post type; Posts Page, Date Archives, Search Results, Author Archives, a post type archive,
   all terms of a taxonomy;
5. All Singular, All Archives;
6. Entire Site (least specific).

Any matching Exclude rule removes a template. When two templates match equally well, the higher
**Priority** wins (−100 to 100, default 0), then the more recently changed one. On a search page a
Search Results template beats an Archive template with the same rule. The dialog warns when other
published templates can show on the same views; it is only a warning.

### Examples

| You want | Template | Conditions |
|----------|----------|------------|
| One header everywhere | Header | Include › Entire Site |
| A different header on the landing page | a second Header | Include › Singular › Pages › Specific item › *Landing* (it beats the first header's Entire Site) |
| The footer everywhere except the page "Order" | Footer | Include › Entire Site; Exclude › Singular › Pages › Specific item › *Order* (that page then gets Hello Pursae's minimal footer) |
| One design for every blog post | Single Post | Include › Singular › Posts › All Posts |
| A special design for posts in "Recipes" | a second Single Post | Include › Singular › Posts › In Categories › *Recipes* |
| One archive for the blog and all categories, another for "News" | two Archives | first: Include › Archives › All Archives; second: Include › Archives › Categories › Specific term › *News* |
| Case studies (a custom post type) | Single (post type *Case studies*) and Archive | Single: Include › Singular › Case studies › All Case studies; Archive: Include › Archives › Case studies Archive |
| A seasonal header for a week | a second Header with Priority 10 | Include › Entire Site; disable it (or move it to draft) afterwards |

## How a page is assembled

With Hello Pursae active, every front-end page is the theme's own document with three parts:

- **Header** and **Footer** — the matching header and footer templates, else Hello Pursae's
  minimal header (logo or site title and the Primary Menu) and footer (Footer Menu and copyright).
- **Main** — in this order: the page's own builder design, if it has one; else the matching main
  template (Single Post, Archive, …), filled with the page's content; else the theme's default
  content, archive list or 404 message.

A page built with the builder has a **Header & footer** setting in its page settings: **Site
templates** (the header and footer templates around the page) or **This page only (canvas)** (the
page is the whole document, for landing pages with their own navigation). While Hello Pursae is
active a new page starts without its own navigation and footer, set to Site templates. Pages built
before the Theme Builder that have their own navigation or footer section stay canvas, so no page
shows two headers.

A static front page built in the builder shows its own design; a Front Page template is used when
the front page has none (or when the front page is the blog).

Pursae Commerce store pages: the product page takes a **Single Product** template and the shop and
product category / tag pages a **Product Archive** template (both offered while Pursae Commerce is
on); no other main template (Single Page, Archive, …) ever replaces a store page, so the cart and
the checkout keep working. The cart, checkout and account pages keep their store layout inside the
header and footer templates; a Cart or Checkout page designed in the builder shows its design
only when it contains the **Cart** / **Checkout** widget. Product templates are built from the
**Product** widgets (title, images, price, add to cart, rating, stock, meta, descriptions, tabs,
related products, upsells, sale badge) and the **Product archive** widgets (the store's product
grid of the page, title, description, breadcrumb). The **Shop** widgets (products, categories,
add-to-cart button, cart icon with mini cart, store notices) work on any page and template, and
the **Language switcher** (Pursae Multi Language) sits in the Site group. The settings and slots
are in the contract (§11.12).

## Preview, publish and cache

- **Preview as** › **Preview on the site** opens the preview object's URL with
  `?wb_template_preview=<id>`: the template fills its place there whatever the conditions (for
  users with `edit_theme_options`; never cached). Row action **Preview** on the Theme Builder
  screen does the same.
- **Publish** makes the template active; saving it as a draft or **Disable** takes it off the site.
  Publishing, disabling, trashing and changing conditions clear Pursae Cache completely. Pages with
  post loops are refreshed when any post is saved.
- The site's admin bar shows **Edit Header**, **Edit Template** and **Edit Footer** for the
  templates on the current page.
- Templates have revisions, the edit lock and autosaved drafts like pages. **Duplicate** copies a
  template as a new draft.
- Forms in a header or footer template work on every page they appear on.
- Tools › Export includes templates (with their conditions); on import, ids in conditions are
  mapped to the imported posts and terms, and rules that cannot be mapped are dropped.
- With Pursae Multi Language, a template can be translated like a page from the builder's
  **Page tools › Languages** tab (the translation starts as a disabled copy with the same
  conditions); each language shows its translation of the winning template when one is published.
  A template in another language never shows on a page: a template in the page's language beats
  one in the default language, which stays the fallback. Conditions that name a page, post or
  term also match its translations.

## Limits

- Archives of Pursae Custom Fields taxonomies are routed (`/?taxonomy=…&term=…`) and match the
  taxonomy and term rules of Archive templates.
- Popups (a template type, see [Web Builder](web-builder.md#popups)) and design kits (files a design
  brings, see [code sections](web-builder-code-sections.md)) exist, but there are no global classes.
  Colours, fonts and corners are the template's own unless it follows the
  [site style](web-builder.md#site-style) (Items › Globals › Site style), which new templates do.
