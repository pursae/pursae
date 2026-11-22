---
title: Compatibility policy
description: What stays stable from Pursae 0.5.0 on, what may change, how things are deprecated and how releases are numbered.
weight: 55
section: getting-started
---

From **0.5.0** on, Pursae keeps the surfaces below stable, so a site, a plugin, a theme, a script
or an AI client written against one 0.5.x release keeps working on the next. This page says what
that promise covers, what it does not, and how something is retired when it has to go.

## Versioning

- After 0.5.0 the releases are **0.5.1, 0.5.2, …**. Each may contain fixes **and new features**;
  none removes or changes a stable surface without the deprecation period below.
- A breaking change happens only after a deprecation period, and then in a later minor release
  (0.6.0 …) or in 1.0. The release notes and `CHANGELOG.md` list it under **Removed** or
  **Changed**, with what to do instead.
- Automatic core updates install only patch releases of the running minor version
  ([upgrading](getting-started/upgrading.md)); a new minor version waits for
  a click.
- Before 1.0 Pursae follows [Semantic Versioning](https://semver.org/) with the 0.x reading above:
  the minor number marks the releases that may remove deprecated things.

## What is stable

| Surface | The promise | Reference |
|---------|-------------|-----------|
| **REST API** under `wp/v2` and `pursae/v1` | Documented routes keep their paths, methods, parameters, response fields and error codes. New routes, new optional parameters and new response fields may be added; clients must ignore fields they do not know | [endpoints](rest-api/endpoints.md), [authentication](rest-api/authentication.md) |
| **Command line** | Documented commands (`pursae serve`, `migrate`, `version`, `healthcheck`, `setup-token`, `launch`, `adminsec`, `headless`, `mcp`, `plugin …` and the WP-CLI style nouns), their flags, exit codes and documented output formats (`--format=json`, `csv`, `yaml`, `ids`, `count`). New commands and flags may be added | [command line](operations/cli.md) |
| **Environment variables** `PURSAE_*` | Names, meaning and accepted values of the documented variables, including `PURSAE_METRICS*` and `PURSAE_SLOW_QUERY` and the metric names and labels of [metrics](operations/metrics.md). The `WPGO_*` names are deprecated aliases until at least 1.0 | [configuration](operations/configuration.md), [metrics](operations/metrics.md), [renamed in 0.5.0](getting-started/upgrading.md#renamed-in-050-environment-variables-and-the-binary) |
| **Plugin ABI, version 1** | The exports, host imports, request and result shapes, the envelope and the permission vocabulary of ABI 1. Additive changes keep the version; removals and changes bump it, and a host keeps running the previous ABI through the deprecation period. A plugin built for ABI 1 leaves `"abi"` out of `plugin.json` (releases before 0.5.0 reject the field); a later version is declared as `"abi": 2` with `"requires"` at least `0.5.0` | [ABI](plugins/abi.md#abi-versions), [plugin.json](plugins/manifest.md), [permissions](plugins/permissions.md) |
| **Plugin packages** | The package layout, `plugin.json` fields and the signature format | [packaging and signing](plugins/packaging-and-signing.md) |
| **Themes** | The theme package format and the `theme.json` fields Pursae documents as supported; templates, template parts and the template hierarchy | [theme package](../theme-package.md), [theme.json](themes/theme-json.md), [templates](themes/templates.md) |
| **Database schema** | Every migration follows **expand/contract**: a release only adds what the previous release does not violate, and drops or changes something only after an earlier release stopped using it, so the previous minor release still runs on the new schema (rollback). The **schema guard** refuses to start a binary below the schema's recorded floor instead of letting it corrupt data | [schema changes](getting-started/upgrading.md#schema-changes) |
| **MCP server** | Tool names, their documented arguments and result shapes, and the scopes they need. New tools and optional arguments may be added | [AI agents and MCP](operations/ai-mcp.md#tools) |

Plugin routes (`/<plugin-slug>/v1/…`) are the plugin's own contract, not Pursae's. Routes of the
built-in plugins that mirror a WordPress plugin (`pll/v1`, `contact-form-7/v1`, `wc/v3`) follow that
plugin's API as documented; `web-builder/v2` follows the [Web Builder contract](web-builder-contract.md).

## What is not stable

- **Internal Go packages** (`internal/…`) and the Go module path `wpgo`: nothing outside this
  repository may import them, and they change in any release.
- **Admin HTML, CSS and JavaScript**: markup, class names, screen layout and admin URLs other than
  the WordPress ones (`/wp-admin/…` screens behave like WordPress, their markup does not promise
  anything). Do not scrape the admin; use the REST API, the CLI or MCP.
- **Undocumented routes, fields, commands and variables**, and anything documented as
  *development only*, *experimental* or *internal* (for example `devhub` build settings, the test
  variables, `PURSAE_LISTEN_FD`).
- **Log lines** and human-readable messages, including CLI output without a `--format`.
- **Performance characteristics** and the built-in limits, which may be raised.
- **Bug-compatibility**: a behavior that contradicts the documentation or WordPress' documented
  behavior is fixed, with a CHANGELOG line under **Fixed**.

## Deprecation

When a stable surface has to change:

1. **Mark it deprecated.** The release that replaces it adds the replacement, keeps the old way
   working, and lists the change under **Deprecated** in `CHANGELOG.md` and the release notes, with
   what to use instead and the earliest release that may remove it.
2. **Warn.** Where Pursae can tell the old way is used, it says so: a log line at start-up (renamed
   environment variables), a REST response header or a deprecation note in the API index, a CLI
   warning on standard error, a notice in the admin. A deprecated item that cannot warn is called
   out in the documentation next to it.
3. **Keep it for at least two releases**, and in any case until the next minor version after the
   one that deprecated it; for the deprecations of 0.5.0 (the `WPGO_*` variable names and the
   `wpgo` binary name) until **1.0**.
4. **Remove it** in a minor release (or 1.0), listed under **Removed** in `CHANGELOG.md`.

Security fixes are the exception: when keeping a surface would leave sites exposed it can change in
a patch release, with the reason in the release notes.

## Current deprecations

| Deprecated | Since | Use instead | Removed no earlier than |
|------------|-------|-------------|-------------------------|
| `WPGO_*` environment variables | 0.5.0 | `PURSAE_*` (same suffix) | 1.0 |
| the `wpgo` binary name (`bin/wpgo`, `/usr/local/bin/wpgo`) | 0.5.0 | `pursae` | 1.0 |
