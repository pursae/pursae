package server

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
)

// gooCartBlocksEditor registers the Cart and Checkout blocks in the block editor: inspector controls
// for their attributes and a server-side rendered preview. The blocks keep WooCommerce's names and
// save markup (a wrapper div around the inner blocks), so pages imported from WordPress open without
// block validation errors and keep their inner blocks; the inner blocks Pursae does not know are kept
// as they are and not shown.
func (s *Server) gooCartBlocksEditor(c *gin.Context) {
	if !s.gooOn() {
		return
	}
	t := func(m string) string { return s.__(c, m) }
	modes := map[string]string{}
	for opt := range gooCheckoutFieldAttrs {
		modes[opt] = s.gooOptString(opt)
	}
	data, _ := json.Marshal(map[string]any{
		"fieldModes": modes,
		"l": map[string]string{
			"cart": t("Cart"), "checkout": t("Checkout"),
			"cartDesc":     t("The shopping cart, rendered by the store on the server."),
			"checkoutDesc": t("The checkout form, rendered by the store on the server."),
			"sections":     t("Sections"), "headings": t("Headings"), "fields": t("Fields"),
			"crossSells": t("Cross-sells"), "coupon": t("Coupon form"), "emptyMessage": t("Empty cart message"),
			"login": t("Returning customer login"), "notes": t("Order notes"), "returnToCart": t("Return to cart link"),
			"billingTitle": t("Billing details heading"), "additionalTitle": t("Additional information heading"),
			"orderTitle": t("Your order heading"), "headingHelp": t("Leave empty for the default heading."),
			"company": t("Company name field"), "apartment": t("Apartment field"), "phone": t("Phone field"),
			"required":   t("Required"),
			"fieldsHelp": t("Field settings apply to the checkout page of the store and to its validation."),
			"preview":    t("The preview shows your own cart."),
		},
	})
	a := s.pageAssets(c)
	a.Script("wp-blocks", "wp-element", "wp-block-editor", "wp-components", "wp-data", "wp-server-side-render")
	a.AddInline("wp-server-side-render", "( "+gooCartBlocksJS+" )( "+string(data)+" );")
}

const gooCartBlocksJS = `function( D ) {
	if ( ! window.wp || ! wp.blocks ) { return; }
	var el = wp.element.createElement, be = wp.blockEditor, cp = wp.components, SSR = wp.serverSideRender, L = D.l;
	// has reports whether the inner blocks hold a block (unknown ones are core/missing with originalName).
	function has( list, name ) {
		return ( list || [] ).some( function( b ) {
			return b.name === name || ( b.attributes && b.attributes.originalName === name ) || has( b.innerBlocks, name );
		} );
	}
	function common( label ) { return { label: label, __nextHasNoMarginBottom: true, __next40pxDefaultSize: true }; }
	function toggle( props, id, label, fallback ) {
		var v = props.attributes[ id ];
		return el( cp.ToggleControl, Object.assign( common( label ), { key: id, checked: v === undefined ? !! fallback : !! v,
			onChange: function( x ) { var o = {}; o[ id ] = x; props.setAttributes( o ); } } ) );
	}
	function text( props, id, label, help ) {
		return el( cp.TextControl, Object.assign( common( label ), { key: id, help: help, value: props.attributes[ id ] || '',
			onChange: function( x ) { var o = {}; o[ id ] = x; props.setAttributes( o ); } } ) );
	}
	function register( name, o ) {
		if ( wp.blocks.getBlockType( name ) ) { return; }
		var attributes = { align: { type: 'string', default: 'wide' }, className: { type: 'string' } };
		o.attrs.forEach( function( a ) { attributes[ a[ 0 ] ] = { type: a[ 1 ] }; } );
		wp.blocks.registerBlockType( name, {
			apiVersion: 3, title: o.title, description: o.description, category: 'widgets', icon: o.icon,
			keywords: o.keywords, attributes: attributes,
			supports: { html: false, multiple: false, reusable: false, align: [ 'wide', 'full' ] },
			edit: function( props ) {
				var inner = wp.data.useSelect( function( sel ) { return sel( 'core/block-editor' ).getBlocks( props.clientId ); }, [ props.clientId ] );
				var at = {};
				Object.keys( props.attributes ).forEach( function( k ) { if ( k !== 'className' && k !== 'align' ) { at[ k ] = props.attributes[ k ]; } } );
				return el( 'div', be.useBlockProps(),
					el( be.InspectorControls, null, o.panels( props, inner ) ),
					el( 'p', { className: 'description', style: { fontSize: '12px', opacity: 0.7 } }, L.preview ),
					el( SSR, { block: name, attributes: at } ) );
			},
			save: function() {
				return el( 'div', be.useBlockProps.save( { className: o.saveClass } ), el( be.InnerBlocks.Content ) );
			}
		} );
	}
	register( 'woocommerce/cart', {
		title: L.cart, description: L.cartDesc, icon: 'cart', keywords: [ 'cart', 'basket', 'woocommerce' ], saveClass: 'is-loading',
		attrs: [ [ 'showCrossSells', 'boolean' ], [ 'showCouponForm', 'boolean' ], [ 'emptyCartMessage', 'string' ] ],
		panels: function( props, inner ) {
			var filled = has( inner, 'woocommerce/filled-cart-block' ), totals = has( inner, 'woocommerce/cart-totals-block' );
			return el( cp.PanelBody, { title: L.sections },
				toggle( props, 'showCrossSells', L.crossSells, ! filled || has( inner, 'woocommerce/cart-cross-sells-block' ) ),
				toggle( props, 'showCouponForm', L.coupon, ! totals || has( inner, 'woocommerce/cart-order-summary-coupon-form-block' ) ),
				text( props, 'emptyCartMessage', L.emptyMessage ) );
		}
	} );
	var fields = [ [ 'Company', 'company', 'checkout_company_field' ], [ 'Apartment', 'apartment', 'checkout_address_2_field' ], [ 'Phone', 'phone', 'checkout_phone_field' ] ];
	var checkoutAttrs = [ [ 'showLoginReminder', 'boolean' ], [ 'showCouponForm', 'boolean' ], [ 'showOrderNotes', 'boolean' ], [ 'showReturnToCart', 'boolean' ],
		[ 'billingTitle', 'string' ], [ 'additionalTitle', 'string' ], [ 'orderTitle', 'string' ] ];
	fields.forEach( function( f ) { checkoutAttrs.push( [ 'show' + f[ 0 ] + 'Field', 'boolean' ], [ 'require' + f[ 0 ] + 'Field', 'boolean' ] ); } );
	register( 'woocommerce/checkout', {
		title: L.checkout, description: L.checkoutDesc, icon: 'money-alt', keywords: [ 'checkout', 'payment', 'woocommerce' ], saveClass: 'wc-block-checkout is-loading',
		attrs: checkoutAttrs,
		panels: function( props, inner ) {
			var at = props.attributes, hasFields = has( inner, 'woocommerce/checkout-fields-block' ), hasTotals = has( inner, 'woocommerce/checkout-totals-block' );
			var fieldControls = [];
			fields.forEach( function( f ) {
				var mode = D.fieldModes[ f[ 2 ] ] || 'optional', show = 'show' + f[ 0 ] + 'Field', req = 'require' + f[ 0 ] + 'Field';
				var shown = at[ show ] === undefined ? mode !== 'hidden' : at[ show ];
				fieldControls.push( toggle( props, show, L[ f[ 1 ] ], mode !== 'hidden' ) );
				if ( shown ) { fieldControls.push( toggle( props, req, L.required + ': ' + L[ f[ 1 ] ], mode === 'required' ) ); }
			} );
			return [
				el( cp.PanelBody, { key: 's', title: L.sections },
					toggle( props, 'showLoginReminder', L.login, true ),
					toggle( props, 'showCouponForm', L.coupon, ! hasTotals || has( inner, 'woocommerce/checkout-order-summary-coupon-form-block' ) ),
					toggle( props, 'showOrderNotes', L.notes, ! hasFields || has( inner, 'woocommerce/checkout-order-note-block' ) ),
					toggle( props, 'showReturnToCart', L.returnToCart, true ) ),
				el( cp.PanelBody, { key: 'h', title: L.headings, initialOpen: false },
					text( props, 'billingTitle', L.billingTitle, L.headingHelp ),
					text( props, 'additionalTitle', L.additionalTitle, L.headingHelp ),
					text( props, 'orderTitle', L.orderTitle, L.headingHelp ) ),
				el( cp.PanelBody, { key: 'f', title: L.fields, initialOpen: false },
					el( 'p', { key: 'help', className: 'description' }, L.fieldsHelp ), fieldControls )
			];
		}
	} );
}`
