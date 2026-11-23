package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// Passkeys (WebAuthn) for Pursae Security, with github.com/go-webauthn/webauthn doing the protocol
// work (attestation and assertion parsing, challenge, origin, RP ID hash, signature, flags and the
// signature counter). Users register any number of named passkeys on Users › Security; a passkey
// signs in on its own from the login form (a discoverable credential, also offered by the browser's
// autofill: conditional mediation), and it is a second factor after a password like the app or the
// e-mailed code. Settings › Two-Factor switches passkeys off and decides whether a passkey sign-in
// counts as two-step login (default) or only replaces the password.
//
// Security notes:
//   - Every ceremony (adminsec_passkey_ceremonies) is single use and expires after 5 minutes: it is
//     deleted when it is answered, before the answer is checked, so a replayed assertion finds nothing.
//   - The relying party is the site: RP ID = the host of the site URL, the only accepted origin is the
//     site URL's origin (on local development sites, the request's local origin, as links do). Sites
//     reached by an IP address can't use passkeys (WebAuthn forbids IP RP IDs).
//   - A passkey sign-in is bound to the browser that asked for its challenge: the ceremony token travels
//     in a SameSite=Strict, HttpOnly cookie, and cross-site POSTs (Origin, Sec-Fetch-Site) are refused,
//     so another site can't log a visitor into an account of its choosing (login CSRF).
//   - No user enumeration: the passkey sign-in takes no username and fails with one message; the
//     second-step options name the user's credentials only after the correct password.
//   - Failed passkey sign-ins count as failed logins (IP and username lockouts, the block list, the log);
//     a client IP holds at most asPasskeyOpenPerIP open sign-in ceremonies (older ones are dropped), on top
//     of the core firewall's per-IP request limit.
//   - Clone detection: a signature counter that does not increase (unless the authenticator always
//     sends 0, as synced passkeys do) refuses the sign-in, flags the passkey and logs a critical event;
//     the flagged passkey stays refused until the user removes it.
//   - The user handle is 32 random bytes (adminsec_passkey_users), not the user id.

const (
	asPasskeyTTL       = 5 * time.Minute
	asPasskeyCookie    = "wpgo_adminsec_pk" //nolint:gosec // G101: a cookie name, not a credential
	asMaxPasskeys      = 20
	asPasskeyOpenPerIP = 30
	asPasskeyNameMax   = 64
)

type asPasskey struct {
	ID             int64
	UserID         int64
	CredentialID   []byte
	PublicKey      []byte
	Name           string
	AttType        string
	AAGUID         []byte
	Transports     []string
	SignCount      int64
	CloneWarning   bool
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
	Created        time.Time
	LastUsed       *time.Time
}

// Synced reports whether the passkey may be copied to the user's other devices (backup eligible).
func (p asPasskey) Synced() bool { return p.BackupEligible }

func (p asPasskey) credential() webauthn.Credential {
	tr := make([]protocol.AuthenticatorTransport, 0, len(p.Transports))
	for _, t := range p.Transports {
		tr = append(tr, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{ID: p.CredentialID, PublicKey: p.PublicKey, AttestationType: p.AttType, Transport: tr,
		Flags:         webauthn.CredentialFlags{UserPresent: true, UserVerified: p.UserVerified, BackupEligible: p.BackupEligible, BackupState: p.BackupState},
		Authenticator: webauthn.Authenticator{AAGUID: p.AAGUID, SignCount: uint32(p.SignCount)}}
}

// asPasskeysOn reports whether passkeys may be used: the plugin is active and the setting is on.
func (s *Server) asPasskeysOn() bool { return s.adminsecOn() && s.asLoad().Passkeys }

// asPasskeys lists the user's passkeys, the newest first.
func (s *Server) asPasskeys(ctx context.Context, userID int64) []asPasskey {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, user_id, credential_id, public_key, name, attestation_type, aaguid, transports, sign_count,
		clone_warning, user_verified, backup_eligible, backup_state, created_at, last_used_at FROM adminsec_passkeys WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []asPasskey
	for rows.Next() {
		var p asPasskey
		if rows.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &p.Name, &p.AttType, &p.AAGUID, &p.Transports, &p.SignCount,
			&p.CloneWarning, &p.UserVerified, &p.BackupEligible, &p.BackupState, &p.Created, &p.LastUsed) == nil {
			out = append(out, p)
		}
	}
	return out
}

// asPasskeyCount counts the user's usable passkeys (flagged clones excluded).
func (s *Server) asPasskeyCount(ctx context.Context, userID int64) int {
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = $1 AND NOT clone_warning`, userID).Scan(&n)
	return n
}

// --- relying party -------------------------------------------------------------------------

var errAsPasskeyRP = errors.New("passkeys need the site to be reached by a domain name")

// asRP is the relying party for this request: RP ID = the site URL's host, origin = its origin. On a
// local development site reached on another local address the request's origin is used, as links do.
func (s *Server) asRP(c *gin.Context) (*webauthn.WebAuthn, string, error) {
	site := s.settings.String("siteurl")
	if site == "" {
		site = s.settings.String("home")
	}
	u, err := url.Parse(localOrigin(c, site))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, "", errAsPasskeyRP
	}
	origin := u.Scheme + "://" + u.Host
	name := s.settings.String("blogname")
	if name == "" {
		name = "Pursae"
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID: u.Hostname(), RPDisplayName: name, RPOrigins: []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: asPasskeyTTL, TimeoutUVD: asPasskeyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: asPasskeyTTL, TimeoutUVD: asPasskeyTTL},
		},
	})
	if err != nil {
		return nil, "", errAsPasskeyRP
	}
	return wa, origin, nil
}

// asPasskeysUsable reports whether passkeys work for this request (on, and the site has a domain).
func (s *Server) asPasskeysUsable(c *gin.Context) bool {
	if !s.asPasskeysOn() {
		return false
	}
	_, _, err := s.asRP(c)
	return err == nil
}

// asSameOrigin refuses cross-site requests: a browser's Sec-Fetch-Site or Origin naming another site.
func asSameOrigin(c *gin.Context, origin string) bool {
	if sf := c.GetHeader("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	if o := c.GetHeader("Origin"); o != "" {
		return strings.EqualFold(strings.TrimRight(o, "/"), origin)
	}
	return true
}

// --- the WebAuthn user -----------------------------------------------------------------------

type asWAUser struct {
	u      *store.User
	handle []byte
	keys   []asPasskey
}

func (w *asWAUser) WebAuthnID() []byte   { return w.handle }
func (w *asWAUser) WebAuthnName() string { return w.u.Login }
func (w *asWAUser) WebAuthnDisplayName() string {
	if w.u.DisplayName != "" {
		return w.u.DisplayName
	}
	return w.u.Login
}

// WebAuthnCredentials are the usable passkeys: a flagged clone is never accepted again.
func (w *asWAUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(w.keys))
	for _, k := range w.keys {
		if !k.CloneWarning {
			out = append(out, k.credential())
		}
	}
	return out
}

func (w *asWAUser) key(credID []byte) *asPasskey {
	for i := range w.keys {
		if bytes.Equal(w.keys[i].CredentialID, credID) {
			return &w.keys[i]
		}
	}
	return nil
}

// asHandle returns the user's WebAuthn user handle, creating it when create is set.
func (s *Server) asHandle(ctx context.Context, userID int64, create bool) ([]byte, error) {
	var h []byte
	err := s.store.Pool.QueryRow(ctx, `SELECT handle FROM adminsec_passkey_users WHERE user_id = $1`, userID).Scan(&h)
	if err == nil || !create || !errors.Is(err, pgx.ErrNoRows) {
		return h, err
	}
	h = make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, err
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO adminsec_passkey_users (user_id, handle) VALUES ($1, $2) ON CONFLICT (user_id) DO NOTHING`, userID, h); err != nil {
		return nil, err
	}
	err = s.store.Pool.QueryRow(ctx, `SELECT handle FROM adminsec_passkey_users WHERE user_id = $1`, userID).Scan(&h)
	return h, err
}

func (s *Server) asWAUserOf(ctx context.Context, u *store.User, create bool) (*asWAUser, error) {
	h, err := s.asHandle(ctx, u.ID, create)
	if err != nil {
		return nil, err
	}
	return &asWAUser{u: u, handle: h, keys: s.asPasskeys(ctx, u.ID)}, nil
}

// asWAUserByHandle finds the account a discoverable credential names.
func (s *Server) asWAUserByHandle(ctx context.Context, handle []byte) (*asWAUser, error) {
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `SELECT user_id FROM adminsec_passkey_users WHERE handle = $1`, handle).Scan(&id); err != nil {
		return nil, err
	}
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &asWAUser{u: u, handle: handle, keys: s.asPasskeys(ctx, id)}, nil
}

// --- ceremonies ------------------------------------------------------------------------------

type asCeremony struct {
	Purpose  string
	UserID   int64 // 0: none (passkey sign-in)
	ParentID int64
	Session  webauthn.SessionData
}

func asPkTokenHash(token string) []byte { return asTokenHash("passkey|" + token) }

func (s *Server) asNewCeremony(c *gin.Context, cer asCeremony) (string, error) {
	raw, err := json.Marshal(cer.Session)
	if err != nil {
		return "", err
	}
	token := auth.RandomToken(32)
	var uid, parent any
	if cer.UserID != 0 {
		uid = cer.UserID
	}
	if cer.ParentID != 0 {
		parent = cer.ParentID
	}
	_, err = s.store.Pool.Exec(c, `INSERT INTO adminsec_passkey_ceremonies (token_hash, purpose, user_id, parent_id, ip, session, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, asPkTokenHash(token), cer.Purpose, uid, parent, clientIP(c), raw, time.Now().Add(asPasskeyTTL))
	return token, err
}

// asTakeCeremony deletes the ceremony and returns it: it is answered once, right or wrong.
func (s *Server) asTakeCeremony(ctx context.Context, token, purpose string) (*asCeremony, error) {
	if token == "" {
		return nil, pgx.ErrNoRows
	}
	cer := &asCeremony{Purpose: purpose}
	var uid, parent *int64
	var raw []byte
	err := s.store.Pool.QueryRow(ctx, `DELETE FROM adminsec_passkey_ceremonies WHERE token_hash = $1 AND purpose = $2 AND expires_at > now()
		RETURNING user_id, parent_id, session`, asPkTokenHash(token), purpose).Scan(&uid, &parent, &raw)
	if err != nil {
		return nil, err
	}
	if uid != nil {
		cer.UserID = *uid
	}
	if parent != nil {
		cer.ParentID = *parent
	}
	if err := json.Unmarshal(raw, &cer.Session); err != nil {
		return nil, err
	}
	return cer, nil
}

// asPasskeyUsed stores what an accepted assertion changed (counter, backup state) and refuses a
// clone: a counter that did not increase flags the passkey for good.
func (s *Server) asPasskeyUsed(c *gin.Context, u *store.User, key *asPasskey, cred *webauthn.Credential) bool {
	flag := func(stored int64) bool {
		_, _ = s.store.Pool.Exec(c, `UPDATE adminsec_passkeys SET clone_warning = true WHERE id = $1`, key.ID)
		s.asAudit(c, asEvent{Event: "passkey_clone_warning", Severity: "critical", User: u, Object: u.Login,
			Message: fmt.Sprintf("Passkey “%s” sent a signature counter that did not increase (%d after %d): it may have been copied. It is refused until it is removed.",
				key.Name, cred.Authenticator.SignCount, stored)})
		return false
	}
	if cred.Authenticator.CloneWarning {
		return flag(key.SignCount)
	}
	// The library compared the counter with the row read before the signature check; another answer may
	// have stored a newer counter since. The counter moves forward only if it is still greater than the
	// stored one (or both are 0: authenticators without a counter), checked and stored in one statement,
	// so two answers with the same or an older counter can't both pass.
	n := int64(cred.Authenticator.SignCount)
	var id int64
	err := s.store.Pool.QueryRow(c, `UPDATE adminsec_passkeys SET sign_count = $2, backup_state = $3, user_verified = user_verified OR $4,
		last_used_at = now() WHERE id = $1 AND NOT clone_warning AND (sign_count < $2 OR (sign_count = 0 AND $2 = 0)) RETURNING id`,
		key.ID, n, cred.Flags.BackupState, cred.Flags.UserVerified).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var stored int64
		var flagged bool
		if s.store.Pool.QueryRow(c, `SELECT sign_count, clone_warning FROM adminsec_passkeys WHERE id = $1`, key.ID).Scan(&stored, &flagged) != nil || flagged {
			return false // removed or already flagged meanwhile
		}
		return flag(stored)
	}
	return err == nil
}

// --- registration (Users › Security) ------------------------------------------------------------

// asPasskeyBegin answers the Security screen's "Add a passkey" (adminsec_do=passkey_begin, nonce
// checked): the creation options as JSON, after the current password.
func (s *Server) asPasskeyBegin(c *gin.Context, u *store.User) {
	fail := func(code int, reason string) { c.JSON(code, gin.H{"error": reason}) }
	wa, _, err := s.asRP(c)
	if !s.asPasskeysOn() || err != nil {
		fail(http.StatusForbidden, "unavailable")
		return
	}
	if !s.checkUserPassword(c.Request.Context(), u, c.PostForm("adminsec_password")) {
		fail(http.StatusForbidden, "password")
		return
	}
	wu, err := s.asWAUserOf(c, u, true)
	if err != nil {
		fail(http.StatusInternalServerError, "failed")
		return
	}
	if len(wu.keys) >= asMaxPasskeys {
		fail(http.StatusConflict, "limit")
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, k := range wu.keys {
		cr := k.credential()
		exclude = append(exclude, cr.Descriptor())
	}
	creation, session, err := wa.BeginRegistration(wu,
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(), UserVerification: protocol.VerificationPreferred}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		fail(http.StatusInternalServerError, "failed")
		return
	}
	token, err := s.asNewCeremony(c, asCeremony{Purpose: "register", UserID: u.ID, Session: *session})
	if err != nil {
		fail(http.StatusInternalServerError, "failed")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"token": token, "publicKey": creation.Response})
}

// asPasskeyName cleans a passkey name: trimmed, one line, at most asPasskeyNameMax characters.
func asPasskeyName(v, fallback string) string {
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		v = fallback
	}
	if utf8.RuneCountInString(v) > asPasskeyNameMax {
		v = string([]rune(v)[:asPasskeyNameMax])
	}
	return v
}

// asPasskeyFinish stores the passkey the browser created (adminsec_do=passkey_finish).
func (s *Server) asPasskeyFinish(c *gin.Context, u *store.User, d *asAccountData) []notice {
	wa, _, err := s.asRP(c)
	if !s.asPasskeysOn() || err != nil {
		return errNotice("Passkeys are not available on this site.")
	}
	cer, err := s.asTakeCeremony(c, c.PostForm("passkey_token"), "register")
	if err != nil || cer.UserID != u.ID {
		return errNotice("The passkey setup expired. Please try again.")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(c.PostForm("passkey_response")))
	if err != nil {
		return errNotice("The passkey could not be added. Please try again.")
	}
	wu, err := s.asWAUserOf(c, u, false)
	if err != nil {
		return errNotice("The passkey setup expired. Please try again.")
	}
	cred, err := wa.CreateCredential(wu, cer.Session, parsed)
	if err != nil {
		return errNotice("The passkey could not be added. Please try again.")
	}
	name := asPasskeyName(c.PostForm("passkey_name"), "Passkey "+strconv.Itoa(len(wu.keys)+1))
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	tx, err := s.store.Pool.Begin(c)
	if err != nil {
		return errNotice(err.Error())
	}
	defer func() { _ = tx.Rollback(c) }()
	n, err := s.asLockPasskeys(c, tx, u.ID)
	if err != nil {
		return errNotice(err.Error())
	}
	if n >= asMaxPasskeys {
		return errNotice(fmt.Sprintf("You can have at most %d passkeys.", asMaxPasskeys))
	}
	tag, err := tx.Exec(c, `INSERT INTO adminsec_passkeys (user_id, credential_id, public_key, name, attestation_type, aaguid, transports,
		sign_count, user_verified, backup_eligible, backup_state) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (credential_id) DO NOTHING`,
		u.ID, cred.ID, cred.PublicKey, name, cred.AttestationType, cred.Authenticator.AAGUID, transports, int64(cred.Authenticator.SignCount),
		cred.Flags.UserVerified, cred.Flags.BackupEligible, cred.Flags.BackupState)
	if err != nil || tag.RowsAffected() != 1 || tx.Commit(c) != nil {
		return errNotice("This passkey is already registered.")
	}
	s.asAudit(c, asEvent{Event: "passkey_added", Severity: "notice", Object: u.Login, Message: "Passkey “" + name + "” added."})
	f := s.asFactors(c, u.ID)
	if len(f.Backup) == 0 || f.backupLeft() == 0 { // a lost passkey must not lock the account
		codes, _ := s.asNewBackupCodes(c, f)
		d.Step, d.NewCodes = "codes", codes
		s.asAudit(c, asEvent{Event: "backup_codes_generated", Object: u.Login})
	}
	return okNotice("Passkey “" + name + "” added. You can now log in with it.")
}

// asLockPasskeys serializes changes to one user's passkeys (the row of adminsec_passkey_users) inside tx
// and returns how many the user has.
func (s *Server) asLockPasskeys(ctx context.Context, tx pgx.Tx, userID int64) (int, error) {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM adminsec_passkey_users WHERE user_id = $1 FOR UPDATE`, userID).Scan(&one); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// asPasskeyRename renames one of the user's passkeys.
func (s *Server) asPasskeyRename(c *gin.Context, u *store.User) []notice {
	id, _ := strconv.ParseInt(c.PostForm("passkey"), 10, 64)
	name := asPasskeyName(c.PostForm("passkey_name"), "")
	if name == "" {
		return errNotice("Please enter a name for the passkey.")
	}
	tag, err := s.store.Pool.Exec(c, `UPDATE adminsec_passkeys SET name = $3 WHERE id = $1 AND user_id = $2`, id, u.ID, name)
	if err != nil || tag.RowsAffected() != 1 {
		return errNotice("The passkey was not found.")
	}
	return okNotice("Passkey renamed to “" + name + "”.")
}

// asPasskeyDelete removes one of the user's passkeys (current password; a passkey the role's
// requirement depends on stays).
func (s *Server) asPasskeyDelete(c *gin.Context, u *store.User, required string) []notice {
	id, _ := strconv.ParseInt(c.PostForm("passkey"), 10, 64)
	var name string
	var flagged bool
	if err := s.store.Pool.QueryRow(c, `SELECT name, clone_warning FROM adminsec_passkeys WHERE id = $1 AND user_id = $2`, id, u.ID).Scan(&name, &flagged); err != nil {
		return errNotice("The passkey was not found.")
	}
	if !s.checkUserPassword(c.Request.Context(), u, c.PostForm("adminsec_password")) {
		return errNotice("Your current password is incorrect.")
	}
	st := s.asLoad()
	tx, err := s.store.Pool.Begin(c)
	if err != nil {
		return errNotice(err.Error())
	}
	defer func() { _ = tx.Rollback(c) }()
	if _, err := s.asLockPasskeys(c, tx, u.ID); err != nil { // parallel removals are checked one after the other
		return errNotice(err.Error())
	}
	if required != "" && !flagged {
		f := s.asFactors(c, u.ID)
		_ = tx.QueryRow(c, `SELECT count(*) FROM adminsec_passkeys WHERE user_id = $1 AND NOT clone_warning`, u.ID).Scan(&f.Passkeys)
		if !st.Passkeys {
			f.Passkeys = 0
		}
		if st.satisfies(required, f) {
			f.Passkeys--
			if !st.satisfies(required, f) {
				return errNotice("Your role requires two-step login, and this is the passkey that provides it: add another passkey or another method first.")
			}
		}
	}
	if tag, err := tx.Exec(c, `DELETE FROM adminsec_passkeys WHERE id = $1 AND user_id = $2`, id, u.ID); err != nil || tag.RowsAffected() != 1 || tx.Commit(c) != nil {
		return errNotice("The passkey was not found.")
	}
	s.asAudit(c, asEvent{Event: "passkey_removed", Severity: "warning", Object: u.Login, Message: "Passkey “" + name + "” removed."})
	if f := s.asFactors(c, u.ID); !f.any() && !f.EmailEnabled && !f.TOTPEnabled && s.asPasskeyCount(c, u.ID) == 0 {
		// The last method is gone: backup codes and remembered devices go with it, as when e-mailed codes are turned off.
		f.Backup, f.BackupAt = nil, nil
		_ = s.asSaveFactors(c, f)
		_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_devices WHERE user_id = $1`, u.ID)
	}
	return okNotice("Passkey “" + name + "” removed.")
}

// --- passkey sign-in (the login form) --------------------------------------------------------------

// asPasskeyCookie sets (or, with maxAge -1, clears) the sign-in ceremony cookie: HttpOnly, SameSite=Strict,
// Secure on HTTPS.
func (s *Server) asPasskeyCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{ //nolint:gosec // G124: Secure is set on https requests
		Name: asPasskeyCookie, Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: s.requestHTTPS(c.Request), SameSite: http.SameSiteStrictMode,
	})
}

// adminsecPasskeyOptions answers loginPath?action=passkey_options (POST from the login form's
// script): the request options of a discoverable sign-in; the ceremony token goes in a cookie.
func (s *Server) adminsecPasskeyOptions(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	wa, origin, err := s.asRP(c)
	if c.Request.Method != http.MethodPost || !s.asPasskeysOn() || err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unavailable"})
		return
	}
	if !asSameOrigin(c, origin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "origin"})
		return
	}
	if msg := s.adminsecPreLogin(c, ""); msg != "" { // the block list and IP lockouts
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "locked"})
		return
	}
	assertion, session, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed"})
		return
	}
	token, err := s.asNewCeremony(c, asCeremony{Purpose: "login", Session: *session})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed"})
		return
	}
	// An IP keeps its newest asPasskeyOpenPerIP sign-in ceremonies: the login page asks for one on every
	// view (autofill), so older ones are dropped rather than refusing everyone behind a shared address.
	_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_passkey_ceremonies WHERE purpose = 'login' AND ip = $1 AND id NOT IN
		(SELECT id FROM adminsec_passkey_ceremonies WHERE purpose = 'login' AND ip = $1 ORDER BY id DESC LIMIT $2)`, clientIP(c), asPasskeyOpenPerIP)
	s.asPasskeyCookie(c, token, int(asPasskeyTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"publicKey": assertion.Response})
}

// adminsecPasskeyLogin is the passkey sign-in's POST (loginPath?action=passkey_login): the browser's
// assertion, checked against the ceremony named by the cookie.
func (s *Server) adminsecPasskeyLogin(c *gin.Context, v loginView) {
	v.RedirectTo = c.PostForm("redirect_to")
	v.Remember = c.PostForm("rememberme") == "forever"
	v.Passkey = s.asPasskeysUsable(c)
	fail := func(msg template.HTML) {
		v.Error = msg
		c.Header("Cache-Control", "no-store")
		s.renderAdminTemplate(c, http.StatusOK, "admin/login", v)
	}
	generic := template.HTML("<strong>Error:</strong> The passkey could not be verified. Try again, or log in with your password.")
	wa, origin, err := s.asRP(c)
	if c.Request.Method != http.MethodPost || !s.asPasskeysOn() || err != nil {
		c.Redirect(http.StatusFound, s.loginPath())
		return
	}
	token := ""
	if ck, err := c.Cookie(asPasskeyCookie); err == nil {
		token = ck
	}
	s.asPasskeyCookie(c, "", -1)
	if !asSameOrigin(c, origin) {
		fail(generic)
		return
	}
	if msg := s.adminsecPreLogin(c, ""); msg != "" {
		fail(msg)
		return
	}
	cer, err := s.asTakeCeremony(c, token, "login")
	if err != nil {
		fail("<strong>Error:</strong> The passkey sign-in expired. Please try again.")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(c.PostForm("passkey_response")))
	if err != nil {
		s.adminsecLoginFailed(c, "", nil, "Passkey sign-in with an unreadable answer.", 0)
		fail(generic)
		return
	}
	var wu *asWAUser
	_, cred, err := wa.ValidatePasskeyLogin(func(_, handle []byte) (webauthn.User, error) {
		u, err := s.asWAUserByHandle(c, handle)
		wu = u
		return u, err
	}, cer.Session, parsed)
	if err != nil || wu == nil {
		var u *store.User
		login := ""
		if wu != nil {
			u, login = wu.u, wu.u.Login
		}
		s.adminsecLoginFailed(c, login, u, "Passkey sign-in failed.", 0)
		fail(generic)
		return
	}
	u := wu.u
	if msg := s.adminsecPreLogin(c, u.Login); msg != "" { // the account's own lockout
		fail(msg)
		return
	}
	key := wu.key(cred.ID)
	if key == nil || !s.asPasskeyUsed(c, u, key, cred) {
		s.adminsecLoginFailed(c, u.Login, u, "Passkey refused: possible clone.", 0)
		fail(generic)
		return
	}
	v.Login = u.Login
	st := s.asLoad()
	f := s.asFactors(c, u.ID)
	if st.PasskeyTwoStep || !(f.EmailEnabled || f.TOTPEnabled) || s.asRememberedDevice(c, u) {
		s.asFinishLogin(c, u, v.Remember, v.RedirectTo, "passkey")
		return
	}
	// The passkey replaced the password only: the second step follows, without the passkey option.
	s.asStartChallenge(c, v, u, f, "login_pk")
}

// --- passkey as the second step of a password login --------------------------------------------

// asPasskeySecondOptions starts the passkey ceremony of a second-step challenge: the options (JSON,
// for the page) and the ceremony token.
func (s *Server) asPasskeySecondOptions(c *gin.Context, ch *asChallenge, u *store.User) (string, string) {
	wa, _, err := s.asRP(c)
	if err != nil {
		return "", ""
	}
	wu, err := s.asWAUserOf(c, u, false)
	if err != nil || len(wu.WebAuthnCredentials()) == 0 {
		return "", ""
	}
	assertion, session, err := wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return "", ""
	}
	// One open passkey ceremony per challenge: a new one replaces the last.
	_, _ = s.store.Pool.Exec(c, `DELETE FROM adminsec_passkey_ceremonies WHERE purpose = 'second' AND parent_id = $1`, ch.ID)
	token, err := s.asNewCeremony(c, asCeremony{Purpose: "second", UserID: u.ID, ParentID: ch.ID, Session: *session})
	if err != nil {
		return "", ""
	}
	raw, _ := json.Marshal(gin.H{"publicKey": assertion.Response})
	return string(raw), token
}

// asCheckPasskeySecond verifies the passkey answer of a second-step challenge.
func (s *Server) asCheckPasskeySecond(c *gin.Context, ch *asChallenge, u *store.User) bool {
	wa, _, err := s.asRP(c)
	if err != nil {
		return false
	}
	cer, err := s.asTakeCeremony(c, c.PostForm("passkey_token"), "second")
	if err != nil || cer.ParentID != ch.ID || cer.UserID != u.ID {
		return false
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(c.PostForm("passkey_response")))
	if err != nil {
		return false
	}
	wu, err := s.asWAUserOf(c, u, false)
	if err != nil {
		return false
	}
	cred, err := wa.ValidateLogin(wu, cer.Session, parsed)
	if err != nil {
		return false
	}
	key := wu.key(cred.ID)
	return key != nil && s.asPasskeyUsed(c, u, key, cred)
}
