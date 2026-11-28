package auth

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Login limits, by what they count:
//
//   - per client network (ClientNet): MaxFailedLogins failures from one network within FailedWindow refuse
//     every further sign-in from it, without checking the password. This is the hard limit; it also bounds
//     the guesses one network makes at any one account.
//   - per account, from every network: an account with MaxFailedLoginsPerLogin or more recent failures is
//     "under attack". That is no lockout (otherwise anyone could lock any account, the owner included, by
//     failing at it from a few addresses): a network that has not signed in to the account recently waits
//     AccountDelay before its password is checked, and gets only MaxFailedPerLoginNetUnderAttack failures at
//     that account instead of the full per-network budget. A network that signed in to the account within
//     KnownNetWindow is not slowed down at all. A correct password from a network that has not failed
//     still signs in.
//   - Application Passwords (REST Basic auth) count in a bucket of their own per client network
//     (BeginAppPasswordAttempt): they are 24 random characters, so guessing them is hopeless, and neither
//     wrong account passwords nor the account's state may refuse a client that holds a valid one.
const (
	// MaxFailedPerLoginNetUnderAttack is how many failures one network gets at an account under attack.
	MaxFailedPerLoginNetUnderAttack = 2
	// KnownNetWindow is how long a successful sign-in makes its network "known" for the account (failed
	// and successful attempts are purged a day after they were made, scheduler.go).
	KnownNetWindow = 24 * time.Hour
	// MaxAccountDelay caps the wait before a password is checked at an account under attack.
	MaxAccountDelay = 3 * time.Second
	// AppPasswordBucket prefixes the ip column of Application Password attempts: their own bucket per
	// client network, apart from sign-ins with account passwords.
	AppPasswordBucket = "app-password "
)

// ClientNet is the network a client address is counted under by the login limits: an IPv4 address on
// its own, an IPv6 address by its /64 prefix. One IPv6 subscriber holds at least a whole /64 and can use
// any address in it, so counting single IPv6 addresses would hand one client 2^64 separate budgets.
// Something that is not an IP address comes back trimmed, as it is.
func ClientNet(ip string) string {
	ip = strings.TrimSpace(ip)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

func interval(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }

// AccountDelay is the wait before a password is checked at an account with failures recent failures
// (threshold or more: under attack): 250 ms per failure past the threshold, at least 500 ms and at most
// MaxAccountDelay.
func AccountDelay(failures, threshold int) time.Duration {
	if threshold < 1 {
		threshold = 1
	}
	d := time.Duration(failures-threshold+1) * 250 * time.Millisecond
	return min(max(d, 500*time.Millisecond), MaxAccountDelay)
}

// Pressure is how hard one account is being guessed at, as seen from one client network.
type Pressure struct {
	Failures    int  // failed sign-ins at the account within the window, from every network (in flight included)
	NetFailures int  // the part of Failures that came from this client network
	KnownNet    bool // this network signed in to the account within KnownNetWindow
}

// LoginPressure counts the recent failures at login (lowercased account login or typed name) within window,
// leaving out the attempt exclude (the caller's own, 0: none) and Application Password checks.
func (m *Manager) LoginPressure(ctx context.Context, ip, login string, window time.Duration, exclude int64) (Pressure, error) {
	var p Pressure
	if strings.TrimSpace(login) == "" {
		return p, nil
	}
	err := m.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE NOT success AND attempted_at > now() - $3::interval),
		count(*) FILTER (WHERE NOT success AND ip = $1 AND attempted_at > now() - $3::interval),
		coalesce(bool_or(success AND ip = $1 AND attempted_at > now() - $6::interval), false)
		FROM login_attempts WHERE lower(login) = lower($2) AND id <> $4 AND NOT starts_with(ip, $5)
		AND attempted_at > now() - greatest($3::interval, $6::interval)`,
		ClientNet(ip), login, interval(window), exclude, AppPasswordBucket, interval(KnownNetWindow)).Scan(&p.Failures, &p.NetFailures, &p.KnownNet)
	return p, err
}

// Attempt is a sign-in attempt BeginLogin stored before its password is checked.
type Attempt struct {
	ID int64
	// Throttled: refused, the caller answers "too many attempts" without checking the password (nothing
	// is stored then).
	Throttled bool
	// Delay is how long the caller waits before checking the password: non-zero while the account is
	// under attack and this network has not signed in to it recently.
	Delay time.Duration
}

// BeginLogin counts a sign-in attempt before its password is checked, so parallel requests cannot all pass
// the limits before any of them is recorded: the attempt is stored as a pending failure first and the counts
// include it (attempts of other requests still in flight count too). It refuses (Throttled) when the client
// network is over MaxFailedLogins, or when the account is under attack and this network already used its
// MaxFailedPerLoginNetUnderAttack failures there; the attempt is then removed again (a refused request is not
// a guess and must not extend the block). Otherwise the caller waits Delay, checks the password and calls
// AttemptSucceeded (or DropAttempt) on success and AttemptFailed on a wrong password.
func (m *Manager) BeginLogin(ctx context.Context, ip, login string) (Attempt, error) {
	net := ClientNet(ip)
	var a Attempt
	if err := m.pool.QueryRow(ctx, `INSERT INTO login_attempts(ip, login, success, pending) VALUES ($1,$2,false,true) RETURNING id`, net, login).Scan(&a.ID); err != nil {
		return Attempt{}, err
	}
	var byNet int
	if err := m.pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE ip = $1 AND NOT success AND attempted_at > now() - $2::interval`,
		net, interval(FailedWindow)).Scan(&byNet); err != nil {
		return a, err
	}
	throttled := byNet > MaxFailedLogins
	if !throttled && login != "" {
		p, err := m.LoginPressure(ctx, ip, login, FailedWindow, a.ID)
		if err != nil {
			return a, err
		}
		if !p.KnownNet && p.Failures >= MaxFailedLoginsPerLogin {
			if p.NetFailures >= MaxFailedPerLoginNetUnderAttack {
				throttled = true
			} else {
				a.Delay = AccountDelay(p.Failures, MaxFailedLoginsPerLogin)
			}
		}
	}
	if throttled {
		_, _ = m.pool.Exec(ctx, `DELETE FROM login_attempts WHERE id = $1`, a.ID)
		return Attempt{Throttled: true}, nil
	}
	return a, nil
}

// BeginAppPasswordAttempt counts an Application Password check (REST Basic auth) before any password is
// hashed, like BeginLogin but in a bucket of its own per client network: at most max failures (in flight
// included) per FailedWindow; max <= 0 means no limit. Wrong account passwords, from this network or any
// other, never refuse it, and its failures never count against the account (LoginPressure leaves them out).
// On success the caller drops the attempt (DropAttempt); on a wrong password it calls AttemptFailed.
func (m *Manager) BeginAppPasswordAttempt(ctx context.Context, ip, login string, max int) (id int64, throttled bool, err error) {
	key := AppPasswordBucket + ClientNet(ip)
	if err = m.pool.QueryRow(ctx, `INSERT INTO login_attempts(ip, login, success, pending) VALUES ($1,$2,false,true) RETURNING id`, key, login).Scan(&id); err != nil {
		return 0, false, err
	}
	if max <= 0 {
		return id, false, nil
	}
	var n int
	if err = m.pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE ip = $1 AND NOT success AND attempted_at > now() - $2::interval`,
		key, interval(FailedWindow)).Scan(&n); err != nil {
		return id, false, err
	}
	if n > max {
		_, _ = m.pool.Exec(ctx, `DELETE FROM login_attempts WHERE id = $1`, id)
		return 0, true, nil
	}
	return id, false, nil
}

// ResetBucket prefixes the ip and login columns of password reset requests stored in login_attempts:
// their own bucket, never counted as sign-ins (they are stored as completed successes, and no sign-in
// network or login carries the prefix). The scheduler purges them with the other attempts.
const ResetBucket = "password-reset "

// AllowResetRequest counts a password reset request for login from ip and reports whether it is within
// max per window, both per client network and per account. Like BeginLogin, the request is stored first
// and the counts include it, so parallel requests (or several processes) cannot all pass; a refused
// request is removed again so it does not extend the block.
func (m *Manager) AllowResetRequest(ctx context.Context, ip, login string, max int, window time.Duration) (bool, error) {
	net, acct := ResetBucket+ClientNet(ip), ResetBucket+strings.ToLower(strings.TrimSpace(login))
	var id int64
	if err := m.pool.QueryRow(ctx, `INSERT INTO login_attempts(ip, login, success, pending) VALUES ($1,$2,true,false) RETURNING id`, net, acct).Scan(&id); err != nil {
		return false, err
	}
	var byNet, byAcct int
	if err := m.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE ip = $1), count(*) FILTER (WHERE login = $2)
		FROM login_attempts WHERE starts_with(ip, $3) AND (ip = $1 OR login = $2) AND attempted_at > now() - $4::interval`,
		net, acct, ResetBucket, interval(window)).Scan(&byNet, &byAcct); err != nil {
		return false, err
	}
	if byNet > max || byAcct > max {
		_, _ = m.pool.Exec(ctx, `DELETE FROM login_attempts WHERE id = $1`, id)
		return false, nil
	}
	return true, nil
}
