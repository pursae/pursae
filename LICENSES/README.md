# License texts

Pursae itself is licensed under the GNU General Public License, version 2 or (at your option) any
later version: [`../LICENSE`](../LICENSE) (a copy is [`GPL-2.0.txt`](GPL-2.0.txt)).

This directory holds the full texts of the licenses of the third-party components Pursae bundles.
[`../NOTICE`](../NOTICE) lists every component with its version, license, copyright holder and
source. The permissive licenses are templates: the `<year>` and `<copyright holder>` placeholders
stand for the copyright lines NOTICE gives for each component.

| File | License | Used by (see NOTICE for the full list) |
| --- | --- | --- |
| `GPL-2.0.txt` | GNU GPL v2 | Pursae, WordPress core and block editor, Plupload, mOxie, Iris, Dashicons (with font exception) |
| `LGPL-2.1.txt` | GNU LGPL v2.1 | TinyMCE, Crystal Project icons, libvips and GLib in wasm-vips |
| `MPL-2.0.txt` | Mozilla Public License 2.0 | Mediabunny (block editor video conversion) |
| `Apache-2.0.txt` | Apache License 2.0 | PDF.js, wazero, purego, pprof, go-tpm, MongoDB Go driver (bson), Mousetrap |
| `MIT.txt` | MIT | jQuery, jQuery UI, React, Lodash, Moment.js, CodeMirror, MediaElement.js, Go modules, … |
| `BSD-2-Clause.txt` | BSD 2-Clause | Esprima, Espree, go-sourcemap, go-webauthn/x (revoke), libavif, dav1d, libaom |
| `BSD-3-Clause.txt` | BSD 3-Clause | Go and golang.org/x, bluemonday, gorilla/css, go-webauthn, google/uuid, protobuf, libwebp, libyuv, Cropper |
| `ISC.txt` | ISC | lucide-react, maxminddb-golang, go-webauthn/x (secp256k1), world-atlas, objectFitPolyfill |
| `0BSD.txt` | BSD Zero Clause | tslib |
| `OFL-1.1.txt` | SIL Open Font License 1.1 | Every bundled web font except Dashicons |
| `CC-BY-4.0.txt` | Creative Commons Attribution 4.0 | DB-IP IP to Country Lite database |
| `W3C-20150513.txt` | W3C Software and Document License | inert polyfill |
| `WTFPL.txt` | WTFPL v2 | jQuery Query |

Components whose own license file ships next to them keep it there: `web/wp-includes/js/tinymce/license.txt`,
`web/wp-includes/js/plupload/license.txt`, `web/wp-includes/images/crystal/license.txt`,
`web/flipbook/vendor/LICENSE.pdfjs.txt` and `LICENSE.page-flip.txt`. The Pursae Web Builder's bundled npm
packages carry their notices in full in `web/web-builder/THIRD_PARTY_LICENSES.txt` (written by
`builder/scripts/licenses.mjs`). Public-domain and CC0 material (JSON2, Natural Earth, element-closest,
Twenty Twenty-Five's photographs) needs no license text.
