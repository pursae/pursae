# Pursae Forum: feature inventory (0.2.5)

Pursae Forum is a native (compiled-in) Pursae plugin for discussion boards in the class of bbPress:
categories and forums, topics and replies, front-end sign-up with email confirmation and Google
sign-in, moderation, member levels and badges, profile photos and a member list. It is written
from scratch for Pursae. Like every native plugin it is **off on a fresh install**
(`native_pursae_forum`): while it is off every forum URL, the sign-in routes, the admin screen and
the `forum/v1` REST routes answer 404, the menu is gone and the theme functions print nothing.
Deactivating keeps the forums, topics, replies, roles and members.

## What it does

- **Forums in any theme.** Every view renders as a page of the active theme, with the forum's own
  stylesheet (`forum.css`, scoped to `.pf`). Themes made for it declare
  `supports: {"pursae-forum": true}`, wrap the views in `templates/forum.html` and may replace any
  view (`docs/developer/themes/forum.md`).
- **Structure.** Categories hold forums (two levels). A forum can be closed to new topics.
  Topics can be pinned (sticky, first in their forum) and closed to replies.
- **Posting without JavaScript.** Plain forms, POST, then redirect. Text with basic HTML (links,
  bold, italic, lists, quotes, code), sanitized on save and again on output; links get `rel="nofollow ugc"`.
  Titles 3–120 characters, posts 2–20,000.
- **Search** over topics and replies (PostgreSQL full-text, `/forums/search/?q=`).
- **Profiles** (`/forums/u/<nicename>/`): photo, level and progress, badges, counts, bio, recent activity.
- **Member list** (`/forums/members/`): search by name, sort by newest, most active or name; cards
  with photo, level, badges and counts. It can be switched off (Settings › *Show the member list on the site*).
- **Shortcodes** for ordinary pages: `[pursae_forum]` (the forum list) and
  `[pursae_forum view="recent" limit="5"]` (recent topics).
- **Theme functions**: `forum_user_menu` (Log in / Join, or the member's avatar and Log out),
  `forum_recent_topics`, `forum_stats`, `forum_url`, `forum_is_view`, `forum_on`.
- **Demo content**: a theme's `demo/demo.json` may carry members, forums, topics and replies; importing
  it switches the forum on. Four library themes ship with one: Campfire, Devboard, Guild Hall and Study Circle.

URLs (default base `forums`, changeable; always with a trailing slash, also with plain permalinks):

| URL | View |
| --- | --- |
| `/forums/` | index: categories, forums, latest topics |
| `/forums/f/<slug>/[page/N/]` | a forum (or a category's forums); `/forums/f/<slug>/new/` new topic |
| `/forums/t/<id>/<slug>/[page/N/]` | a topic; a wrong or missing slug redirects (301) |
| `/forums/r/<id>/` | a reply: redirects to its page of the topic, `#post-<id>` |
| `/forums/edit/<id>/` | edit a topic or reply |
| `/forums/search/?q=`, `/forums/u/<nicename>/`, `/forums/members/` | search, profile, member list |
| `/forums/login/`, `/forums/register/`, `/forums/verify/`, `/forums/account/` | membership |

## Membership

- **Email sign-up** (`/forums/register/`): username, email, password (at least 8 characters, or
  the Pursae Security policy when it is on). A confirmation link is sent. It works for 48 hours and only
  its SHA-256 is stored. Unconfirmed members can log in and read; while *New members confirm their email
  address before they can post* is on (the default) they cannot post until they confirm. They can ask for a new link.
- **Login** on the forum's own page, with the core's throttling, the Pursae Security login rules and the
  login captcha when one is set up. Logging out returns to the forum.
- **Account page**: display name, bio, password (Google-only members choose one), profile photo,
  Google link and unlink.
- **Profile photos**: JPEG, PNG, WebP or GIF up to 2 MB. Larger than 6000×6000 or smaller than 16×16 is
  refused before decoding. The photo is turned upright (EXIF), cropped to a centred square of at
  most 512 px and stored in the Media Library, owned by the member. It is the member's site avatar
  too (`avatar_id`). Ten changes an hour. Removing it deletes only the forum's own upload. Members
  without a photo get an initials disc in a colour derived from their ID. No Gravatar.
- **Roles.** Activation adds *Forum Participant* (`forum_participate`) and *Forum Moderator*
  (`forum_participate`, `forum_moderate`). It also gives `forum_participate` to subscribers,
  contributors, authors and customers, and both caps to administrators, editors and shop managers.
  New members get Forum Participant (or Subscriber, a setting).
- **Spam guards** on the logged-out forms: a honeypot field, a signed time token, a captcha when one is
  set up, and rate limits (20 sign-up attempts per network in 10 minutes, 5 new accounts per network an
  hour, 3 confirmation mails per address and per network an hour). Posting: a flood interval (15 s by
  default), 30 posts an hour, the Discussion screen's disallowed words. Moderators skip the limits.
- **Enumeration.** While the core blocks user enumeration (the default), a profile is a 404 until
  the member has posted, and the member list shows only members who posted.

## Google sign-in

"Continue with Google" on the login and sign-up pages, and "Connect Google" on the account page:
OAuth 2.0 with PKCE and OpenID Connect, standard library only. The flow state travels in a short-lived
signed cookie, so nothing is stored on the server.

Setup (Forums › **Google sign-in**, which shows both values to copy):

1. In the Google Cloud Console, create an OAuth consent screen (External, scopes `openid`, `email`,
   `profile`) and an OAuth client of type **Web application**.
2. Add the site origin under *Authorized JavaScript origins* and
   `https://<your-site>/forum-auth/google/callback` under *Authorized redirect URIs*. The route is fixed
   and has no language prefix, so the URI never changes.
3. Paste the Client ID and Client secret, tick **Enable** and save. The secret is write-only: it is stored
   encrypted with a key derived from the server secret, never shown again, and `/pursae/v1/options/forum` masks it.

Google requires HTTPS (except `localhost`). A Google account whose verified email matches an existing
member is linked to that member, unless the member can edit content, moderate or manage the site: they
log in with their password and connect Google from the account page. Linking an account that never
confirmed its email resets its password and sessions (protection against account pre-hijacking).
Administrators, and members with a Pursae Security second factor, always log in with their password.
While registration is closed, Google cannot create accounts.

## Levels and badges

- **Points** = published topics × topic points (3) + published replies × reply points (1). Nothing is
  stored per post; spam and trash do not count.
- **Levels** by points: Newcomer 0, Member 10, Regular 50, Contributor 150, Veteran 400, Legend 1000.
  Each level has a name, a minimum, an icon (43 line icons) or a Media Library image, and a colour. Up
  to 30 levels. Profiles show the progress to the next one.
- **Badges** with a rule: topics started, replies written, posts, points or days since joining (each
  with a threshold); verified email; Google account linked; forum moderator; or *awarded by hand*.
  Defaults: First Post, Conversation Starter (5 topics), Helpful (50 replies), Prolific (250 posts),
  One Year Club, Verified, Moderator, Founding Member (by hand). Each badge has a name, description, icon
  or image, colour and an on/off switch. Up to 100; the list order is the display order.
- Rule badges are computed from the member's numbers when they are shown, so they appear and disappear
  by themselves (behind the page cache, with its delay). Manual
  awards are stored per member (`forum_badges`). Administrators award and revoke them from the admin member list
  (bulk actions, or a member's **Manage** page) or from the member's forum profile.
- Levels and badges show next to every post (level and up to three badges), on profiles, member cards
  and in the admin. Default names are translated; custom names print as typed.

## Moderation

- **Front end**: moderators get a button bar under each post: approve, pin and unpin, close and reopen
  (topics), spam and not spam, trash and restore. They see pending posts in place and can open a spam or
  trashed topic by its link, with the author's IP address, and can block or unblock a member from the profile. Closed forums and
  topics do not stop them.
- **Admin › Forums › Moderation**: topics and replies by status (pending, published, spam, trash), with
  search, row actions and bulk actions.
- **Holding posts**: optional moderation of a member's first post; posts that hit the disallowed words
  are refused.
- **Blocking** takes `forum_participate` away (a per-user deny), so the member keeps the account but
  cannot post. Administrators cannot be blocked, and only an administrator can block a moderator.
- **Editing**: members edit their own posts within the edit window (30 minutes, 0 to turn it off,
  up to a week); moderators always can. Edited posts say so.

## Personal data

Tools › Export Personal Data exports a member's topics and replies (with the stored author IP), forum
profile settings, likes, points, subscriptions and the reports they filed.

Tools › Erase Personal Data **anonymizes** the member's forum presence, the way WordPress anonymizes
comments. The account itself stays (as in WordPress); nothing in the forum links to it any more:

| Data | Erase |
| --- | --- |
| Topics and replies | Stay published with their text, so threads and reply numbers do not break. The author is cleared (`author_id` NULL) and the stored author IP removed; every view shows **Deleted user** (translated, e.g. tr_TR *Silinmiş kullanıcı*) with an empty avatar and no profile link; REST answers `"author": null` |
| Edit history | Revisions of forum posts they authored lose their author |
| Post meta naming them | Last poster of a topic or forum (set to 0), who accepted a solution (removed), who moderated a post last (`by` set to 0), the moderators shown on a forum (removed from the list) |
| Likes they gave | Kept as **anonymous likes** (`forum_likes.user_id` NULL): the post keeps its count and its author the points the like earned |
| Points ledger | Their own rows are deleted (the posts that earned them are no longer theirs). Rows they caused for other members (a like, a manual adjustment they made) stay with `actor_id` NULL; a like of a post that already has an anonymous like row is merged into it (one event per kind, post and actor), so totals and *members of the week* do not change |
| Subscriptions | Deleted |
| Reports they filed | Stay in the moderation queue without reporter or note; reports they resolved lose `resolved_by` |
| Forum profile | Bio, the photo the forum uploaded (the attachment is deleted), badges, the rules and digest settings and the saved draft are removed. An avatar an administrator picked in the core profile stays with the account |
| Forum sign-in | Linked Google account and verified address stay with the account until it is deleted (reported as retained) |

Every row changed counts as one item removed; the request screen lists the messages. No other
member's posts, likes, points or settings change. Their profile page has no posts afterwards, so
visitors get a 404 (moderators still see the empty profile). Posts of an account that is deleted
later (`author_id` NULL) show as *Deleted user* the same way.

Deleting an account (Users › Delete) keeps the likes it gave the same way: they become anonymous
likes and its like ledger rows lose their actor, merged into the post's existing anonymous row, so
the posts keep their counts and their authors their points however many likers are deleted.

## The admin screen

**Forums** in the admin menu, after Comments, visible with `forum_moderate`:

| Tab | Who | What |
| --- | --- | --- |
| Overview | moderators | totals, the moderation queue (pending and spam, linked), a 30-day bar chart of new members, topics and replies (plain CSS, no script), top contributors, recent activity |
| Forums | administrators | categories and forums: add, edit, order, close, delete (only when empty) |
| Moderation | moderators | the queue described above |
| Members | moderators (roles and badges: administrators) | a list table with photo, level, badges, topics, replies, points, joined, last active, status and Google. Filters (unverified, blocked, moderators, Google), search (also by email), sorting; row actions (manage, view profile, edit user, verify, block or unblock); bulk actions to verify, block, unblock, award or take back a badge and set the forum role; a per-member **Manage** page |
| Badges & Levels | administrators | point weights, the levels table, the badge list with add, edit, reorder and delete |
| Settings | administrators | URL base, forum title, topics and replies per page, registration, require confirmation, default role, first-post moderation, edit window, flood interval, the member list |
| Google sign-in | administrators | setup steps, client ID and secret |

Settings are also readable and writable at `/pursae/v1/options/forum`.

## Multi Language

With Pursae Multi Language on, forum URLs answer under each language's prefix (`/en/forums/…`) and keep
it on every link, form and redirect. The forum's own text follows the page's language. The language
switcher on a forum page leads to the same forum page in the other language. Public views print
`hreflang` alternates, `x-default` (on the first page) and a canonical URL in the page's language; private views (login,
search, account) print none. The content itself (forums, topics, replies) is shared by every language.

## Caching and SEO

Pages a visitor sees are cacheable by Pursae Cache and tagged `forum`; every forum write purges the
tag (a new photo purges everything). A logged-in member's pages carry their forms and are sent
`no-store`. Login, sign-up, email confirmation, account, search, new topic, edit and a member search are `noindex` and
never cached. Forum views get a canonical URL (with `/page/N/`) and no shortlink. Pursae SEO does
not add its head to them.

Instead, public forum views (index, forum, topic, topic lists, profile) print their own tags,
and only when the view sets a title or description; `noindex` and private views (login, search,
account and so on) print none:

- `<meta name="description">` from the forum or topic text, whitespace collapsed; over 160
  characters it is cut to 157 plus "…".
- Open Graph: `og:type` (`article` on a topic, `website` elsewhere), `og:title`, `og:description`,
  `og:url` (with `/page/N/`) and `og:site_name` (the site title, translated by Pursae SEO / Pursae
  Multi Language where that applies).
- A Twitter card. While Pursae SEO is on and its Titles & Meta › default image is set, that image
  becomes `og:image` and `twitter:image` with a `summary_large_image` card (a relative path is made
  absolute against the home URL). For a Media Library upload `og:image:width`/`height` are added
  when both are known, `og:image:type` when the MIME type is known and `og:image:alt` when the
  attachment has alt text. Without an image the card is `summary`.

These tags ignore Pursae SEO's Open Graph and X cards on/off switches and its card type: the forum
always prints them. See `forumSEOHead` and `forumSocialImage` in `internal/server/forum_render.go`.

## API

- `GET /wp-json/forum/v1/forums`, `/forums/<id>/topics`, `/topics/<id>`, `/topics/<id>/replies`: public
  read routes, published content only (`docs/developer/rest-api/endpoints.md`).
- `/pursae/v1/options/forum`: the settings, like the other built-in plugins.
- 0.2.5: the community routes listed above (views, presence, likes, solutions, subscriptions,
  reports, previews, drafts, tags, similar topics, statistics, levels, badges, members of the week,
  `me`). Member routes take cookie + `X-WP-Nonce` (`wp_rest`) or an Application Password.

## Community features (0.2.5)

Everything here is part of the plugin and follows its on/off switch; nothing is rendered or stored
while it is off. Visitor pages stay cacheable: what is visitor-specific (likes, follows, the forms)
only appears on members' pages, which are `no-store`; views and "who is online" are refreshed from
cached pages by `forum.js` through the REST API.

| Feature | Data | URLs and REST | Notes |
| --- | --- | --- | --- |
| **Forum look** | forum `posts.meta`: `_forum_icon`, `_forum_color` (`sky\|mint\|peach\|lilac\|grey`, or a custom `#rrggbb` since 0.2.6), `_forum_label`, `_forum_number`, `_forum_chips` [names], `_forum_tips` [lines], `_forum_staff_only`, `_forum_moderators` [user ids] | Forums › Forums (edit: *Look*: colour swatches + a custom colour picker, a 6×6 icon grid of radios); `forum/v1/forums` adds `icon color color_hex color_ink label number chips tips staff_only` | A custom colour gets a computed text colour (`ColorInk`, contrast ≥ 4.5:1). Chips link the forum's `?tag=<slug>` filter. *Team only* (`staff_only`): only moderators start topics, members still reply |
| **Topic tags** | topic `_forum_tags` [slugs] + `_forum_tag_names` [names] (GIN index `posts_forum_tags`) | tag field on the new-topic form (≤ 5, comma separated); `/forums/tag/<slug>/`, `/forums/tags/`, `?tag=` on forum, index and `/forums/topics/`; `GET forum/v1/tags` | Tags are plain text ≤ 30 characters, slugs from `SanitizeTitle` |
| **Views** | `forum_topic_views (topic_id, views)` | `POST forum/v1/topics/<id>/view` (forum.js beacon) → `{views, counted}` | Counted once per visitor and topic per 30 minutes (member id, else a salted hash of address + user agent, never stored), not for robots or empty user agents, ≤ 120 per network per 10 minutes. Cached pages count too; no-JS visitors are not counted |
| **Sort tabs** | — | `?sort=latest\|popular\|unanswered\|solved` on index, forum, `/forums/topics/`, tag pages and `forum/v1/forums/<id>/topics` | Popular = views + 10 × replies. `sort` and `tag` are page cache key parameters while the plugin is on; `?sort=latest` and unknown values are 301-redirected to the canonical URL |
| **Likes** | `forum_likes (post_id, user_id)`; `user_id` NULL is an anonymous like left by the personal data eraser or a deleted account (0109) | `forum_action=like\|unlike` (+ `post_id`) on the topic URL; `POST/DELETE forum/v1/posts/<id>/like` | Members who may post; not their own post; 60 per 10 minutes. A like purges only that topic's pages (`forum-topic:<id>` cache tag). The author earns `like_points` |
| **Solved** | topic `_forum_solution` (reply id), `_forum_solved_at`, `_forum_solved_by` | `forum_action=solve\|unsolve`; `POST/DELETE forum/v1/topics/<id>/solution` | The topic's author (who may post) or a moderator. The answer is highlighted (`.pf-post--solution`) and repeated under the opening post; lists show a *Solved* badge. Trashing or spamming the answer withdraws it. Its author earns `solution_points` (not on their own topic); badge rule `solutions` |
| **Who is online** | process memory (visitor key → forum, time) | `POST forum/v1/presence {forum}`, `GET forum/v1/online?forum=` | Window *Forums › Settings › Online* (5 minutes). Each instance counts its own visitors |
| **Points ledger** | `forum_points (user_id, kind, points, post_id, actor_id, note, created_at)` | Forums › Members › member › *Adjust points*; `GET forum/v1/weekly` | One row per topic, reply, accepted answer, like received and manual adjustment. Totals stay computed from the posts (+ manual rows); the ledger dates them for **members of the week** (last 7 days, only rows whose post is still live) |
| **Levels and badges** | levels option adds `solution_points` (10), `like_points` (1); `forumLevel.Rank` | `GET forum/v1/levels`, `/badges` | New rules: `solutions` (accepted answers), `likes` (likes received), `streak_days` (longest run of consecutive days with a post). New default badges *Problem Solver* (5 accepted answers) and *7-Day Streak* |
| **Statistics** | — | `GET forum/v1/stats` | `.Stats` on index, login, register and verify: forums, topics, replies, members, solved and solved %, new topics and members this week, online |
| **Moderators per forum** | `_forum_moderators` | Forums › Forums › edit | A display list (the forum's own, else the site's forum moderators, administrators first); rights stay site-wide |
| **New topic** | users.meta `forum_draft` | `/forums/new/` (forum picker), `?preview=1` and `draft=1` buttons; `POST forum/v1/preview`, `GET/POST/DELETE forum/v1/draft` | The picker lists every forum, closed and team-only forums disabled for members. *Preview* renders the text as it will be posted, nothing is saved. *Save draft* keeps one draft per member; posting deletes it |
| **Follow by email** | `forum_subscriptions (topic_id, user_id)` | *Email me when someone replies* on the new-topic and reply forms, *Follow* on a topic; `POST/DELETE forum/v1/topics/<id>/subscription`; `/forums/unsubscribe/?t=&u=&k=` | A published reply (or an approved one) mails followers except its author, ≤ 500 per reply, through the mail queue. The link is an HMAC of the site secret; GET asks, POST unsubscribes (mail scanners open links). Off: *Forums › Settings › Notifications* |
| **Similar topics** | — | `.Similar` on topic and new topic; `GET forum/v1/topics/similar?q=&tags=` | Words of the title (`simple` text search) and shared tags |
| **Sign-up choices** | users.meta `forum_rules_accepted` (RFC 3339), `forum_digest` | *Forums › Settings › Community rules* (URL, required) | The rules checkbox links the rules page; *weekly digest* is stored (and editable on the account page) for a digest sender; no digest is mailed by 0.2.5 |
| **Change the address** | — | `forum_action=change_email` on the verify URL | Only for unconfirmed accounts: the logged-in member, or the visitor who just signed up (a signed token on the check-your-inbox step, valid 48 hours). Sends a new confirmation link |
| **Report a post** | `forum_reports (post_id, user_id, reason, note, status)` | *Report* on each post (`spam\|abuse\|off_topic\|other` + note); `POST forum/v1/posts/<id>/report` | Members only, once per post (a second report updates it), 10 an hour. *Forums › Moderation › Reported* lists them with reasons and notes; *Dismiss Reports*, Spam or Trash closes them |

The demo format carries all of it (see the theme guide): forum meta, tags, views, likes, the
accepted answer and the rules URL.

## Not in 0.2.5

- ✗ Per-language forums or translated topics: content is shared by every language.
- ✗ Guest posting, private or hidden forums, per-forum moderation rights (the per-forum list is for
  display), reply threading, topic merge and split, favourites, revision history.
- ✗ Sending the weekly digest (the preference is stored).
- ✗ Forum RSS feeds, sitemap entries for forums and topics, forum results in the site search.
- ✗ Web Builder widgets for forum content. Web Builder themes link the forum and style it through the
  `--pf-*` tokens; the forum body is not designed in the builder.
- ✗ Changing a confirmed email address from the forum (it is done on the core profile screen), and
  importers from bbPress or other boards.

## Files

| File | What it holds |
| --- | --- |
| `internal/server/forum.go` | constants, settings, activation and roles, plugin row, menu, routes |
| `internal/server/forum_types.go` | the domain types and `forumPage`, the data of every view template |
| `internal/server/forum_store.go` | data access: lists, create, moderate, recount, authors |
| `internal/server/forum_urls.go` | URL building, path parsing, the language switcher's forum URLs |
| `internal/server/forum_render.go` | the synthetic page, theme overrides, shortcodes, pagination, messages |
| `internal/server/forum_front.go`, `forum_views.go`, `forum_post.go` | the front end: intercept, views, forms, moderation |
| `internal/server/forum_members_front.go` | the public member list |
| `internal/server/forum_account.go`, `forum_mail.go`, `forum_google.go` | login, sign-up, confirmation mail, account, photo upload, Google sign-in |
| `internal/server/forum_gamify.go`, `forum_icons.go`, `forum_avatar.go` | points, levels, badges, member queries, the overview, icons, photos and initials |
| `internal/server/forum_funcs.go` | the template functions for themes |
| `internal/server/forum_admin.go`, `web/admin/templates/pages/forum*.html` | the admin screen |
| `internal/server/forum_demo.go` | the theme demo's `forum` section |
| `internal/server/rest_forum.go`, `rest_forum_engage.go` | `forum/v1` |
| `internal/server/forum_meta.go` | 0.2.5: forum meta, tags, sort tabs, statistics, members of the week, similar topics, the forum picker |
| `internal/server/forum_engage.go`, `forum_engage_post.go` | 0.2.5: likes, views, solutions, online, the points ledger, reports, subscriptions, drafts and their forms |
| `internal/server/forum_views_lists.go`, `forum_account_more.go` | 0.2.5: all topics, tags, unsubscribe views; sign-up choices and change the address |
| `internal/db/migrations/0097_forum.sql` | the `forum`, `topic` and `reply` content types, their indexes, one account per Google subject |
| `internal/db/migrations/0104_forum_engagement.sql` | 0.2.5: likes, views, the points ledger, subscriptions, reports; tag and solved indexes |
| `internal/db/migrations/0109_forum_anonymous_likes.sql` | Anonymous likes: `forum_likes.user_id` nullable, one like per member and post kept by a partial unique index |
| `internal/server/privacy_sources.go` | The forum's personal data exporter and eraser (anonymization) |
| `web/forum/templates/*.html`, `web/forum/assets/forum.css`, `forum.js` | the default view templates, the stylesheet and the optional script |
| `library/themes/{campfire,devboard,guild-hall,study-circle}` | the forum themes |
| `internal/server/forum_*_test.go`, `theme_library_forum_*_test.go` | unit and integration tests |
