/* Pursae Commerce checkout (checkout.js): payment method boxes, "ship to a different address", account
   creation fields, live totals through ?goo-ajax=update_order_review and the AJAX order submission
   through ?goo-ajax=checkout (redirect on success, error notices on failure). Without JavaScript
   the form posts to the checkout page. */
(function () {
	'use strict';
	var params = window.goocommerce_params || {};
	var store = window.gooStore;
	if (!store) {
		return;
	}
	var timer = null;
	var xhrSeq = 0;

	function ready(fn) {
		if (document.readyState === 'loading') {
			document.addEventListener('DOMContentLoaded', fn);
		} else {
			fn();
		}
	}

	function form() {
		return document.querySelector('form.checkout');
	}

	function paymentBoxes() {
		var f = form();
		if (!f) {
			return;
		}
		var checked = f.querySelector('input[name="payment_method"]:checked');
		f.querySelectorAll('.payment_box').forEach(function (box) {
			var li = box.closest('li');
			var input = li ? li.querySelector('input[name="payment_method"]') : null;
			box.style.display = input && input === checked ? '' : 'none';
		});
		var btn = f.querySelector('#place_order');
		if (btn && checked) {
			var txt = checked.getAttribute('data-order_button_text');
			btn.textContent = txt || btn.getAttribute('data-value') || 'Place order';
		}
	}

	function shippingToggle() {
		var f = form();
		var cb = f ? f.querySelector('#ship-to-different-address-checkbox') : null;
		var box = f ? f.querySelector('.shipping_address') : null;
		if (box) {
			box.style.display = cb && cb.checked ? '' : 'none';
		}
	}

	function accountToggle() {
		var f = form();
		var cb = f ? f.querySelector('#createaccount') : null;
		var box = f ? f.querySelector('div.create-account') : null;
		if (box && cb) {
			box.style.display = cb.checked ? '' : 'none';
		}
	}

	function updateOrderReview() {
		clearTimeout(timer);
		timer = setTimeout(function () {
			var f = form();
			if (!f) {
				return;
			}
			var seq = ++xhrSeq;
			var review = document.querySelector('#order_review');
			if (review) {
				review.classList.add('processing');
			}
			var data = new URLSearchParams(new FormData(f));
			store.post('update_order_review', { security: params.update_order_review_nonce, post_data: data.toString() }).then(function (res) {
				if (seq !== xhrSeq) {
					return;
				}
				if (review) {
					review.classList.remove('processing');
				}
				if (!res || res.result !== 'success') {
					if (res && res.reload) {
						window.location.reload();
						return;
					}
					if (res && res.messages) {
						store.showNotices(res.messages, f.closest('.woocommerce'));
					}
					return;
				}
				var chosen = (f.querySelector('input[name="payment_method"]:checked') || {}).value;
				var fields = {};
				f.querySelectorAll('.payment_box input, .payment_box select, .payment_box textarea').forEach(function (el) {
					if (el.name) {
						fields[el.name] = el.value;
					}
				});
				store.applyFragments(res.fragments);
				// Keep the chosen method and anything already typed in its fields.
				if (chosen) {
					var again = f.querySelector('input[name="payment_method"][value="' + chosen + '"]');
					if (again) {
						again.checked = true;
					}
				}
				Object.keys(fields).forEach(function (n) {
					var el = f.querySelector('.payment_box [name="' + n + '"]');
					if (el) {
						el.value = fields[n];
					}
				});
				paymentBoxes();
				store.trigger('updated_checkout', res);
			}).catch(function () {
				if (review) {
					review.classList.remove('processing');
				}
			});
		}, 400);
	}

	function submit(e) {
		var f = e.target;
		if (!f.classList || !f.classList.contains('checkout')) {
			return;
		}
		if (e.submitter && e.submitter.name === 'woocommerce_checkout_update_totals') {
			return;
		}
		e.preventDefault();
		if (f.classList.contains('processing')) {
			return;
		}
		f.classList.add('processing');
		var btn = f.querySelector('#place_order');
		if (btn) {
			btn.disabled = true;
		}
		store.post('checkout', new FormData(f)).then(function (res) {
			if (res && res.result === 'success' && res.redirect) {
				window.location = res.redirect;
				return;
			}
			f.classList.remove('processing');
			if (btn) {
				btn.disabled = false;
			}
			if (res && res.reload) {
				window.location.reload();
				return;
			}
			var msg = res && res.messages ? res.messages : '<ul class="woocommerce-error" role="alert"><li>' + (params.i18n_checkout_error || 'Error') + '</li></ul>';
			showCheckoutError(f, msg);
			updateOrderReview();
		}).catch(function () {
			f.classList.remove('processing');
			if (btn) {
				btn.disabled = false;
			}
			showCheckoutError(f, '<ul class="woocommerce-error" role="alert"><li>' + (params.i18n_checkout_error || 'Error') + '</li></ul>');
		});
	}

	function showCheckoutError(f, markup) {
		f.querySelectorAll('.woocommerce-NoticeGroup-checkout').forEach(function (n) { n.remove(); });
		var group = document.createElement('div');
		group.className = 'woocommerce-NoticeGroup woocommerce-NoticeGroup-checkout';
		var el = store.html(markup);
		group.innerHTML = el && el.classList.contains('woocommerce-notices-wrapper') ? el.innerHTML : markup;
		f.insertBefore(group, f.firstChild);
		group.scrollIntoView({ behavior: 'smooth', block: 'start' });
		store.trigger('checkout_error', { messages: markup });
	}

	function inlineValidate(e) {
		var row = e.target.closest('.form-row');
		if (!row) {
			return;
		}
		var v = (e.target.value || '').trim();
		var ok = true;
		if (row.classList.contains('validate-required') && v === '' && e.target.type !== 'checkbox') {
			ok = false;
		}
		if (ok && v !== '' && row.classList.contains('validate-email')) {
			ok = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v);
		}
		row.classList.toggle('woocommerce-invalid', !ok);
		row.classList.toggle('woocommerce-validated', ok);
	}

	ready(function () {
		var f = form();
		if (!f) {
			return;
		}
		paymentBoxes();
		shippingToggle();
		accountToggle();
		f.addEventListener('change', function (e) {
			var t = e.target;
			if (t.name === 'payment_method') {
				paymentBoxes();
				// A method that adds to the total (an installment surcharge) is on the page: the order
				// review must show the total of the method now chosen.
				if (f.querySelector('.goo-pay-installments')) {
					updateOrderReview();
				}
				return;
			}
			if (t.closest('.update_totals_on_change')) {
				updateOrderReview();
				return;
			}
			if (t.id === 'ship-to-different-address-checkbox') {
				shippingToggle();
				updateOrderReview();
				return;
			}
			if (t.id === 'createaccount') {
				accountToggle();
				return;
			}
			if (t.classList.contains('shipping_method') || t.closest('.address-field') || t.classList.contains('country_to_state') || t.classList.contains('state_select') ||
				/(_country|_state|_postcode|_city)$/.test(t.name || '')) {
				updateOrderReview();
			}
		});
		f.addEventListener('focusout', function (e) {
			if (e.target.matches('input, select, textarea')) {
				inlineValidate(e);
			}
		});
		document.addEventListener('submit', submit);
		document.addEventListener('country_to_state_changed', updateOrderReview);

		// The coupon form by AJAX.
		var coupon = document.querySelector('form.checkout_coupon');
		if (coupon) {
			coupon.addEventListener('submit', function (e) {
				e.preventDefault();
				var code = coupon.querySelector('[name="coupon_code"]');
				store.post('apply_coupon', { security: params.apply_coupon_nonce, coupon_code: code ? code.value : '' }).then(function (html) {
					store.showNotices(html, coupon.closest('.woocommerce'));
					coupon.style.display = 'none';
					updateOrderReview();
				});
			});
		}
		document.addEventListener('click', function (e) {
			var rm = e.target.closest('#order_review a.woocommerce-remove-coupon');
			if (!rm) {
				return;
			}
			e.preventDefault();
			store.post('remove_coupon', { security: params.remove_coupon_nonce, coupon: rm.getAttribute('data-coupon') }).then(function (html) {
				store.showNotices(html, f.closest('.woocommerce'));
				updateOrderReview();
			});
		});
	});
})();
