/* Pursae Security: passkeys (WebAuthn) in the browser (internal/server/adminsec_passkey.go).
 *
 * Three pages wire themselves up by their markup:
 *   - the login form: #adminsec-passkey (button and hidden form) — "Log in with a passkey" and the
 *     browser's passkey autofill on the username field (conditional mediation);
 *   - the "Verify it's you" step: #adminsec-passkey-step (options from the server in data-options);
 *   - Users › Security: #adminsec-passkey-add (registration).
 * Messages shown to the user live in the page as [data-pk-msg] elements, so they are translated
 * with the rest of the page. Nothing here runs without WebAuthn support: the markup stays hidden.
 */
(function () {
	'use strict';
	if (!window.PublicKeyCredential || !navigator.credentials) return;

	function b64u(buf) {
		var bytes = new Uint8Array(buf), s = '';
		for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
		return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
	}
	function unb64u(str) {
		str = String(str).replace(/-/g, '+').replace(/_/g, '/');
		while (str.length % 4) str += '=';
		var bin = atob(str), out = new Uint8Array(bin.length);
		for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
		return out.buffer;
	}
	function descriptors(list) {
		return (list || []).map(function (d) { var o = Object.assign({}, d); o.id = unb64u(d.id); return o; });
	}
	function creationOptions(o) {
		if (PublicKeyCredential.parseCreationOptionsFromJSON) return PublicKeyCredential.parseCreationOptionsFromJSON(o);
		var p = Object.assign({}, o);
		p.challenge = unb64u(o.challenge);
		p.user = Object.assign({}, o.user, { id: unb64u(o.user.id) });
		p.excludeCredentials = descriptors(o.excludeCredentials);
		return p;
	}
	function requestOptions(o) {
		if (PublicKeyCredential.parseRequestOptionsFromJSON) return PublicKeyCredential.parseRequestOptionsFromJSON(o);
		var p = Object.assign({}, o);
		p.challenge = unb64u(o.challenge);
		p.allowCredentials = descriptors(o.allowCredentials);
		return p;
	}
	function credentialJSON(c) {
		if (typeof c.toJSON === 'function') {
			try { return JSON.stringify(c.toJSON()); } catch (e) { /* fall back below */ }
		}
		var r = c.response, out = {
			id: c.id, rawId: b64u(c.rawId), type: c.type, authenticatorAttachment: c.authenticatorAttachment || undefined,
			clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {}, response: { clientDataJSON: b64u(r.clientDataJSON) }
		};
		if (r.attestationObject) {
			out.response.attestationObject = b64u(r.attestationObject);
			if (r.getTransports) out.response.transports = r.getTransports();
		} else {
			out.response.authenticatorData = b64u(r.authenticatorData);
			out.response.signature = b64u(r.signature);
			if (r.userHandle) out.response.userHandle = b64u(r.userHandle);
		}
		return JSON.stringify(out);
	}
	function say(root, key) {
		var box = root.querySelector('[data-pk-status]'), msg = root.querySelector('[data-pk-msg="' + key + '"]');
		if (!box) return;
		box.textContent = msg ? msg.textContent : key;
		box.hidden = false;
	}
	function postForm(url, data) {
		var body = new URLSearchParams();
		Object.keys(data).forEach(function (k) { body.append(k, data[k]); });
		return fetch(url, { method: 'POST', credentials: 'same-origin', headers: { 'Accept': 'application/json' }, body: body })
			.then(function (r) { return r.json().catch(function () { return { error: 'failed' }; }); });
	}
	function cancelled(err) { return err && (err.name === 'NotAllowedError' || err.name === 'AbortError'); }

	// --- the login form ------------------------------------------------------------------------
	var login = document.getElementById('adminsec-passkey');
	if (login) {
		var form = document.getElementById('adminsec-passkey-form'), btn = document.getElementById('adminsec-passkey-btn');
		var optionsURL = login.getAttribute('data-options'), autofill = null, busy = false;
		login.hidden = false;
		var submit = function (cred) {
			form.querySelector('[name="passkey_response"]').value = credentialJSON(cred);
			var remember = document.getElementById('rememberme');
			form.querySelector('[name="rememberme"]').value = remember && remember.checked ? 'forever' : '';
			form.submit();
		};
		var options = function () {
			return postForm(optionsURL, {}).then(function (j) {
				if (!j || !j.publicKey) throw new Error(j && j.error || 'failed');
				return requestOptions(j.publicKey);
			});
		};
		btn.addEventListener('click', function () {
			if (busy) return;
			busy = true;
			if (autofill) { autofill.abort(); autofill = null; }
			options().then(function (pk) { return navigator.credentials.get({ publicKey: pk }); })
				.then(function (cred) { if (cred) submit(cred); else busy = false; })
				.catch(function (err) { busy = false; say(login, cancelled(err) ? 'cancelled' : (err && err.message === 'locked' ? 'locked' : 'failed')); });
		});
		// Passkeys offered in the username field's autofill.
		if (PublicKeyCredential.isConditionalMediationAvailable) {
			PublicKeyCredential.isConditionalMediationAvailable().then(function (ok) {
				if (!ok || busy) return;
				autofill = new AbortController();
				var signal = autofill.signal;
				options().then(function (pk) { return navigator.credentials.get({ publicKey: pk, mediation: 'conditional', signal: signal }); })
					.then(function (cred) { if (cred && !busy) { busy = true; submit(cred); } })
					.catch(function () { /* aborted, dismissed or unavailable: the button still works */ });
			}).catch(function () {});
		}
	}

	// --- the second step of a password login -----------------------------------------------------
	var step = document.getElementById('adminsec-passkey-step');
	if (step) {
		var stepForm = document.getElementById('adminsec-form'), stepBtn = document.getElementById('adminsec-passkey-go');
		var stepOpts = null;
		try { stepOpts = JSON.parse(step.getAttribute('data-options') || 'null'); } catch (e) { stepOpts = null; }
		var unsupported = step.querySelector('[data-pk-unsupported]');
		if (unsupported) unsupported.hidden = true;
		if (stepOpts && stepOpts.publicKey) {
			stepBtn.hidden = false;
			var run = function () {
				stepBtn.disabled = true;
				navigator.credentials.get({ publicKey: requestOptions(stepOpts.publicKey) }).then(function (cred) {
					stepForm.querySelector('[name="passkey_response"]').value = credentialJSON(cred);
					stepForm.submit();
				}).catch(function (err) { stepBtn.disabled = false; say(step, cancelled(err) ? 'cancelled' : 'failed'); });
			};
			stepBtn.addEventListener('click', run);
			if (!step.hasAttribute('data-error')) run(); // browsers that need a click keep the button
		} else {
			say(step, 'failed');
		}
	}

	// --- Users › Security: add a passkey -------------------------------------------------------------
	var add = document.getElementById('adminsec-passkey-add');
	if (add) {
		var addBtn = document.getElementById('adminsec-passkey-add-btn');
		add.querySelectorAll('[data-pk-unsupported]').forEach(function (el) { el.hidden = true; });
		addBtn.disabled = false;
		add.addEventListener('submit', function (ev) {
			if (add.querySelector('[name="passkey_response"]').value) return; // the browser's answer is in: post it
			ev.preventDefault();
			addBtn.disabled = true;
			var pass = add.querySelector('[name="adminsec_password"]');
			postForm(add.getAttribute('action'), { _wpnonce: add.querySelector('[name="_wpnonce"]').value, adminsec_do: 'passkey_begin', adminsec_password: pass.value })
				.then(function (j) {
					if (!j || !j.publicKey) throw new Error(j && j.error || 'failed');
					add.querySelector('[name="passkey_token"]').value = j.token;
					return navigator.credentials.create({ publicKey: creationOptions(j.publicKey) });
				})
				.then(function (cred) {
					add.querySelector('[name="passkey_response"]').value = credentialJSON(cred);
					pass.value = '';
					add.submit();
				})
				.catch(function (err) {
					addBtn.disabled = false;
					var key = cancelled(err) ? 'cancelled' : (err && err.name === 'InvalidStateError' ? 'exists' : (err && err.message) || 'failed');
					say(add, ['password', 'limit', 'unavailable', 'cancelled', 'exists'].indexOf(key) >= 0 ? key : 'failed');
				});
		});
	}
})();
