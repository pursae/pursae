package server

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/plugins"
	"wpgo/internal/wp"
)

// Declarative fields of Wasm plugins (settings pages, meta boxes): one renderer for the form-table
// row of every field type and one parser for the posted value, so a settings page and a meta box
// treat a field the same way.

var pluginColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// pluginFieldRow renders a field as a WordPress form-table row (<tr><th>…</th><td>…</td></tr>).
// name is the input name (and id); v the stored value (nil = the field's default).
func pluginFieldRow(name string, f plugins.SettingsField, v any) string {
	e := html.EscapeString
	text, checked := settingValue(v, f)
	n, label := e(name), e(f.Label)
	desc := ""
	if f.Description != "" {
		desc = "\n<p class=\"description\">" + e(f.Description) + "</p>"
	}
	ph := ""
	if f.Placeholder != "" {
		ph = ` placeholder="` + e(f.Placeholder) + `"`
	}
	th := `<label for="` + n + `">` + label + `</label>`
	var td string
	switch f.Type {
	case "checkbox":
		th = label
		chk := ""
		if checked {
			chk = ` checked="checked"`
		}
		inner := label
		if f.Description != "" {
			inner = e(f.Description)
		}
		td = `<fieldset><legend class="screen-reader-text"><span>` + label + `</span></legend>` + "\n" +
			`<label for="` + n + `"><input name="` + n + `" type="checkbox" id="` + n + `" value="1"` + chk + ` /> ` + inner + `</label></fieldset>`
	case "radio":
		th = label
		var b strings.Builder
		b.WriteString(`<fieldset><legend class="screen-reader-text"><span>` + label + `</span></legend>`)
		for i, o := range f.Options {
			chk := ""
			if o.Value == text {
				chk = ` checked="checked"`
			}
			if i > 0 {
				b.WriteString("<br />")
			}
			b.WriteString("\n" + `<label><input type="radio" name="` + n + `" value="` + e(o.Value) + `"` + chk + ` /> <span>` + e(o.Label) + `</span></label>`)
		}
		b.WriteString("</fieldset>")
		td = b.String() + desc
	case "textarea":
		rows := f.Rows
		if rows <= 0 {
			rows = 5
		}
		td = `<textarea name="` + n + `" id="` + n + `" rows="` + strconv.Itoa(rows) + `" cols="50" class="large-text code"` + ph + `>` + e(text) + `</textarea>` + desc
	case "number":
		mm := ""
		if f.Min != nil {
			mm += ` min="` + strconv.FormatFloat(*f.Min, 'f', -1, 64) + `"`
		}
		if f.Max != nil {
			mm += ` max="` + strconv.FormatFloat(*f.Max, 'f', -1, 64) + `"`
		}
		td = `<input name="` + n + `" type="number" id="` + n + `" value="` + e(text) + `" class="small-text"` + mm + ` step="any"` + ph + ` />` + desc
	case "image":
		td = pluginImageField(n, label, text, ph) + desc
	case "color":
		td = pluginColorField(n, label, text) + desc
	case "select":
		var b strings.Builder
		b.WriteString(`<select name="` + n + `" id="` + n + `">`)
		for _, o := range f.Options {
			sel := ""
			if o.Value == text {
				sel = ` selected="selected"`
			}
			b.WriteString(`<option value="` + e(o.Value) + `"` + sel + `>` + e(o.Label) + `</option>`)
		}
		b.WriteString("</select>")
		td = b.String() + desc
	default: // text, email, url, password, date
		typ, class := f.Type, ` class="regular-text"`
		switch typ {
		case "password":
			text = "" // never printed back; an empty input keeps the stored value
			ph = ` autocomplete="new-password"` + ph
		case "date":
			class = ""
		case "email", "url":
			class = ` class="regular-text code"`
		default:
			typ = "text"
		}
		td = `<input name="` + n + `" type="` + typ + `" id="` + n + `" value="` + e(text) + `"` + class + ph + ` />` + desc
	}
	return "<tr>\n<th scope=\"row\">" + th + "</th>\n<td>\n" + td + "\n</td></tr>\n"
}

// pluginFieldValue parses the posted value of a field. ok is false when nothing should be stored
// (absent, invalid, or an empty password, which keeps the stored one).
func pluginFieldValue(f plugins.SettingsField, raw string, present bool) (any, bool) {
	if f.Type == "checkbox" {
		return present && raw != "" && raw != "0", true
	}
	if !present {
		return nil, false
	}
	switch f.Type {
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, false
		}
		if f.Min != nil && n < *f.Min {
			n = *f.Min
		}
		if f.Max != nil && n > *f.Max {
			n = *f.Max
		}
		return n, true
	case "select", "radio":
		for _, o := range f.Options {
			if o.Value == raw {
				return raw, true
			}
		}
		return nil, false
	case "email":
		raw = strings.TrimSpace(raw)
		if raw != "" && (len(raw) > 254 || !validEmail(raw)) {
			return nil, false
		}
		return raw, true
	case "url":
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return "", true
		}
		clean := wp.CleanURL(raw, "http", "https")
		if clean == "" || len(clean) > 2048 {
			return nil, false
		}
		return clean, true
	case "image":
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return "", true
		}
		clean := pluginImageURL(raw)
		if clean == "" {
			return nil, false
		}
		return clean, true
	case "color":
		raw = strings.TrimSpace(raw)
		if raw != "" && !pluginColorRe.MatchString(raw) {
			return nil, false
		}
		return raw, true
	case "date":
		raw = strings.TrimSpace(raw)
		if raw != "" {
			if _, err := time.Parse("2006-01-02", raw); err != nil {
				return nil, false
			}
		}
		return raw, true
	case "password":
		if raw == "" {
			return nil, false
		}
	case "text":
		raw = strings.NewReplacer("\r", "", "\n", " ").Replace(raw)
	}
	if len(raw) > 64<<10 {
		raw = raw[:64<<10]
	}
	return strings.ReplaceAll(raw, "\r\n", "\n"), true
}

// pluginImageURL accepts the address of an image field: an http(s) URL (what the media library
// hands out) or a site-relative path such as /wp-content/uploads/…; "" when it is neither.
func pluginImageURL(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\") {
		clean := wp.CleanURL(raw)
		if !strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "//") || len(clean) > 2048 {
			return ""
		}
		return clean
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	clean := wp.CleanURL(raw, "http", "https")
	if len(clean) > 2048 {
		return ""
	}
	return clean
}

// pluginImageField is the control of an image field: the address in a URL input (which works
// without JavaScript), a "Select image" button that opens the media library (upload or pick), a
// Remove link and a preview. pluginMediaFieldJS wires the buttons.
func pluginImageField(n, label, value, ph string) string {
	e := html.EscapeString
	hidden := ""
	if value == "" {
		hidden = " hidden"
	}
	return `<div class="pursae-media-field">` +
		`<input name="` + n + `" type="url" id="` + n + `" value="` + e(value) + `" class="regular-text code pursae-media-field__url"` + ph + ` /> ` +
		`<button type="button" class="button pursae-media-field__select hide-if-no-js" data-title="` + label + `">Select image</button> ` +
		`<button type="button" class="button-link button-link-delete pursae-media-field__remove hide-if-no-js"` + hidden + `>Remove</button>` +
		`<p class="pursae-media-field__preview-wrap"` + hidden + `><img class="pursae-media-field__preview" src="` + e(value) + `" alt="" style="display:block;max-width:120px;max-height:120px;margin-top:8px;padding:6px;background:#f0f0f1;border:1px solid #dcdcde;border-radius:4px" /></p>` +
		`</div>`
}

// pluginColorField is the control of a color field. The posted value is a text input, so an empty
// colour stays empty: a bare <input type="color"> cannot be empty, shows black and posts #000000 on
// every save, which turned an untouched optional colour into black. The native picker is a swatch
// without a name (never posted) that pluginColorFieldJS keeps in step with the text, plus a Clear
// link; without JavaScript the text input alone works.
func pluginColorField(n, label, value string) string {
	e := html.EscapeString
	swatch := "#000000"
	if pluginColorRe.MatchString(value) {
		switch h := value[1:]; len(h) {
		case 3:
			swatch = "#" + string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		default: // 6, or 8 with alpha the picker cannot show
			swatch = "#" + h[:6]
		}
		swatch = strings.ToLower(swatch)
	}
	hidden := ""
	if value == "" {
		hidden = " hidden"
	}
	return `<span class="pursae-color-field">` +
		`<input name="` + n + `" type="text" id="` + n + `" value="` + e(value) + `" class="code pursae-color-field__value" size="10" maxlength="9" placeholder="No colour" pattern="#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})" /> ` +
		`<input type="color" class="pursae-color-field__swatch hide-if-no-js" value="` + swatch + `" aria-label="` + label + `" /> ` +
		`<button type="button" class="button-link button-link-delete pursae-color-field__clear hide-if-no-js"` + hidden + `>Clear</button>` +
		`</span>`
}

// pluginFieldsHave reports whether a list of fields holds a field of type typ.
func pluginFieldsHave(fields []plugins.SettingsField, typ string) bool {
	for _, f := range fields {
		if f.Type == typ {
			return true
		}
	}
	return false
}

// pluginFieldsHaveImage reports whether a list of fields holds an image field.
func pluginFieldsHaveImage(fields []plugins.SettingsField) bool {
	return pluginFieldsHave(fields, "image")
}

// pluginColorFieldJS keeps the swatch, the text value and the Clear link of color fields in step
// (plain DOM, delegated, so meta boxes reloaded after a save keep working).
const pluginColorFieldJS = `( function() {
	var re = /^#([0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$/i;
	function wrap( el ) { return el.closest && el.closest( '.pursae-color-field' ); }
	function sync( w ) {
		var v = w.querySelector( '.pursae-color-field__value' ).value.trim(), m = re.exec( v );
		if ( m ) {
			var h = m[1];
			if ( h.length === 3 ) { h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2]; }
			w.querySelector( '.pursae-color-field__swatch' ).value = '#' + h.slice( 0, 6 ).toLowerCase();
		}
		w.querySelector( '.pursae-color-field__clear' ).hidden = ! v;
	}
	document.addEventListener( 'input', function( event ) {
		var t = event.target, w = wrap( t );
		if ( ! w ) { return; }
		if ( t.classList.contains( 'pursae-color-field__swatch' ) ) {
			w.querySelector( '.pursae-color-field__value' ).value = t.value;
		}
		sync( w );
	} );
	document.addEventListener( 'click', function( event ) {
		var t = event.target, w = wrap( t );
		if ( ! w || ! t.classList.contains( 'pursae-color-field__clear' ) ) { return; }
		event.preventDefault();
		w.querySelector( '.pursae-color-field__value' ).value = '';
		sync( w );
	} );
} )();`

// pluginMediaFieldJS wires the image fields of plugin settings pages and meta boxes to the media
// modal. Handlers are delegated, so meta boxes Pursae Editor reloads after a save keep working.
const pluginMediaFieldJS = `jQuery( function( $ ) {
	function sync( $wrap ) {
		var v = $.trim( $wrap.find( '.pursae-media-field__url' ).val() || '' );
		$wrap.find( '.pursae-media-field__preview' ).attr( 'src', v );
		$wrap.find( '.pursae-media-field__preview-wrap, .pursae-media-field__remove' ).prop( 'hidden', ! v );
	}
	$( document ).on( 'click', '.pursae-media-field__select', function( event ) {
		event.preventDefault();
		var $wrap = $( this ).closest( '.pursae-media-field' ), frame = $wrap.data( 'frame' );
		if ( ! frame ) {
			frame = wp.media( { title: $( this ).data( 'title' ) || 'Select image', library: { type: 'image' }, button: { text: 'Use this image' }, multiple: false } );
			frame.on( 'select', function() {
				var a = frame.state().get( 'selection' ).first().toJSON();
				$wrap.find( '.pursae-media-field__url' ).val( a.url ).trigger( 'change' );
			} );
			$wrap.data( 'frame', frame );
		}
		frame.open();
	} );
	$( document ).on( 'click', '.pursae-media-field__remove', function( event ) {
		event.preventDefault();
		var $wrap = $( this ).closest( '.pursae-media-field' );
		$wrap.find( '.pursae-media-field__url' ).val( '' ).trigger( 'change' );
	} );
	$( document ).on( 'change input', '.pursae-media-field__url', function() {
		sync( $( this ).closest( '.pursae-media-field' ) );
	} );
} );`
