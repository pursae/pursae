# Gopher Facts (shortcode-demo)

An example plugin for WordPress Go that uses every part of the plugin SDK beyond hooks:

- `[gopher-facts count="3"]` prints a list of gopher facts, styled by the package's `assets/style.css`.
- `[recent-gophers count="5"]` lists the newest posts through the posts API.
- `GET /wp-json/shortcode-demo/v1/facts` and `/facts/<id>` return the facts as JSON; `POST /facts` adds one (editors only).
- **Settings › Gopher Facts** holds the heading, the number of facts, a greeting toggle, a list style and a footer note.
- A transient caches the fact list for a minute; a scheduled event (`cron_shortcode_demo_tick`) counts its runs in plugin storage.

The source is in `examples/plugins/shortcode-demo` of the WordPress Go repository.
