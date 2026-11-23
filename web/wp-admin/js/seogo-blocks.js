/* Pursae SEO blocks for Pursae Editor: FAQ, How-to, Breadcrumbs and Table of Contents (category "Pursae SEO").
   FAQ and How-to save plain HTML (the site looks the same with the plugin off); their attributes
   feed the schema graph. Breadcrumbs and the Table of Contents are rendered on the server. */
( function ( wp ) {
	'use strict';
	if ( ! wp || ! wp.blocks || ! wp.element ) { return; }
	var el = wp.element.createElement;
	var be = wp.blockEditor || wp.editor;
	var RichText = be.RichText, useBlockProps = be.useBlockProps;
	var c = wp.components;
	var cats = wp.blocks.getCategories();
	if ( ! cats.some( function ( x ) { return x.slug === 'seogo'; } ) ) {
		wp.blocks.setCategories( cats.concat( [ { slug: 'seogo', title: 'Pursae SEO', icon: 'chart-line' } ] ) );
	}
	function uid( p ) { return p + '-' + Date.now().toString( 36 ) + Math.random().toString( 36 ).slice( 2, 6 ); }
	function update( list, i, key, value ) {
		var copy = list.map( function ( x ) { return Object.assign( {}, x ); } );
		copy[ i ][ key ] = value;
		return copy;
	}

	wp.blocks.registerBlockType( 'seogo/faq', {
		apiVersion: 3,
		title: 'FAQ',
		description: 'Questions and answers with FAQPage schema.',
		category: 'seogo',
		icon: 'editor-help',
		keywords: [ 'faq', 'questions', 'schema', 'seo' ],
		attributes: { questions: { type: 'array', default: [] } },
		edit: function ( props ) {
			var qs = props.attributes.questions;
			var set = function ( v ) { props.setAttributes( { questions: v } ); };
			return el( 'div', useBlockProps( { className: 'seogo-faq' } ),
				qs.map( function ( q, i ) {
					return el( 'div', { key: q.id, className: 'seogo-faq-item' },
						el( RichText, { tagName: 'h3', className: 'seogo-faq-question', value: q.question, placeholder: 'Enter a question', onChange: function ( v ) { set( update( qs, i, 'question', v ) ); } } ),
						el( RichText, { tagName: 'p', className: 'seogo-faq-answer', value: q.answer, placeholder: 'Enter the answer to the question', onChange: function ( v ) { set( update( qs, i, 'answer', v ) ); } } ),
						el( c.Button, { variant: 'link', isDestructive: true, onClick: function () { set( qs.filter( function ( _, j ) { return j !== i; } ) ); } }, 'Remove question' ) );
				} ),
				el( c.Button, { variant: 'secondary', icon: 'plus', onClick: function () { set( qs.concat( [ { id: uid( 'faq-question' ), question: '', answer: '' } ] ) ); } }, 'Add question' ) );
		},
		save: function ( props ) {
			return el( 'div', be.useBlockProps.save( { className: 'seogo-faq' } ),
				props.attributes.questions.map( function ( q ) {
					return el( 'div', { key: q.id, className: 'seogo-faq-item', id: q.id },
						el( RichText.Content, { tagName: 'h3', className: 'seogo-faq-question', value: q.question } ),
						el( RichText.Content, { tagName: 'p', className: 'seogo-faq-answer', value: q.answer } ) );
				} ) );
		}
	} );

	wp.blocks.registerBlockType( 'seogo/howto', {
		apiVersion: 3,
		title: 'How-to',
		description: 'Step-by-step instructions with HowTo schema.',
		category: 'seogo',
		icon: 'editor-ol',
		keywords: [ 'how to', 'howto', 'steps', 'schema', 'seo' ],
		attributes: {
			title: { type: 'string', default: '' },
			description: { type: 'string', default: '' },
			totalTime: { type: 'string', default: '' },
			steps: { type: 'array', default: [] }
		},
		edit: function ( props ) {
			var a = props.attributes;
			var set = function ( v ) { props.setAttributes( { steps: v } ); };
			return el( 'div', useBlockProps( { className: 'seogo-howto' } ),
				el( RichText, { tagName: 'h3', value: a.title, placeholder: 'How-to title', onChange: function ( v ) { props.setAttributes( { title: v } ); } } ),
				el( c.TextControl, { label: 'Total time (ISO 8601, e.g. PT30M)', value: a.totalTime, onChange: function ( v ) { props.setAttributes( { totalTime: v } ); } } ),
				el( RichText, { tagName: 'p', className: 'seogo-howto-description', value: a.description, placeholder: 'Describe the task', onChange: function ( v ) { props.setAttributes( { description: v } ); } } ),
				el( 'ol', { className: 'seogo-howto-steps' }, a.steps.map( function ( s, i ) {
					return el( 'li', { key: s.id, className: 'seogo-howto-step' },
						el( RichText, { tagName: 'strong', className: 'seogo-howto-step-name', value: s.name, placeholder: 'Step title', onChange: function ( v ) { set( update( a.steps, i, 'name', v ) ); } } ),
						el( RichText, { tagName: 'p', className: 'seogo-howto-step-text', value: s.text, placeholder: 'Step description', onChange: function ( v ) { set( update( a.steps, i, 'text', v ) ); } } ),
						el( c.Button, { variant: 'link', isDestructive: true, onClick: function () { set( a.steps.filter( function ( _, j ) { return j !== i; } ) ); } }, 'Remove step' ) );
				} ) ),
				el( c.Button, { variant: 'secondary', icon: 'plus', onClick: function () { set( a.steps.concat( [ { id: uid( 'howto-step' ), name: '', text: '' } ] ) ); } }, 'Add step' ) );
		},
		save: function ( props ) {
			var a = props.attributes;
			return el( 'div', be.useBlockProps.save( { className: 'seogo-howto' } ),
				a.title ? el( RichText.Content, { tagName: 'h3', value: a.title } ) : null,
				a.description ? el( RichText.Content, { tagName: 'p', className: 'seogo-howto-description', value: a.description } ) : null,
				el( 'ol', { className: 'seogo-howto-steps' }, a.steps.map( function ( s ) {
					return el( 'li', { key: s.id, id: s.id, className: 'seogo-howto-step' },
						el( RichText.Content, { tagName: 'strong', className: 'seogo-howto-step-name', value: s.name } ),
						el( RichText.Content, { tagName: 'p', className: 'seogo-howto-step-text', value: s.text } ) );
				} ) ) );
		}
	} );

	wp.blocks.registerBlockType( 'seogo/breadcrumbs', {
		apiVersion: 3,
		title: 'Breadcrumbs',
		description: 'The breadcrumb trail of the current page.',
		category: 'seogo',
		icon: 'menu',
		keywords: [ 'breadcrumbs', 'seo', 'navigation' ],
		attributes: {},
		edit: function () {
			return el( 'nav', useBlockProps( { className: 'seogo-breadcrumb' } ), el( 'p', null, 'Home » Category » This page' ) );
		},
		save: function () { return null; }
	} );
	// Table of Contents: the editor previews the headings of the post; the site renders the list with
	// jump links on the server (empty attributes take the defaults of SEO › General › Table of Contents).
	function headingsOf( list, out ) {
		( list || [] ).forEach( function ( b ) {
			if ( b.name === 'core/heading' ) {
				var d = document.createElement( 'div' );
				d.innerHTML = String( b.attributes.content || '' );
				var t = ( d.textContent || '' ).trim();
				if ( t ) { out.push( { level: b.attributes.level || 2, text: t } ); }
			}
			headingsOf( b.innerBlocks, out );
		} );
		return out;
	}
	// nest turns the flat heading list into a tree by level (deeper headings go under the last shallower one).
	function nest( items, base ) {
		var root = { level: base - 1, children: [] }, stack = [ root ];
		items.forEach( function ( h ) {
			var lvl = Math.max( h.level, base );
			while ( stack.length > 1 && stack[ stack.length - 1 ].level >= lvl ) { stack.pop(); }
			var node = { level: lvl, text: h.text, children: [] };
			stack[ stack.length - 1 ].children.push( node );
			stack.push( node );
		} );
		return root.children;
	}
	wp.blocks.registerBlockType( 'seogo/toc', {
		apiVersion: 3,
		title: 'Table of Contents',
		description: 'Lists the headings of this post with jump links.',
		category: 'seogo',
		icon: 'list-view',
		keywords: [ 'toc', 'table of contents', 'contents', 'headings', 'seo' ],
		supports: { multiple: false, html: false },
		attributes: {
			title: { type: 'string', default: '' },
			levels: { type: 'array', default: [] },
			listStyle: { type: 'string', default: '' },
			collapsible: { type: 'boolean' },
			collapsed: { type: 'boolean', default: false }
		},
		edit: function ( props ) {
			var a = props.attributes;
			var all = wp.data.useSelect( function ( sel ) { return headingsOf( sel( 'core/block-editor' ).getBlocks(), [] ); }, [] );
			var levels = a.levels && a.levels.length ? a.levels : [ 2, 3, 4 ];
			var items = all.filter( function ( h ) { return levels.indexOf( h.level ) >= 0; } );
			var tag = a.listStyle === 'ul' ? 'ul' : 'ol';
			var base = items.reduce( function ( m, h ) { return Math.min( m, h.level ); }, 6 );
			var inspector = el( be.InspectorControls, null,
				el( c.PanelBody, { title: 'Settings' },
					el( c.TextControl, { label: 'Title', value: a.title, placeholder: 'Table of Contents', onChange: function ( v ) { props.setAttributes( { title: v } ); } } ),
					el( 'p', null, el( 'strong', null, 'Headings to list' ) ),
					[ 1, 2, 3, 4, 5, 6 ].map( function ( n ) {
						return el( c.CheckboxControl, { key: n, label: 'H' + n, checked: levels.indexOf( n ) >= 0, onChange: function ( on ) {
							var next = levels.filter( function ( x ) { return x !== n; } );
							if ( on ) { next.push( n ); }
							next.sort();
							props.setAttributes( { levels: next } );
						} } );
					} ),
					el( c.SelectControl, { label: 'List style', value: a.listStyle, options: [
						{ label: 'Default', value: '' }, { label: 'Numbered', value: 'ol' }, { label: 'Bullets', value: 'ul' } ],
						onChange: function ( v ) { props.setAttributes( { listStyle: v } ); } } ),
					el( c.ToggleControl, { label: 'Visitors can collapse it', checked: !! a.collapsible, onChange: function ( v ) { props.setAttributes( { collapsible: v } ); } } ),
					a.collapsible ? el( c.ToggleControl, { label: 'Start collapsed', checked: !! a.collapsed, onChange: function ( v ) { props.setAttributes( { collapsed: v } ); } } ) : null ) );
			return el( wp.element.Fragment, null, inspector,
				el( 'nav', useBlockProps( { className: 'seogo-toc' } ),
					el( 'p', { className: 'seogo-toc-title' }, el( 'strong', null, a.title || 'Table of Contents' ) ),
					items.length ? renderList( nest( items, base ) ) : el( 'p', { style: { opacity: 0.7 } }, 'Add headings to the post; they are listed here.' ) ) );
			function renderList( nodes, sub ) {
				return el( tag, sub ? { key: 'sub' } : { className: 'seogo-toc-list' }, nodes.map( function ( n, i ) {
					return el( 'li', { key: i }, n.text, n.children.length ? renderList( n.children, true ) : null );
				} ) );
			}
		},
		save: function () { return null; }
	} );
}( window.wp ) );
