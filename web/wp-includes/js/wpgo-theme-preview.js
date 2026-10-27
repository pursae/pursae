/**
 * Theme packages: live preview of the Customizer settings declared with transport postMessage.
 *
 * The Customizer screen posts { wpgoCustomize: 'setting', id, value } to the preview frame. A
 * package theme's footer prints window.wpgoThemePreview = { settings: { 'mod:<id>': { type,
 * css_var, body_class, default } } } (theme_package.go); each change is applied here without a
 * reload: a css_var setting updates its custom property on <html>, a body_class setting toggles its
 * class on <body> (or swaps a "prefix-{value}" class), and the site title and tagline are updated in
 * place. Everything else reloads the preview, as the Customizer does for transport refresh.
 */
( function() {
	'use strict';

	var cfg = window.wpgoThemePreview || {},
		settings = cfg.settings || {},
		root = document.documentElement,
		body = document.body;

	function truthy( type, value ) {
		if ( 'checkbox' === type ) {
			return '1' === value || 'true' === value;
		}
		return '' !== value && '0' !== value && 'false' !== value;
	}

	function cssValue( type, value ) {
		if ( 'checkbox' === type ) {
			return truthy( type, value ) ? '1' : '0';
		}
		return String( value ).replace( /[{}<>;\\\r\n]/g, '' );
	}

	function classText( value ) {
		return String( value ).toLowerCase().replace( /[^a-z0-9_-]+/g, '-' );
	}

	function swapClass( pattern, value ) {
		var parts = pattern.split( '{value}' ), prefix = parts[ 0 ], suffix = parts[ 1 ] || '', text = classText( value );
		Array.prototype.slice.call( body.classList ).forEach( function( c ) {
			if ( 0 === c.indexOf( prefix ) && ( ! suffix || c.slice( -suffix.length ) === suffix ) ) {
				body.classList.remove( c );
			}
		} );
		if ( text ) {
			body.classList.add( prefix + text + suffix );
		}
	}

	function apply( id, value ) {
		var s = settings[ id ];
		value = null === value || undefined === value ? '' : String( value );
		if ( 'blogname' === id ) {
			document.querySelectorAll( '.site-title a, .site-title' ).forEach( function( el ) {
				if ( ! el.querySelector( 'a' ) ) {
					el.textContent = value;
				}
			} );
			return;
		}
		if ( 'blogdescription' === id ) {
			document.querySelectorAll( '.site-description' ).forEach( function( el ) {
				el.textContent = value;
			} );
			return;
		}
		if ( ! s ) {
			return;
		}
		if ( '' === value && null !== s[ 'default' ] && undefined !== s[ 'default' ] ) {
			value = String( s[ 'default' ] );
		}
		if ( s.css_var ) {
			if ( '' === cssValue( s.type, value ) ) {
				root.style.removeProperty( s.css_var );
			} else {
				root.style.setProperty( s.css_var, cssValue( s.type, value ) );
			}
		}
		if ( s.body_class ) {
			if ( -1 !== s.body_class.indexOf( '{value}' ) ) {
				swapClass( s.body_class, value );
			} else {
				body.classList.toggle( s.body_class, truthy( s.type, value ) );
			}
		}
	}

	window.addEventListener( 'message', function( e ) {
		if ( e.origin !== window.location.origin || ! e.data || 'setting' !== e.data.wpgoCustomize ) {
			return;
		}
		apply( e.data.id, e.data.value );
	} );
}() );
