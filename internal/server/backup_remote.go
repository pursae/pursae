package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/backupcrypt"
	"wpgo/internal/config"
	"wpgo/internal/gocdn"
)

// Remote storage of Backup Manager: an optional S3-compatible bucket (AWS S3, Cloudflare R2, MinIO,
// Backblaze B2, Wasabi …) that backups are copied to, encrypted on this server first
// (internal/backupcrypt: AES-256-GCM in 64 KiB chunks, key from a passphrase through scrypt), so
// the storage provider never sees a readable archive. Nothing changes while no bucket is set.
//
//   - The archive is read from the backups folder, encrypted and uploaded as one stream: parts of
//     16 MB (more for very large files) go up as a multipart upload, so memory use stays at one part
//     whatever the size of the backup.
//   - Settings live in the backup_manager_remote option; the secret key and the passphrase are sealed
//     with the site secret (never stored in plain text) and never shown again on the screen, in the
//     REST API or by the CLI. PURSAE_BACKUP_S3_* and PURSAE_BACKUP_PASSPHRASE override any field and
//     keep the value out of the database altogether.
//   - The endpoint must be https on a public address: the connection is checked after DNS
//     resolution (no SSRF through a host name pointing inside), redirects are not followed, and
//     link-local / metadata addresses are refused always. PURSAE_BACKUP_S3_ALLOW_PRIVATE=1 (or
//     PURSAE_ALLOW_PRIVATE_ENDPOINTS=1, endpoint_guard.go) lets a
//     self-hosted MinIO on the private network (or plain http there) through; it is an environment
//     setting so that an administrator account alone cannot point the server at internal services.
//   - Retention keeps the newest N encrypted backups under the prefix; restoring downloads and
//     decrypts one into the backups folder and continues with the usual restore screen.

const (
	bmgrOptRemote     = "backup_manager_remote"
	bmgrRemoteSealPfx = "bkr:v1:"
	bmgrRemoteKeepMax = 1000
	bmgrRemoteMinPass = 12
	bmgrRemoteTimeout = 2 * time.Minute
	bmgrTestKeyPrefix = ".pursae-connection-test-"
)

// bmgrRemotePartSize is the multipart part size (a variable so tests can use small parts).
var bmgrRemotePartSize int64 = 16 << 20

// bmgrRemoteSettings is the remote target. SecretKey and Passphrase are plain only in memory.
type bmgrRemoteSettings struct {
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region"`
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix"`
	AccessKey  string `json:"access_key"`
	SecretKey  string `json:"secret_key"`
	PathStyle  bool   `json:"path_style"`
	Passphrase string `json:"passphrase"`
	Auto       bool   `json:"auto"` // upload every new manual and scheduled backup
	Keep       int    `json:"keep"` // newest encrypted backups kept in the bucket; 0 keeps all
}

// bmgrRemoteEnv maps each field to its environment variable (without the PURSAE_ prefix).
var bmgrRemoteEnv = []struct{ Field, Env string }{
	{"endpoint", "BACKUP_S3_ENDPOINT"},
	{"region", "BACKUP_S3_REGION"},
	{"bucket", "BACKUP_S3_BUCKET"},
	{"prefix", "BACKUP_S3_PREFIX"},
	{"access_key", "BACKUP_S3_ACCESS_KEY"},
	{"secret_key", "BACKUP_S3_SECRET_KEY"},
	{"path_style", "BACKUP_S3_PATH_STYLE"},
	{"passphrase", "BACKUP_PASSPHRASE"},
}

// bmgrRemoteLocked is the set of fields an environment variable defines (read-only on the screen).
func bmgrRemoteLocked() map[string]string {
	out := map[string]string{}
	for _, e := range bmgrRemoteEnv {
		if v := config.Env(e.Env); v != "" {
			out[e.Field] = config.EnvName(e.Env)
		}
	}
	return out
}

// bmgrRemoteAllowPrivate reports PURSAE_BACKUP_S3_ALLOW_PRIVATE, or the switch shared by every
// admin-set endpoint (PURSAE_ALLOW_PRIVATE_ENDPOINTS).
func bmgrRemoteAllowPrivate() bool {
	return envTrue(config.Env("BACKUP_S3_ALLOW_PRIVATE")) || privateEndpointsAllowed()
}

func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// bmgrRemoteStored reads the option as stored (secrets sealed), without the environment.
func (s *Server) bmgrRemoteStored() bmgrRemoteSettings {
	m, _ := s.settings.Get(bmgrOptRemote).(map[string]any)
	st := bmgrRemoteSettings{Endpoint: strAny(m["endpoint"]), Region: strAny(m["region"]), Bucket: strAny(m["bucket"]),
		Prefix: strAny(m["prefix"]), AccessKey: strAny(m["access_key"]), SecretKey: strAny(m["secret_key"]),
		Passphrase: strAny(m["passphrase"])}
	st.PathStyle, _ = m["path_style"].(bool)
	st.Auto, _ = m["auto"].(bool)
	switch k := m["keep"].(type) {
	case float64:
		st.Keep = int(k)
	case int:
		st.Keep = k
	case int64:
		st.Keep = int(k)
	}
	return st
}

// bmgrRemote is the effective configuration: stored values, secrets unsealed, environment on top.
func (s *Server) bmgrRemote() bmgrRemoteSettings {
	st := s.bmgrRemoteStored()
	st.SecretKey = s.bmgrRemoteReveal(st.SecretKey)
	st.Passphrase = s.bmgrRemoteReveal(st.Passphrase)
	for _, e := range bmgrRemoteEnv {
		v := config.Env(e.Env)
		if v == "" {
			continue
		}
		switch e.Field {
		case "endpoint":
			st.Endpoint = v
		case "region":
			st.Region = v
		case "bucket":
			st.Bucket = v
		case "prefix":
			st.Prefix = v
		case "access_key":
			st.AccessKey = v
		case "secret_key":
			st.SecretKey = v
		case "path_style":
			st.PathStyle = envTrue(v)
		case "passphrase":
			st.Passphrase = v
		}
	}
	st.Endpoint = strings.TrimRight(strings.TrimSpace(st.Endpoint), "/")
	st.Prefix = bmgrRemoteCleanPrefix(st.Prefix)
	if st.Keep < 0 || st.Keep > bmgrRemoteKeepMax {
		st.Keep = 0
	}
	return st
}

// bmgrRemoteConfigured reports whether a bucket is set up (the feature is off otherwise).
func (st bmgrRemoteSettings) configured() bool {
	return st.Endpoint != "" && st.Bucket != ""
}

// problem is the first reason the settings cannot be used, or nil.
func (st bmgrRemoteSettings) problem() error {
	switch {
	case st.Endpoint == "" || st.Bucket == "":
		return errors.New("remote storage is not set up: enter an endpoint and a bucket")
	case st.AccessKey == "" || st.SecretKey == "":
		return errors.New("enter the access key and the secret key of the bucket")
	case st.Passphrase == "":
		return errors.New("set an encryption passphrase: backups are only uploaded encrypted")
	}
	if err := bmgrRemoteCheckEndpoint(st.Endpoint, bmgrRemoteAllowPrivate()); err != nil {
		return err
	}
	if !bmgrRemoteBucketRe.MatchString(st.Bucket) {
		return errors.New("the bucket name may only contain letters, digits, dots, hyphens and underscores (3–63 characters)")
	}
	return nil
}

var (
	bmgrRemoteBucketRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)
	bmgrRemotePrefixRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,200}$`)
	bmgrRemoteRegionRe = regexp.MustCompile(`^[A-Za-z0-9-]{0,40}$`)
	// bmgrRemoteNameRe is an encrypted archive in the bucket: an archive name plus ".enc".
	bmgrRemoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.zip\.enc$`)
)

// bmgrRemoteCleanPrefix makes "backups", "/backups/" and "backups//" the folder "backups/".
func bmgrRemoteCleanPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return path.Clean(p) + "/"
}

// bmgrRemoteCheckEndpoint validates the endpoint URL before any request: http(s), a host, no user
// information, query or fragment; https unless private addresses are allowed; an IP literal must be
// public (or private with the override, never link-local). Host names are checked again at connect
// time, after DNS resolution.
func bmgrRemoteCheckEndpoint(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("the endpoint must be a URL such as https://s3.eu-central-1.amazonaws.com")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("the endpoint must start with https://")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("the endpoint must not contain a user name, a query or a fragment")
	}
	if u.Scheme == "http" && !allowPrivate {
		return errors.New("the endpoint must use https (plain http is only allowed with " + config.EnvName("BACKUP_S3_ALLOW_PRIVATE") + "=1)")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("the endpoint has an invalid port")
		}
	}
	return endpointHostOK(u, allowPrivate, "BACKUP_S3_ALLOW_PRIVATE")
}

// bmgrRemoteAddrOK decides whether the storage may be reached at ip (endpointAddrOK, with the
// backup storage's own switch named in the refusal).
func bmgrRemoteAddrOK(ip net.IP, allowPrivate bool) error {
	return endpointAddrOK(ip, allowPrivate, "BACKUP_S3_ALLOW_PRIVATE")
}

// bmgrEmbeddedIPv4 returns the IPv4 address carried by a NAT64 or 6to4 address, or nil.
// IPv4-mapped IPv6 (::ffff:0:0/96) is left alone: net.IP's own predicates already unwrap it.
func bmgrEmbeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case ip16[0] == 0x00 && ip16[1] == 0x64 && ip16[2] == 0xff && ip16[3] == 0x9b && isZero(ip16[4:12]): // 64:ff9b::/96
		return net.IP(ip16[12:16])
	case ip16[0] == 0x20 && ip16[1] == 0x02: // 2002::/16
		return net.IP(ip16[2:6])
	}
	return nil
}

// bmgrCGNAT reports 100.64.0.0/10 (carrier-grade NAT). The Azure and Alibaba metadata
// addresses inside it are rejected earlier, by the metadata list.
func bmgrCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 0x40
}

// bmgrRemoteHTTP is the client for the storage: every connection's address is checked after DNS
// resolution, no proxy (it would hide the real destination), no redirects.
func bmgrRemoteHTTP(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second,
		Control: endpointDialControl(func() bool { return allowPrivate }, "BACKUP_S3_ALLOW_PRIVATE")}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    15 * time.Second,
			ResponseHeaderTimeout:  bmgrRemoteTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConnsPerHost:    4,
			IdleConnTimeout:        60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// bmgrRemoteClient is the S3 client for the settings (after problem() returned nil).
func bmgrRemoteClient(st bmgrRemoteSettings) *gocdn.Client {
	region := st.Region
	if region == "" {
		region = "us-east-1"
	}
	return &gocdn.Client{Endpoint: st.Endpoint, Region: region, Bucket: st.Bucket, AccessKey: st.AccessKey, SecretKey: st.SecretKey,
		PathStyle: st.PathStyle, HTTP: bmgrRemoteHTTP(bmgrRemoteAllowPrivate())}
}

// redact removes the credentials and the passphrase from a message before it is shown or logged.
func (st bmgrRemoteSettings) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range []string{st.SecretKey, st.Passphrase, st.AccessKey} {
		if len(secret) >= 4 {
			msg = strings.ReplaceAll(msg, secret, "***")
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// --- sealing ----------------------------------------------------------------------------------

func (s *Server) bmgrRemoteAEAD() cipher.AEAD {
	k := sha256.Sum256([]byte("wpgo-backup-remote|" + s.siteSecret()))
	block, _ := aes.NewCipher(k[:])
	gcm, _ := cipher.NewGCM(block)
	return gcm
}

// bmgrRand is the entropy source for sealed secrets. Tests replace it.
var bmgrRand = rand.Reader

// bmgrRemoteSeal encrypts a secret for the option. An empty value stays empty. Every other value
// is sealed, including one that already begins with the seal prefix: that prefix is how a stored
// value is recognised, so treating it as "already sealed" would write the secret in plain text.
func (s *Server) bmgrRemoteSeal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	gcm := s.bmgrRemoteAEAD()
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(bmgrRand, nonce); err != nil {
		return "", fmt.Errorf("sealing the remote storage secret: %w", err)
	}
	return bmgrRemoteSealPfx + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// bmgrRemoteReveal opens a sealed secret ("" when it does not open: the site secret changed).
func (s *Server) bmgrRemoteReveal(v string) string {
	raw, ok := strings.CutPrefix(v, bmgrRemoteSealPfx)
	if !ok {
		return v
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	gcm := s.bmgrRemoteAEAD()
	if err != nil || len(data) < gcm.NonceSize() {
		return ""
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		slog.Warn("backup remote storage: a stored secret cannot be decrypted (was the server secret changed?)")
		return ""
	}
	return string(plain)
}

// bmgrRemoteSave stores the settings. Empty secret fields keep the stored value; fields set by the
// environment are never written.
func (s *Server) bmgrRemoteSave(ctx context.Context, in bmgrRemoteSettings, clearSecrets bool) error {
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	in.Region = strings.TrimSpace(in.Region)
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.AccessKey = strings.TrimSpace(in.AccessKey)
	in.SecretKey = strings.TrimSpace(in.SecretKey)
	in.Prefix = bmgrRemoteCleanPrefix(in.Prefix)
	if in.Endpoint != "" {
		if err := bmgrRemoteCheckEndpoint(in.Endpoint, bmgrRemoteAllowPrivate()); err != nil {
			return err
		}
	}
	if in.Bucket != "" && !bmgrRemoteBucketRe.MatchString(in.Bucket) {
		return errors.New("the bucket name may only contain letters, digits, dots, hyphens and underscores (3–63 characters)")
	}
	if !bmgrRemotePrefixRe.MatchString(in.Prefix) || strings.Contains(in.Prefix, "..") {
		return errors.New("the prefix may only contain letters, digits, dots, hyphens, underscores and slashes")
	}
	if !bmgrRemoteRegionRe.MatchString(in.Region) {
		return errors.New("the region may only contain letters, digits and hyphens")
	}
	if in.Passphrase != "" && len([]rune(in.Passphrase)) < bmgrRemoteMinPass {
		return fmt.Errorf("the passphrase must be at least %d characters long", bmgrRemoteMinPass)
	}
	if in.Keep < 0 || in.Keep > bmgrRemoteKeepMax {
		return fmt.Errorf("keep between 0 and %d remote backups (0 keeps all)", bmgrRemoteKeepMax)
	}
	old := s.bmgrRemoteStored()
	var err error
	if clearSecrets {
		in.SecretKey, in.Passphrase = "", ""
	} else {
		// An empty field keeps the value already stored (it is sealed). A new value is sealed
		// here and only here, so a stored blob is not sealed a second time.
		if in.SecretKey == "" {
			in.SecretKey = old.SecretKey
		} else if in.SecretKey, err = s.bmgrRemoteSeal(in.SecretKey); err != nil {
			return err
		}
		if in.Passphrase == "" {
			in.Passphrase = old.Passphrase
		} else if in.Passphrase, err = s.bmgrRemoteSeal(in.Passphrase); err != nil {
			return err
		}
	}
	m := map[string]any{"endpoint": in.Endpoint, "region": in.Region, "bucket": in.Bucket, "prefix": in.Prefix,
		"access_key": in.AccessKey, "secret_key": in.SecretKey, "path_style": in.PathStyle, "passphrase": in.Passphrase,
		"auto": in.Auto, "keep": in.Keep}
	stored, _ := s.settings.Get(bmgrOptRemote).(map[string]any)
	for field := range bmgrRemoteLocked() {
		if v, ok := stored[field]; ok {
			m[field] = v
		} else {
			delete(m, field)
		}
	}
	return s.settings.Set(ctx, map[string]any{bmgrOptRemote: m})
}

// --- objects ------------------------------------------------------------------------------------

// bmgrRemoteObject is an encrypted backup in the bucket.
type bmgrRemoteObject struct {
	Name     string    `json:"name"`
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// LocalName is the archive name the object decrypts to.
func (o bmgrRemoteObject) LocalName() string { return strings.TrimSuffix(o.Name, backupcrypt.Suffix) }

// bmgrRemoteList lists the encrypted backups directly under the prefix, newest first.
func bmgrRemoteList(ctx context.Context, cl *gocdn.Client, st bmgrRemoteSettings) ([]bmgrRemoteObject, error) {
	var out []bmgrRemoteObject
	token := ""
	for page := 0; page < 1000; page++ {
		res, err := cl.List(ctx, st.Prefix, "/", token, 1000)
		if err != nil {
			return nil, st.redact(err)
		}
		for _, o := range res.Objects {
			name := strings.TrimPrefix(o.Key, st.Prefix)
			if !bmgrRemoteNameRe.MatchString(name) {
				continue
			}
			out = append(out, bmgrRemoteObject{Name: name, Key: o.Key, Size: o.Size, Modified: o.LastModified})
		}
		if !res.Truncated {
			break
		}
		token = res.NextToken
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.After(out[j].Modified)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// bmgrRemotePutStream uploads everything r yields under key: one PUT when it fits in a part,
// else a multipart upload with one part in memory at a time (each retried on its own; a failed
// upload is aborted so no orphaned parts stay in the bucket). sizeHint (the expected length, 0 when
// unknown) raises the part size for very large files, which S3 limits to 10 000 parts.
func bmgrRemotePutStream(ctx context.Context, cl *gocdn.Client, key string, r io.Reader, sizeHint int64, progress func(int64)) (int64, error) {
	part := bmgrRemotePartSize
	if need := sizeHint/9000 + 1; need > part {
		part = (need + (1<<20 - 1)) &^ (1<<20 - 1)
	}
	part = min(part, gocdn.MaxPartSize)
	buf := make([]byte, part)
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if err := cl.Put(ctx, key, buf[:n], gocdn.PutOptions{ContentType: "application/octet-stream"}); err != nil {
			return 0, err
		}
		if progress != nil {
			progress(int64(n))
		}
		return int64(n), nil
	}
	if err != nil {
		return 0, err
	}
	id, err := cl.CreateMultipartUpload(ctx, key, gocdn.PutOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return 0, err
	}
	abort := func(err error) (int64, error) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = cl.AbortMultipartUpload(actx, key, id)
		return 0, err
	}
	var parts []gocdn.Part
	var total int64
	for num := 1; n > 0; num++ {
		if num > 10000 {
			return abort(errors.New("the backup is too large for a multipart upload"))
		}
		var etag string
		for try := 0; ; try++ {
			etag, err = cl.UploadPart(ctx, key, id, num, buf[:n])
			if err == nil || try >= 2 || ctx.Err() != nil {
				break
			}
			time.Sleep(time.Duration(try+1) * time.Second)
		}
		if err != nil {
			return abort(fmt.Errorf("part %d: %w", num, err))
		}
		parts = append(parts, gocdn.Part{Number: num, ETag: etag})
		total += int64(n)
		if progress != nil {
			progress(total)
		}
		n, err = io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return abort(err)
		}
	}
	if err := cl.CompleteMultipartUpload(ctx, key, id, parts); err != nil {
		return abort(err)
	}
	return total, nil
}

// bmgrRemoteUpload encrypts a local archive and uploads it, then applies the retention. It reports
// progress as a percentage of the archive.
func (s *Server) bmgrRemoteUpload(ctx context.Context, name string, logf func(string, ...any), pct func(int)) (key string, err error) {
	st := s.bmgrRemote()
	if err := st.problem(); err != nil {
		return "", err
	}
	p := s.bmgrPath(name)
	if p == "" {
		return "", fmt.Errorf("backup %s not found", name)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	cl := bmgrRemoteClient(st)
	key = st.Prefix + name + backupcrypt.Suffix
	size := fi.Size() + backupcrypt.Overhead(fi.Size())
	pr, pw := io.Pipe()
	encErr := make(chan error, 1)
	safeGo("backup_remote.bmgrRemoteUpload", func() {
		w, err := backupcrypt.NewWriter(pw, []byte(st.Passphrase))
		if err == nil {
			_, err = io.Copy(w, f)
			if err == nil {
				err = w.Close()
			}
		}
		pw.CloseWithError(err)
		encErr <- err
	})
	start := time.Now()
	n, err := bmgrRemotePutStream(ctx, cl, key, pr, size, func(done int64) {
		if pct != nil && size > 0 {
			pct(int(done * 100 / size))
		}
	})
	pr.CloseWithError(errors.New("upload stopped")) // ends the encrypting goroutine on failure
	eerr := <-encErr                                // joined before f.Close, which runs as this function returns
	if err != nil {
		return "", st.redact(fmt.Errorf("upload to %s failed: %w", st.Bucket, err))
	}
	if eerr != nil {
		// The bytes that were uploaded are not a finished ciphertext. Remove that object; a failed
		// multipart upload was already aborted and must not be deleted here, because that would
		// remove an earlier backup stored under the same key.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = cl.Delete(dctx, key)
		cancel()
		return "", st.redact(fmt.Errorf("upload to %s failed: %w", st.Bucket, eerr))
	}
	if logf != nil {
		logf("Uploaded %s encrypted (%s) to %s/%s in %s", name, bmgrHuman(n), st.Bucket, key, time.Since(start).Round(time.Second))
	}
	if st.Keep > 0 {
		removed, perr := bmgrRemotePrune(ctx, cl, st, st.Keep, key)
		if perr != nil && logf != nil {
			logf("Remote retention: %v", perr)
		}
		for _, r := range removed {
			if logf != nil {
				logf("Deleted the old remote backup %s (keeping the newest %d)", r, st.Keep)
			}
		}
	}
	return key, nil
}

// bmgrRemotePrune deletes the oldest encrypted backups under the prefix so that keep remain.
// protect, when set, is the object just uploaded: S3's LastModified resolution is one second, and
// the random suffix of a backup name is not an upload order, so a tie would otherwise delete the
// backup that was just stored and keep an older one.
func bmgrRemotePrune(ctx context.Context, cl *gocdn.Client, st bmgrRemoteSettings, keep int, protect string) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	list, err := bmgrRemoteList(ctx, cl, st)
	if err != nil {
		return nil, err
	}
	if protect != "" {
		rest := make([]bmgrRemoteObject, 0, len(list))
		var top *bmgrRemoteObject
		for i := range list {
			if top == nil && list[i].Key == protect {
				o := list[i]
				top = &o
				continue
			}
			rest = append(rest, list[i])
		}
		if top != nil {
			list = append([]bmgrRemoteObject{*top}, rest...)
		}
	}
	var removed []string
	var errs []error
	for i := keep; i < len(list); i++ {
		if err := cl.Delete(ctx, list[i].Key); err != nil {
			errs = append(errs, st.redact(err))
			continue
		}
		removed = append(removed, list[i].Name)
	}
	return removed, errors.Join(errs...)
}

// bmgrRemoteFetch downloads and decrypts a remote backup into the backups folder and returns the
// local archive name. An archive of that name already there is used as it is. passOverride
// replaces the configured passphrase (a backup made before the passphrase was changed).
func (s *Server) bmgrRemoteFetch(ctx context.Context, name, passOverride string, logf func(string, ...any), pct func(int)) (string, error) {
	st := s.bmgrRemote()
	if passOverride != "" {
		st.Passphrase = passOverride
	}
	if err := st.problem(); err != nil {
		return "", err
	}
	if !bmgrRemoteNameRe.MatchString(name) {
		return "", errors.New("not an encrypted backup name")
	}
	local := strings.TrimSuffix(name, backupcrypt.Suffix)
	if s.bmgrPath(local) != "" {
		if logf != nil {
			logf("%s is already in the backups folder; using it", local)
		}
		return local, nil
	}
	if err := os.MkdirAll(s.bmgrDir(), 0o700); err != nil {
		return "", err
	}
	cl := bmgrRemoteClient(st)
	body, obj, err := cl.Get(ctx, st.Prefix+name)
	if err != nil {
		if gocdn.IsNotFound(err) {
			return "", fmt.Errorf("%s is not in the bucket", name)
		}
		return "", st.redact(err)
	}
	defer body.Close()
	dst := filepath.Join(s.bmgrDir(), local)
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G703: name matched bmgrRemoteNameRe (no path separators)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(tmp) //nolint:gosec // G703: name matched bmgrRemoteNameRe (no path separators)
		}
	}()
	src := io.Reader(body)
	if pct != nil && obj.Size > 0 {
		src = &bmgrCountingReader{r: body, f: func(n int64) { pct(int(n * 100 / obj.Size)) }}
	}
	dr, err := backupcrypt.NewReader(src, []byte(st.Passphrase))
	if err != nil {
		return "", st.redact(err)
	}
	n, err := io.Copy(out, dr)
	if err != nil {
		return "", st.redact(fmt.Errorf("downloading %s: %w", name, err))
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if _, err := bmgrReadManifest(tmp); err != nil {
		return "", fmt.Errorf("the decrypted file is not a usable backup: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil { //nolint:gosec // G703: name matched bmgrRemoteNameRe (no path separators)
		return "", err
	}
	ok = true
	if logf != nil {
		logf("Downloaded and decrypted %s (%s)", local, bmgrHuman(n))
	}
	return local, nil
}

type bmgrCountingReader struct {
	r io.Reader
	n int64
	f func(int64)
}

func (c *bmgrCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.f(c.n)
	return n, err
}

// bmgrRemoteDelete removes one encrypted backup from the bucket.
func (s *Server) bmgrRemoteDelete(ctx context.Context, name string) error {
	st := s.bmgrRemote()
	if err := st.problem(); err != nil {
		return err
	}
	if !bmgrRemoteNameRe.MatchString(name) {
		return errors.New("not an encrypted backup name")
	}
	return st.redact(bmgrRemoteClient(st).Delete(ctx, st.Prefix+name))
}

// bmgrRemoteTest checks the settings against the bucket: write, list, read back and delete a small
// encrypted test object. It returns what was checked.
func (s *Server) bmgrRemoteTest(ctx context.Context) ([]string, error) {
	st := s.bmgrRemote()
	if err := st.problem(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cl := bmgrRemoteClient(st)
	key := st.Prefix + bmgrTestKeyPrefix + bmgrToken()
	payload := []byte("Pursae Backup Manager connection test " + time.Now().UTC().Format(time.RFC3339))
	var enc bytes.Buffer
	if _, err := backupcrypt.EncryptStream(&enc, bytes.NewReader(payload), []byte(st.Passphrase)); err != nil {
		return nil, err
	}
	var steps []string
	if err := cl.Put(ctx, key, enc.Bytes(), gocdn.PutOptions{ContentType: "application/octet-stream"}); err != nil {
		return steps, st.redact(fmt.Errorf("writing a test object: %w", err))
	}
	steps = append(steps, "write")
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer dcancel()
		_ = cl.Delete(dctx, key)
	}()
	if _, err := cl.List(ctx, st.Prefix, "/", "", 1); err != nil {
		return steps, st.redact(fmt.Errorf("listing the bucket: %w", err))
	}
	steps = append(steps, "list")
	body, _, err := cl.Get(ctx, key)
	if err != nil {
		return steps, st.redact(fmt.Errorf("reading the test object back: %w", err))
	}
	var back bytes.Buffer
	_, err = backupcrypt.DecryptStream(&back, io.LimitReader(body, 1<<20), []byte(st.Passphrase))
	body.Close()
	if err != nil || !bytes.Equal(back.Bytes(), payload) {
		return steps, errors.New("the test object came back changed")
	}
	steps = append(steps, "read")
	if err := cl.Delete(ctx, key); err != nil {
		return steps, st.redact(fmt.Errorf("deleting the test object: %w", err))
	}
	steps = append(steps, "delete")
	return steps, nil
}

// bmgrRemoteAuto reports whether a backup of this kind is uploaded right after it is written.
func (s *Server) bmgrRemoteAuto(kind string) bool {
	if kind != "" && kind != "manual" && kind != bmgrScheduleKind {
		return false // safety snapshots and pre-update backups stay local
	}
	st := s.bmgrRemote()
	return st.Auto && st.configured()
}

// bmgrRemoteAfterBackup uploads a fresh archive inside its job when automatic upload is on. A
// failed upload leaves the local backup in place: it is a warning on the job, recorded in
// RemoteError.
func (s *Server) bmgrRemoteAfterBackup(ctx context.Context, j *bmgrJob, name, kind string) {
	if name == "" || !s.bmgrRemoteAuto(kind) {
		return
	}
	s.bmgrProgress(j, "Uploading to remote storage", 0)
	s.bmgrLog(j, "Encrypting and uploading %s to remote storage", name)
	key, err := s.bmgrRemoteUpload(ctx, name, func(f string, a ...any) { s.bmgrLog(j, f, a...) },
		func(p int) { s.bmgrProgress(j, "Uploading to remote storage", p) })
	s.bmgrUpdate(j, func(j *bmgrJob) {
		if err != nil {
			j.RemoteError = err.Error()
			j.Warnings++
		} else {
			j.Remote = key
		}
	})
	if err != nil {
		s.bmgrLog(j, "WARNING: the backup is stored locally, but the upload failed: %v", err)
		slog.Warn("backup remote upload", "file", name, "err", err)
	}
}
