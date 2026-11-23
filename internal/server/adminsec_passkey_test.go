package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// --- a software authenticator ------------------------------------------------------------------

// fakeAuthenticator is a platform authenticator in software: one ES256 discoverable credential, a
// signature counter, user presence and verification always given. origin is what its "browser"
// writes into clientDataJSON.
type fakeAuthenticator struct {
	t          *testing.T
	origin     string
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	rpID       string
	count      uint32
	synced     bool // backup eligible and backed up, like a synced passkey
}

func newFakeAuthenticator(t *testing.T, origin string) *fakeAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &fakeAuthenticator{t: t, origin: origin, key: key, credID: id}
}

func pkB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pkUnB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("base64url %q: %v", s, err)
	}
	return b
}

func (a *fakeAuthenticator) authData(attested bool) []byte {
	h := sha256.Sum256([]byte(a.rpID))
	flags := byte(0x01 | 0x04) // UP, UV
	if a.synced {
		flags |= 0x08 | 0x10 // BE, BS
	}
	if attested {
		flags |= 0x40 // AT
	}
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, a.count)
	if attested {
		pub, err := a.key.PublicKey.Bytes() // 0x04 || X || Y
		if err != nil {
			a.t.Fatal(err)
		}
		cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
			PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
			Curve:         1, XCoord: pub[1:33], YCoord: pub[33:65]})
		if err != nil {
			a.t.Fatal(err)
		}
		out = append(out, make([]byte, 16)...) // AAGUID
		out = binary.BigEndian.AppendUint16(out, uint16(len(a.credID)))
		out = append(out, a.credID...)
		out = append(out, cose...)
	}
	return out
}

func (a *fakeAuthenticator) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return b
}

// create answers navigator.credentials.create() for the options JSON ({"publicKey": …}).
func (a *fakeAuthenticator) create(options []byte) string {
	a.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string              `json:"challenge"`
			RP        struct{ ID string } `json:"rp"`
			User      struct{ ID string } `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		a.t.Fatalf("creation options: %v: %s", err, options)
	}
	a.rpID, a.userHandle = o.PublicKey.RP.ID, pkUnB64(a.t, o.PublicKey.User.ID)
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(true)})
	if err != nil {
		a.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{"id": pkB64(a.credID), "rawId": pkB64(a.credID), "type": "public-key", "clientExtensionResults": map[string]any{},
		"authenticatorAttachment": "platform",
		"response":                map[string]any{"clientDataJSON": pkB64(a.clientData("webauthn.create", o.PublicKey.Challenge)), "attestationObject": pkB64(att), "transports": []string{"internal"}}})
	return string(out)
}

// get answers navigator.credentials.get() for the options JSON; the counter goes up by one.
func (a *fakeAuthenticator) get(options []byte) string {
	a.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		a.t.Fatalf("request options: %v: %s", err, options)
	}
	if o.PublicKey.RPID != "" && o.PublicKey.RPID != a.rpID {
		a.t.Fatalf("request options for RP %q, the credential belongs to %q", o.PublicKey.RPID, a.rpID)
	}
	if !a.synced { // synced passkeys send 0 every time
		a.count++
	}
	ad := a.authData(false)
	cd := a.clientData("webauthn.get", o.PublicKey.Challenge)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte(nil), ad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{"id": pkB64(a.credID), "rawId": pkB64(a.credID), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": pkB64(cd), "authenticatorData": pkB64(ad), "signature": pkB64(sig), "userHandle": pkB64(a.userHandle)}})
	return string(out)
}

// --- helpers ---------------------------------------------------------------------------------

// pkStart is asStart on a host name: WebAuthn refuses IP addresses as the RP ID, and a local site
// follows the request's local origin, so http://localhost:port is the relying party.
func pkStart(t *testing.T) (lbase string, c *http.Client, srv *Server) {
	t.Helper()
	base, _, srv := asStart(t)
	lbase = strings.Replace(base, "127.0.0.1", "localhost", 1)
	c = asNewClient()
	if code, _, loc := asLogin(t, c, lbase, "/wp-login", "admin", "correct horse"); code != http.StatusFound {
		t.Fatalf("login on localhost: %d %s", code, loc)
	}
	return lbase, c, srv
}

func pkRequest(t *testing.T, c *http.Client, method, u string, form url.Values, header map[string]string) (int, string, *http.Response) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, u, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

// pkRegister adds a passkey of a to the account logged in on c and returns the page after it.
func pkRegister(t *testing.T, c *http.Client, lbase string, a *fakeAuthenticator, name, password string) string {
	t.Helper()
	nonce := formNonce(t, asPage(t, c, lbase+"/wp-admin/admin?page=adminsecgo-account"))
	code, body, _ := pkRequest(t, c, http.MethodPost, lbase+"/wp-admin/admin?page=adminsecgo-account",
		url.Values{"_wpnonce": {nonce}, "adminsec_do": {"passkey_begin"}, "adminsec_password": {password}}, nil)
	if code != http.StatusOK {
		t.Fatalf("passkey_begin: %d %s", code, body)
	}
	var begin struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(body), &begin); err != nil || begin.Token == "" {
		t.Fatalf("passkey_begin: %v %s", err, body)
	}
	_, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_finish"}, "passkey_token": {begin.Token}, "passkey_name": {name},
		"passkey_response": {a.create([]byte(`{"publicKey":` + string(begin.PublicKey) + `}`))}})
	return page
}

// pkSignIn runs a passkey sign-in on the login form with a fresh client: options, then the answer.
func pkSignIn(t *testing.T, lbase string, a *fakeAuthenticator) (*http.Client, int, string, string) {
	t.Helper()
	c := asNewClient()
	code, body, _ := pkRequest(t, c, http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, map[string]string{"Origin": lbase})
	if code != http.StatusOK {
		t.Fatalf("passkey_options: %d %s", code, body)
	}
	code, body, resp := pkRequest(t, c, http.MethodPost, lbase+"/wp-login?action=passkey_login",
		url.Values{"passkey_response": {a.get([]byte(body))}, "redirect_to": {"/wp-admin/"}}, map[string]string{"Origin": lbase})
	return c, code, body, resp.Header.Get("Location")
}

var pkStepRe = regexp.MustCompile(`id="adminsec-passkey-step" data-options="([^"]*)"`)
var pkStepTokenRe = regexp.MustCompile(`name="passkey_token" value="([^"]+)"`)

// pkStep answers the passkey option of the "Verify it's you" page.
func pkStep(t *testing.T, c *http.Client, lbase, page string, a *fakeAuthenticator) (int, string, string) {
	t.Helper()
	m, tm := pkStepRe.FindStringSubmatch(page), pkStepTokenRe.FindStringSubmatch(page)
	if m == nil || tm == nil || m[1] == "" {
		t.Fatalf("no passkey step on the challenge page: %.800s", page)
	}
	return post(t, c, lbase+"/wp-login?action=adminsec_2fa", url.Values{"adminsec_token": {asToken(t, page)}, "provider": {"passkey"},
		"passkey_token": {tm[1]}, "passkey_response": {a.get([]byte(html.UnescapeString(m[1])))}})
}

// --- tests -----------------------------------------------------------------------------------

func TestAdminSecPasskeyRegisterAndSignIn(t *testing.T) {
	lbase, c, srv := pkStart(t)
	ctx := context.Background()

	body := asPage(t, c, lbase+"/wp-admin/admin?page=adminsecgo-account")
	if !strings.Contains(body, `id="adminsec-passkeys-card"`) || !strings.Contains(body, `id="adminsec-passkey-add"`) {
		t.Fatal("no passkey card on Users › Security")
	}
	// On an IP address the site can't be a relying party.
	if b := asPage(t, asNewClient(), strings.Replace(lbase, "localhost", "127.0.0.1", 1)+"/wp-login"); strings.Contains(b, `id="adminsec-passkey"`) {
		t.Error("passkey sign-in offered on an IP address")
	}
	// Lockouts are tested on their own below; the failures here must not lock the test client out.
	asConfigure(t, srv, func(st *asSettings) { st.IPMaxFailures, st.UserMaxFailures = 100, 100 })

	// The current password comes first.
	nonce := formNonce(t, body)
	if code, b, _ := pkRequest(t, c, http.MethodPost, lbase+"/wp-admin/admin?page=adminsecgo-account",
		url.Values{"_wpnonce": {nonce}, "adminsec_do": {"passkey_begin"}, "adminsec_password": {"wrong"}}, nil); code != http.StatusForbidden || !strings.Contains(b, `"password"`) {
		t.Errorf("passkey_begin with a wrong password: %d %s", code, b)
	}
	// And the nonce (CSRF).
	if code, _, _ := pkRequest(t, c, http.MethodPost, lbase+"/wp-admin/admin?page=adminsecgo-account",
		url.Values{"_wpnonce": {"bad"}, "adminsec_do": {"passkey_begin"}, "adminsec_password": {"correct horse"}}, nil); code == http.StatusOK {
		t.Error("passkey_begin without a valid nonce")
	}

	// Registration: discoverable, RP ID from the site's host, no attestation.
	code, b, _ := pkRequest(t, c, http.MethodPost, lbase+"/wp-admin/admin?page=adminsecgo-account",
		url.Values{"_wpnonce": {nonce}, "adminsec_do": {"passkey_begin"}, "adminsec_password": {"correct horse"}}, nil)
	if code != http.StatusOK || !strings.Contains(b, `"id":"localhost"`) || !strings.Contains(b, `"residentKey":"required"`) || !strings.Contains(b, `"attestation":"none"`) {
		t.Fatalf("creation options: %d %s", code, b)
	}
	var begin struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	_ = json.Unmarshal([]byte(b), &begin)
	laptop := newFakeAuthenticator(t, lbase)
	resp := laptop.create([]byte(`{"publicKey":` + string(begin.PublicKey) + `}`))
	_, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_finish"}, "passkey_token": {begin.Token}, "passkey_name": {"  Work\nlaptop  "}, "passkey_response": {resp}})
	if !strings.Contains(page, "Passkey “Work laptop” added") {
		t.Fatalf("passkey_finish: %.600s", page)
	}
	if !strings.Contains(page, "Your backup codes") {
		t.Error("no backup codes with the first passkey")
	}
	// The ceremony is single use.
	if _, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_finish"}, "passkey_token": {begin.Token}, "passkey_response": {resp}}); !strings.Contains(page, "The passkey setup expired") {
		t.Error("registration ceremony replayed")
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = 1 AND name = 'Work laptop'`); n != 1 {
		t.Fatalf("%d passkeys stored", n)
	}
	var handle []byte
	_ = getPool(t).QueryRow(ctx, `SELECT handle FROM adminsec_passkey_users WHERE user_id = 1`).Scan(&handle)
	if len(handle) != 32 || string(handle) == "1" {
		t.Errorf("user handle %x: want 32 random bytes", handle)
	}
	// A response made for another origin is refused.
	evil := newFakeAuthenticator(t, "https://evil.example")
	if page := pkRegister(t, c, lbase, evil, "Evil", "correct horse"); !strings.Contains(page, "The passkey could not be added") {
		t.Error("passkey from another origin registered")
	}
	// The same credential can't be registered twice (the browser is told to exclude it too).
	if page := pkRegister(t, c, lbase, laptop, "Again", "correct horse"); !strings.Contains(page, "already registered") && !strings.Contains(page, "could not be added") {
		t.Errorf("credential registered twice: %.400s", page)
	}
	phone := newFakeAuthenticator(t, lbase)
	phone.synced = true
	if page := pkRegister(t, c, lbase, phone, "Phone", "correct horse"); !strings.Contains(page, "Passkey “Phone” added") || strings.Contains(page, "Your backup codes") {
		t.Fatalf("second passkey: %.400s", page)
	}
	page = asPage(t, c, lbase+"/wp-admin/admin?page=adminsecgo-account")
	if !strings.Contains(page, "Work laptop") || !strings.Contains(page, "Synced") {
		t.Error("passkeys not listed")
	}
	if !strings.Contains(asPage(t, c, lbase+"/wp-admin/users"), "is-passkey") {
		t.Error("Users list doesn't show passkeys")
	}

	// The login form offers it, with the browser's autofill.
	anon := asNewClient()
	login := asPage(t, anon, lbase+"/wp-login")
	if !strings.Contains(login, `id="adminsec-passkey"`) || !strings.Contains(login, `autocomplete="username webauthn"`) || !strings.Contains(login, "adminsec-passkey.js") {
		t.Fatal("no passkey sign-in on the login form")
	}

	// Sign-in with a discoverable credential: no username typed.
	cl, code, _, loc := pkSignIn(t, lbase, laptop)
	if code != http.StatusFound || !pkAdmin(loc) {
		t.Fatalf("passkey sign-in: %d %s", code, loc)
	}
	if code, _, _ := get(t, cl, lbase+"/wp-admin/"); code != http.StatusOK {
		t.Errorf("no session after the passkey sign-in: %d", code)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_log WHERE event = 'login_success' AND message = 'Logged in with passkey.'`); n != 1 {
		t.Errorf("%d passkey logins logged", n)
	}
	var count int64
	_ = getPool(t).QueryRow(ctx, `SELECT sign_count FROM adminsec_passkeys WHERE name = 'Work laptop'`).Scan(&count)
	if count != int64(laptop.count) {
		t.Errorf("sign count %d, authenticator at %d", count, laptop.count)
	}
	if _, _, _, loc := pkSignIn(t, lbase, phone); !pkAdmin(loc) {
		t.Error("synced passkey (counter always 0 is fine too) refused")
	}

	// Replay: the same answer with the same ceremony cookie finds nothing.
	rc := asNewClient()
	_, opts, oresp := pkRequest(t, rc, http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil)
	var cookie string
	for _, ck := range oresp.Cookies() {
		if ck.Name == asPasskeyCookie {
			cookie = ck.Value
			if ck.SameSite != http.SameSiteStrictMode || !ck.HttpOnly {
				t.Error("the ceremony cookie must be SameSite=Strict and HttpOnly")
			}
		}
	}
	answer := laptop.get([]byte(opts))
	hdr := map[string]string{"Cookie": asPasskeyCookie + "=" + cookie}
	if code, _, r := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_login", url.Values{"passkey_response": {answer}}, hdr); code != http.StatusFound {
		t.Fatalf("first answer: %d %s", code, r.Header.Get("Location"))
	}
	if code, b, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_login", url.Values{"passkey_response": {answer}}, hdr); code != http.StatusOK || !strings.Contains(b, "expired") {
		t.Errorf("replayed assertion: %d", code)
	}

	// Login CSRF: without the ceremony cookie, or posted from another site, nothing happens.
	_, opts, _ = pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil)
	if code, b, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_login", url.Values{"passkey_response": {laptop.get([]byte(opts))}}, nil); code != http.StatusOK || !strings.Contains(b, "expired") {
		t.Errorf("assertion without its browser's cookie: %d", code)
	}
	if code, _, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("cross-origin options: %d", code)
	}
	xc := asNewClient()
	_, opts, _ = pkRequest(t, xc, http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil)
	if code, b, _ := pkRequest(t, xc, http.MethodPost, lbase+"/wp-login?action=passkey_login", url.Values{"passkey_response": {laptop.get([]byte(opts))}},
		map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusOK || !strings.Contains(b, "could not be verified") {
		t.Errorf("cross-site passkey sign-in: %d", code)
	}

	// An expired ceremony is refused.
	ec := asNewClient()
	_, opts, _ = pkRequest(t, ec, http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil)
	_, _ = getPool(t).Exec(ctx, `UPDATE adminsec_passkey_ceremonies SET expires_at = now() - interval '1 second' WHERE purpose = 'login'`)
	if _, b, _ := pkRequest(t, ec, http.MethodPost, lbase+"/wp-login?action=passkey_login", url.Values{"passkey_response": {laptop.get([]byte(opts))}}, nil); !strings.Contains(b, "expired") {
		t.Error("expired ceremony accepted")
	}

	// A wrong origin or a bad signature fails, counts as a failed login and gives no detail.
	before := asCount(t, `SELECT count(*) FROM login_attempts WHERE NOT success`)
	laptop.origin = "https://evil.example"
	if _, code, b, _ := pkSignIn(t, lbase, laptop); code != http.StatusOK || !strings.Contains(b, "The passkey could not be verified") {
		t.Errorf("assertion for another origin: %d", code)
	}
	laptop.origin = lbase
	stranger := newFakeAuthenticator(t, lbase)
	stranger.rpID, stranger.userHandle, stranger.credID = laptop.rpID, laptop.userHandle, laptop.credID // the right ids, the wrong key
	if _, code, b, _ := pkSignIn(t, lbase, stranger); code != http.StatusOK || !strings.Contains(b, "The passkey could not be verified") {
		t.Errorf("forged signature: %d", code)
	}
	unknown := newFakeAuthenticator(t, lbase)
	unknown.rpID, unknown.userHandle = laptop.rpID, []byte("no-such-user-handle-0123456789ab")
	if _, code, b, _ := pkSignIn(t, lbase, unknown); code != http.StatusOK || !strings.Contains(b, "The passkey could not be verified") {
		t.Errorf("unknown credential: %d", code)
	}
	if after := asCount(t, `SELECT count(*) FROM login_attempts WHERE NOT success`); after != before+3 {
		t.Errorf("failed passkey sign-ins recorded: %d, want %d", after-before, 3)
	}

	// Clone detection: a counter that goes backwards refuses the sign-in and flags the passkey.
	_ = getPool(t).QueryRow(ctx, `SELECT sign_count FROM adminsec_passkeys WHERE name = 'Work laptop'`).Scan(&count)
	laptop.count = uint32(count) - 1 // the next answer repeats the stored counter
	if _, code, b, _ := pkSignIn(t, lbase, laptop); code != http.StatusOK || !strings.Contains(b, "The passkey could not be verified") {
		t.Errorf("cloned authenticator: %d", code)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkeys WHERE name = 'Work laptop' AND clone_warning`); n != 1 {
		t.Error("possible clone not flagged")
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_log WHERE event = 'passkey_clone_warning' AND severity = 'critical'`); n != 1 {
		t.Error("possible clone not logged")
	}
	laptop.count += 10
	if _, _, _, loc := pkSignIn(t, lbase, laptop); loc != "" {
		t.Error("flagged passkey accepted again")
	}
	if page := asPage(t, c, lbase+"/wp-admin/admin?page=adminsecgo-account"); !strings.Contains(page, "A passkey may have been copied") {
		t.Error("the user is not told about the flagged passkey")
	}

	// Rename and remove (remove needs the password).
	var phoneID string
	_ = getPool(t).QueryRow(ctx, `SELECT id::text FROM adminsec_passkeys WHERE name = 'Phone'`).Scan(&phoneID)
	if _, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_rename"}, "passkey": {phoneID}, "passkey_name": {"Pixel"}}); !strings.Contains(page, "Passkey renamed to “Pixel”") {
		t.Error("rename failed")
	}
	if _, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_delete"}, "passkey": {phoneID}, "adminsec_password": {"wrong"}}); !strings.Contains(page, "Your current password is incorrect") {
		t.Error("passkey removed without the password")
	}
	if _, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_delete"}, "passkey": {phoneID}, "adminsec_password": {"correct horse"}}); !strings.Contains(page, "Passkey “Pixel” removed") {
		t.Error("remove failed")
	}

	// A locked-out IP gets no options and no sign-in.
	pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil)
	var ip string
	_ = getPool(t).QueryRow(ctx, `SELECT ip FROM adminsec_passkey_ceremonies ORDER BY id DESC LIMIT 1`).Scan(&ip)
	_, _ = getPool(t).Exec(ctx, `INSERT INTO adminsec_lockouts (kind, value, until) VALUES ('ip', $1, now() + interval '10 minutes')`, ip)
	if code, b, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil); code != http.StatusTooManyRequests || !strings.Contains(b, "locked") {
		t.Errorf("options for a locked-out IP: %d %s", code, b)
	}
	_, _ = getPool(t).Exec(ctx, `UPDATE adminsec_lockouts SET released = true`)

	// An IP keeps its newest open sign-in ceremonies only: many login page views drop the oldest, they
	// don't switch passkeys off for everyone behind the address.
	_, _ = getPool(t).Exec(ctx, `INSERT INTO adminsec_passkey_ceremonies (token_hash, purpose, ip, session, expires_at)
		SELECT sha256(g::text::bytea), 'login', $1, '{}', now() + interval '5 minutes' FROM generate_series(1, 40) g`, ip)
	if code, b, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil); code != http.StatusOK {
		t.Errorf("options past the per-IP cap: %d %s", code, b)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkey_ceremonies WHERE ip = $1 AND purpose = 'login'`, ip); n != asPasskeyOpenPerIP {
		t.Errorf("%d open ceremonies for one IP, want %d", n, asPasskeyOpenPerIP)
	}
	// The daily job drops expired ceremonies.
	_, _ = getPool(t).Exec(ctx, `UPDATE adminsec_passkey_ceremonies SET expires_at = now() - interval '1 second'`)
	if err := srv.adminsecDaily(ctx); err != nil {
		t.Fatal(err)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkey_ceremonies`); n != 0 {
		t.Errorf("%d expired ceremonies left", n)
	}
}

func TestAdminSecPasskeySecondStepAndSettings(t *testing.T) {
	lbase, c, srv := pkStart(t)
	ctx := context.Background()
	key := newFakeAuthenticator(t, lbase)
	if page := pkRegister(t, c, lbase, key, "Key", "correct horse"); !strings.Contains(page, "added") {
		t.Fatalf("register: %.400s", page)
	}

	// After a password, the passkey is the second step.
	anon := asNewClient()
	code, page, _ := asLogin(t, anon, lbase, "/wp-login", "admin", "correct horse")
	if code != http.StatusOK || !strings.Contains(page, `id="adminsec-passkey-step"`) {
		t.Fatalf("password login of a passkey user: %d %.500s", code, page)
	}
	// A wrong passkey answer (another authenticator) counts as a wrong code.
	other := newFakeAuthenticator(t, lbase)
	other.rpID, other.userHandle, other.credID = key.rpID, key.userHandle, key.credID
	code, page2, _ := pkStep(t, anon, lbase, page, other)
	if code != http.StatusOK || !strings.Contains(page2, "The passkey could not be verified") {
		t.Fatalf("wrong passkey in the second step: %d", code)
	}
	code, _, loc := pkStep(t, anon, lbase, page2, key)
	if code != http.StatusFound || !pkAdmin(loc) {
		t.Fatalf("passkey second step: %d %s", code, loc)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_log WHERE message = 'Logged in with password + passkey.'`); n != 1 {
		t.Error("password + passkey not logged")
	}
	// The challenge and its passkey ceremony are spent.
	if _, b, _ := pkStep(t, anon, lbase, page2, key); !strings.Contains(b, "expired or was used") {
		t.Error("second step replayed")
	}

	// "A passkey sign-in counts as two-step login" off: users with an e-mailed code enter it after the passkey.
	srv.cfg.SMTPHost = "smtp.example.test"
	asEnableMFA(t, c, lbase, "admin@example.test")
	asConfigure(t, srv, func(st *asSettings) { st.PasskeyTwoStep = false })
	cl, code, page, _ := pkSignIn(t, lbase, key)
	if code != http.StatusOK || !strings.Contains(page, "We e-mailed a 6-digit code") || strings.Contains(page, `id="adminsec-use-passkey"`) {
		t.Fatalf("passkey sign-in without two-step credit: %d %.500s", code, page)
	}
	if code, _, _ := post(t, cl, lbase+"/wp-login?action=adminsec_2fa", url.Values{"adminsec_token": {asToken(t, page)}, "provider": {"passkey"}}); code == http.StatusFound {
		t.Error("the passkey counted twice")
	}
	cl, _, page, _ = pkSignIn(t, lbase, key)
	if code, _, loc := post(t, cl, lbase+"/wp-login?action=adminsec_2fa", url.Values{"adminsec_token": {asToken(t, page)}, "provider": {"email"},
		"authcode": {asMailedCode(t, "Your login code")}}); code != http.StatusFound || !pkAdmin(loc) {
		t.Fatalf("passkey + e-mailed code: %d %s", code, loc)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_log WHERE message = 'Logged in with passkey + e-mailed code.'`); n != 1 {
		t.Error("passkey + e-mailed code not logged")
	}

	// Role requirement: a passkey for administrators; the last passkey can't be removed then.
	asConfigure(t, srv, func(st *asSettings) {
		st.PasskeyTwoStep = true
		st.RequireRoles = map[string]string{"administrator": "passkey"}
	})
	var id string
	_ = getPool(t).QueryRow(ctx, `SELECT id::text FROM adminsec_passkeys WHERE user_id = 1`).Scan(&id)
	if _, page, _ := asAccount(t, c, lbase, url.Values{"adminsec_do": {"passkey_delete"}, "passkey": {id}, "adminsec_password": {"correct horse"}}); !strings.Contains(page, "Your role requires two-step login") {
		t.Error("the required passkey was removed")
	}
	// The requirement holds at login: after the password only the passkey (or a backup code), no mailed code.
	pc := asNewClient()
	_, page, _ = asLogin(t, pc, lbase, "/wp-login", "admin", "correct horse")
	if !strings.Contains(page, `id="adminsec-passkey-step"`) || strings.Contains(page, "adminsec-email-me") {
		t.Errorf("a required passkey can be skipped: %.600s", page)
	}
	mails := asCount(t, `SELECT count(*) FROM jobs WHERE name = 'wp_mail'`)
	if _, page, _ := post(t, pc, lbase+"/wp-login?action=adminsec_2fa", url.Values{"adminsec_token": {asToken(t, page)}, "provider": {"email"}, "adminsec_do": {"switch"}}); !strings.Contains(page, `id="adminsec-passkey-step"`) {
		t.Error("switched to e-mailed codes while a passkey is required")
	}
	if asCount(t, `SELECT count(*) FROM jobs WHERE name = 'wp_mail'`) != mails {
		t.Error("a code was mailed while a passkey is required")
	}
	asCreateUser(t, srv, "pkeditor", "administrator", asStrongPass)
	ec := asNewClient()
	if code, _, _ := asLogin(t, ec, lbase, "/wp-login", "pkeditor", asStrongPass); code != http.StatusFound {
		t.Fatal("login of an administrator without a passkey")
	}
	if b := asPage(t, ec, lbase+"/wp-admin/admin?page=adminsecgo-account"); !strings.Contains(b, "Your role requires a passkey") {
		t.Error("passkey requirement not shown")
	}
	if !strings.Contains(asPage(t, c, lbase+"/wp-admin/users"), "Passkey required") {
		t.Error("Users list doesn't flag the unmet passkey requirement")
	}
	// With "counts as two-step login", a passkey also meets an MFA/2FA requirement.
	asConfigure(t, srv, func(st *asSettings) { st.RequireRoles = map[string]string{"administrator": "2fa"} })
	if e := srv.asLoad().satisfies("2fa", &asFactors{Passkeys: 1}); !e {
		t.Error("a passkey doesn't meet 2FA while it counts as two-step login")
	}
	asConfigure(t, srv, func(st *asSettings) { st.PasskeyTwoStep = false })
	if srv.asLoad().satisfies("2fa", &asFactors{Passkeys: 1}) {
		t.Error("a passkey meets 2FA while it only replaces the password")
	}

	// Passkeys switched off: no button, no options, stored passkeys kept but not asked for.
	asConfigure(t, srv, func(st *asSettings) {
		st.Passkeys = false
		st.PasskeyTwoStep = true
		st.RequireRoles = map[string]string{}
	})
	if b := asPage(t, asNewClient(), lbase+"/wp-login"); strings.Contains(b, `id="adminsec-passkey"`) || strings.Contains(b, "webauthn") {
		t.Error("passkey sign-in offered while switched off")
	}
	if code, _, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil); code != http.StatusNotFound {
		t.Errorf("options while off: %d", code)
	}
	if _, page, _ := asLogin(t, asNewClient(), lbase, "/wp-login", "admin", "correct horse"); strings.Contains(page, "adminsec-passkey-step") || !strings.Contains(page, "We e-mailed a 6-digit code") {
		t.Error("passkey asked for while switched off")
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = 1`); n != 1 {
		t.Error("passkeys not kept while switched off")
	}

	// Settings form round trip.
	asConfigure(t, srv, func(st *asSettings) { st.Passkeys = true })
	set := asPage(t, c, lbase+"/wp-admin/admin?page=adminsecgo-settings&tab=twofactor")
	if !strings.Contains(set, `name="passkeys" value="1" checked`) || !strings.Contains(set, `value="passkey"`) {
		t.Error("passkey settings missing")
	}

	// Plugin off: nothing of it.
	asToggle(t, c, lbase, "deactivate")
	if b := asPage(t, asNewClient(), lbase+"/wp-login"); strings.Contains(b, "adminsec-passkey") {
		t.Error("passkey sign-in while the plugin is off")
	}
	if _, b, _ := pkRequest(t, asNewClient(), http.MethodPost, lbase+"/wp-login?action=passkey_options", url.Values{}, nil); strings.Contains(b, "publicKey") {
		t.Error("passkey options while the plugin is off")
	}
	asToggle(t, c, lbase, "activate")

	// Admin reset and the CLI remove passkeys.
	if _, err := AdminSecCLI(ctx, srv.store.Pool, []string{"reset-user", "admin"}); err != nil {
		t.Fatal(err)
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = 1`); n != 0 {
		t.Error("reset-user kept the passkeys")
	}
}

func TestAdminSecPasskeyName(t *testing.T) {
	if got := asPasskeyName("  a\n\tb  ", "x"); got != "a b" {
		t.Errorf("%q", got)
	}
	if got := asPasskeyName("", "Passkey 2"); got != "Passkey 2" {
		t.Errorf("%q", got)
	}
	if got := asPasskeyName(strings.Repeat("ş", 100), ""); len([]rune(got)) != asPasskeyNameMax {
		t.Errorf("%d runes", len([]rune(got)))
	}
}

// pkAdmin reports whether a login redirected to the dashboard.
func pkAdmin(loc string) bool { return strings.HasSuffix(strings.TrimSuffix(loc, "/"), "/wp-admin") }

// TestAdminSecPasskeyCounterRace: the library checks the signature counter against the row read before
// the signature was verified. An answer whose counter is no longer greater than the stored one by the time
// it is recorded (a parallel answer stored a newer counter meanwhile) must be refused and flagged, not
// accepted with the stored counter kept.
func TestAdminSecPasskeyCounterRace(t *testing.T) {
	lbase, c, srv := pkStart(t)
	ctx := context.Background()
	a := newFakeAuthenticator(t, lbase)
	pkRegister(t, c, lbase, a, "Race", "correct horse")
	u, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	keys := srv.asPasskeys(ctx, u.ID)
	if len(keys) != 1 {
		t.Fatalf("%d passkeys", len(keys))
	}
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest(http.MethodPost, "/wp-login", nil)
	answer := func(stale asPasskey, counter uint32) bool {
		cred := stale.credential()
		cred.Authenticator.UpdateCounter(counter) // what the library does with the row it read
		return srv.asPasskeyUsed(gc, u, &stale, &cred)
	}

	stale := keys[0] // read with the registration's counter
	if !answer(stale, 7) {
		t.Fatal("a greater counter was refused")
	}
	if n := asCount(t, `SELECT sign_count FROM adminsec_passkeys WHERE id = $1`, stale.ID); n != 7 {
		t.Fatalf("stored counter %d, want 7", n)
	}
	// A second answer checked against the same stale row: the library sees 5 > 0, but 7 is stored now.
	if answer(stale, 5) {
		t.Error("an answer with an older counter than the stored one was accepted")
	}
	if n := asCount(t, `SELECT count(*) FROM adminsec_passkeys WHERE id = $1 AND clone_warning`, stale.ID); n != 1 {
		t.Error("the counter regression was not flagged")
	}
	if n := asCount(t, `SELECT sign_count FROM adminsec_passkeys WHERE id = $1`, stale.ID); n != 7 {
		t.Errorf("stored counter went to %d", n)
	}
	// Flagged: even a greater counter is refused from now on.
	if answer(stale, 99) {
		t.Error("a flagged passkey was accepted")
	}
}
