/**
 * Starter: the small-screen menu toggle.
 */
( function() {
	'use strict';
	var nav = document.getElementById( 'site-navigation' ),
		button = nav && nav.querySelector( '.menu-toggle' );
	if ( ! button ) {
		return;
	}
	button.addEventListener( 'click', function() {
		var open = nav.classList.toggle( 'toggled' );
		button.setAttribute( 'aria-expanded', open ? 'true' : 'false' );
	} );
	document.addEventListener( 'keydown', function( e ) {
		if ( 'Escape' === e.key && nav.classList.contains( 'toggled' ) ) {
			nav.classList.remove( 'toggled' );
			button.setAttribute( 'aria-expanded', 'false' );
			button.focus();
		}
	} );
}() );
