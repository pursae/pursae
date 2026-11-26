package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/config"
	"wpgo/internal/gocdn"
	"wpgo/internal/store"
)

// GoCDN is a native plugin in the spirit of WP Offload Media + an image CDN + CDN Enabler: it copies
// the media library to S3-compatible object storage (AWS S3, Cloudflare R2, DigitalOcean Spaces,
// Backblaze B2, MinIO or any S3 API), rewrites media URLs to the bucket or a custom CDN domain,
// can remove the local copies, browses and imports bucket contents, serves resized / WebP images
// from /cdn-img/ with signed URLs, rewrites theme and core asset URLs to a CDN, and purges WPGo
// Cache (and optionally Cloudflare) when any of it changes.
//
// Like every native plugin it is off on a fresh install (native_gocdn) and adds nothing while
// off: its routes answer 404, uploads are served from disk as before and no response is touched.
// Settings live in the option gocdn_settings; the secret access key and the Cloudflare token are
// AES-GCM encrypted with the server secret and never sent back to the browser. The settings can
// also come from the environment variable PURSAE_CDN_SETTINGS (JSON with the same keys, like WP
// Offload Media's AS3CF_SETTINGS); keys set there are locked on the settings screen.
// docs/gocdn-features.md is the feature inventory.

const (
	gocdnSlug    = "gocdn"
	gocdnSetting = "native_gocdn"   // the plugin is active
	gocdnOption  = "gocdn_settings" // gocdnSettings as one JSON object
	gocdnEnv     = "PURSAE_CDN_SETTINGS"
	gocdnVersion = "1.0.0"
	gocdnImgPath = "/cdn-img/" // the image service: /cdn-img/<path in uploads>?w=&h=&fit=&q=&fm=&s=
	gocdnNonce   = "gocdn-admin"

	gocdnSealPrefix = "enc:v1:"
)

// gocdnAllowPrivate lets tests reach httptest. Production stays on public addresses.
var gocdnAllowPrivate atomic.Bool

// gocdnHTTP is the client of the public CDN APIs and of public object storage. It refuses
// private addresses at connect time and on every redirect.
var gocdnHTTP = publicClient(120*time.Second, gocdnAllowPrivate.Load, "CDN address points at a private network")

// gocdnStorageHTTP is the client of the object storage (endpoint_guard.go): public addresses, plus
// the private network (a MinIO, Ceph or Garage next to the site) only with
// PURSAE_ALLOW_PRIVATE_ENDPOINTS=1. Link-local and metadata addresses stay refused.
var gocdnStorageHTTP = endpointClient(120*time.Second, func() bool {
	return gocdnAllowPrivate.Load() || privateEndpointsAllowed()
}, privateEndpointsEnv)

// gocdnEnvStorageHTTP is used when PURSAE_CDN_SETTINGS itself names the endpoint: the server's
// operator set it, not an administrator account, so the private network is allowed.
var gocdnEnvStorageHTTP = endpointClient(120*time.Second, func() bool { return true }, privateEndpointsEnv)

// gocdnEnvEndpoint is the endpoint PURSAE_CDN_SETTINGS defines ("" when it defines none).
func gocdnEnvEndpoint() string {
	env, _ := gocdnEnvSettings()
	v, _ := env["endpoint"].(string)
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// gocdnCheckEndpoint is the save-time check of an endpoint entered on the settings screen or sent
// through the options API: an http(s) URL without user information whose host is not on the
// private network (unless allowed) and never link-local or a metadata service. Host names are
// checked again when dialled.
func gocdnCheckEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" {
		return errors.New("the endpoint must be a URL such as https://s3.eu-central-1.amazonaws.com")
	}
	if u.User != nil {
		return errors.New("the endpoint must not contain a user name")
	}
	return endpointHostOK(u, gocdnAllowPrivate.Load() || privateEndpointsAllowed(), privateEndpointsEnv)
}

// gocdnCFBase is Cloudflare's API base (tests point it at a fake).
var gocdnCFBase = "https://api.cloudflare.com/client/v4"

// gocdnSettings are the plugin's options. JSON names follow WP Offload Media's settings keys where
// it has one (so a PURSAE_CDN_SETTINGS made from an AS3CF_SETTINGS needs few changes).
type gocdnSettings struct {
	// Storage
	Provider   string `json:"provider"` // aws, r2, do, b2, minio
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region"`
	Bucket     string `json:"bucket"`
	AccessKey  string `json:"access-key-id"`
	SecretKey  string `json:"secret-access-key"` // sealed at rest
	PathStyle  bool   `json:"use-path-style"`
	Prefix     string `json:"object-prefix"` // key prefix, e.g. "wp-content/uploads/"
	PublicRead bool   `json:"public-read"`   // upload with x-amz-acl: public-read
	CDNDomain  string `json:"delivery-domain"`
	// Uploads: multipart from this size on (MB), SSE-S3 and the storage class of new objects.
	MultipartMB  int    `json:"multipart-threshold-mb"`
	SSE          bool   `json:"server-side-encryption"` // legacy: = SSEType "s3" (x-amz-server-side-encryption: AES256)
	StorageClass string `json:"storage-class"`          // "" = the bucket's default
	// A time-stamped folder in the key of every new object (WP Offload Media's object-versioning).
	ObjectVersioning bool `json:"object-versioning"`
	// Encryption at rest: "" (the bucket's default), "s3" (SSE-S3), "kms" (SSE-KMS, key SSEKMSKey
	// or the account's default aws/s3 key) or "customer" (SSE-C, 256-bit SSECKey, sealed at rest).
	SSEType   string `json:"sse-type"`
	SSEKMSKey string `json:"sse-kms-key-id"` // key ID, alias or ARN
	SSECKey   string `json:"sse-c-key"`      // 64 hex characters or base64 of 32 bytes; sealed

	// Offload
	CopyToBucket  bool   `json:"copy-to-s3"`
	RemoveLocal   bool   `json:"remove-local-file"`
	ServeFromCDN  bool   `json:"serve-from-s3"`  // rewrite media URLs on the site and in the REST API
	LocalDelivery string `json:"local-delivery"` // /wp-content/uploads for removed files: redirect, proxy or signed

	// Image service
	Img           bool   `json:"img-enabled"`
	ImgSign       bool   `json:"img-sign"`
	ImgMaxW       int    `json:"img-max-width"`
	ImgMaxH       int    `json:"img-max-height"`
	ImgQuality    int    `json:"img-quality"`
	ImgWebP       bool   `json:"img-webp"`
	ImgBucket     bool   `json:"img-cache-bucket"` // keep variants in the bucket too
	ImgKey        string `json:"img-key"`          // sealed random HMAC key of the signed URLs
	ImgRewrite    bool   `json:"img-rewrite"`      // serve content images through the service (WebP)
	ImgRewriteQ   int    `json:"img-rewrite-quality"`
	ImgMaxSource  int    `json:"img-max-source-mp"` // megapixels
	ImgAVIF       bool   `json:"img-avif"`          // send AVIF to browsers that accept it (slower to encode)
	ImgMaxFrames  int    `json:"img-max-frames"`    // animated GIFs with more frames are flattened
	ImgCDNURL     string `json:"img-cdn-url"`       // origin of a CDN pull zone in front of the site: content image URLs are written to it
	ImgCacheMaxMB int    `json:"img-cache-max-mb"`  // size limit of the processed image cache (0 = unlimited)

	// Assets CDN
	Assets        bool   `json:"assets-enabled"`
	AssetsURL     string `json:"assets-url"`
	AssetsDirs    string `json:"assets-dirs"`       // one path prefix per line
	AssetsExt     string `json:"assets-extensions"` // comma separated
	AssetsExclude string `json:"assets-exclude"`    // one substring per line

	// Purge: one CDN provider (gocdn_purge.go). Cloudflare keeps its own fields (the first
	// version had only Cloudflare); the others share zone / key ID / secret.
	PurgeProvider string `json:"purge-provider"` // "", cloudflare, cloudfront, bunny, keycdn, fastly
	CF            bool   `json:"cloudflare"`     // = PurgeProvider == "cloudflare"
	CFZone        string `json:"cloudflare-zone"`
	CFToken       string `json:"cloudflare-token"`    // sealed
	CFAuto        bool   `json:"cloudflare-auto"`     // purge the CDN with every automatic purge of GoCDN (any provider)
	PurgeZone     string `json:"purge-zone"`          // CloudFront distribution ID, Bunny pull zone ID, KeyCDN zone ID, Fastly service ID
	PurgeKeyID    string `json:"purge-access-key-id"` // CloudFront: the IAM access key ID
	PurgeSecret   string `json:"purge-secret"`        // sealed: CloudFront secret key, Bunny / KeyCDN API key, Fastly API token

	// More CDNs purged together with the one above (gocdn_purge.go): each has its own credentials.
	PurgeExtra []gocdnPurgeConf `json:"purge-extra,omitempty"`

	// Connections replaced on the Storage screen while items still live there (gocdn_move.go).
	Previous []gocdnConn `json:"previous-connections,omitempty"`
}

// gocdnProvider describes a storage provider's defaults.
type gocdnProvider struct {
	Key, Name, Region, Endpoint, Public string // Endpoint and Public: {region} and {bucket} are filled in
	PathStyle                           bool
	Help                                string
}

var gocdnProviders = []gocdnProvider{
	{Key: "aws", Name: "Amazon S3", Region: "us-east-1", Endpoint: "https://s3.{region}.amazonaws.com", Public: "https://{bucket}.s3.{region}.amazonaws.com",
		Help: "Create an IAM user with s3:PutObject, s3:GetObject, s3:DeleteObject and s3:ListBucket on the bucket."},
	{Key: "r2", Name: "Cloudflare R2", Region: "auto", Endpoint: "https://<ACCOUNT_ID>.r2.cloudflarestorage.com", PathStyle: true,
		Help: "Use an R2 API token (Object Read & Write). R2's S3 endpoint is not public: set the bucket's r2.dev URL or a custom domain as the delivery domain."},
	{Key: "do", Name: "DigitalOcean Spaces", Region: "nyc3", Endpoint: "https://{region}.digitaloceanspaces.com", Public: "https://{bucket}.{region}.digitaloceanspaces.com",
		Help: "Create a Spaces access key. The Spaces CDN domain ({bucket}.{region}.cdn.digitaloceanspaces.com) can be the delivery domain."},
	{Key: "b2", Name: "Backblaze B2", Region: "us-west-004", Endpoint: "https://s3.{region}.backblazeb2.com", Public: "https://{bucket}.s3.{region}.backblazeb2.com",
		Help: "Create an application key for the bucket; the region is the part of the bucket's S3 endpoint after \"s3.\"."},
	{Key: "minio", Name: "MinIO / other S3-compatible", Region: "us-east-1", PathStyle: true,
		Help: "Any S3 API: MinIO, Wasabi, Scaleway, Linode, Ceph RGW, Garage… Enter the endpoint URL."},
}

func gocdnProviderByKey(k string) *gocdnProvider {
	for i := range gocdnProviders {
		if gocdnProviders[i].Key == k {
			return &gocdnProviders[i]
		}
	}
	return nil
}

func gocdnDefaults() gocdnSettings {
	return gocdnSettings{
		ImgCacheMaxMB: 2048,
		Provider:      "aws",
		Prefix:        "wp-content/uploads/",
		PublicRead:    true,
		CopyToBucket:  true,
		ServeFromCDN:  true,
		LocalDelivery: "redirect",
		ImgMaxW:       2560,
		ImgMaxH:       2560,
		ImgQuality:    82,
		ImgWebP:       true,
		ImgSign:       true,
		ImgRewriteQ:   80,
		ImgMaxSource:  60,
		ImgMaxFrames:  200,
		MultipartMB:   16,
		AssetsDirs:    "/wp-content/themes/\n/wp-includes/\n/wp-content/plugins/\n/wp-admin/assets/",
		AssetsExt:     "css,js,mjs,png,jpg,jpeg,gif,svg,webp,avif,ico,woff,woff2,ttf,otf,eot",
	}
}

// gocdnOn reports whether the plugin is active.
func (s *Server) gocdnOn() bool { return s.settings.Bool(gocdnSetting) }

// gocdnEnvSettings parses PURSAE_CDN_SETTINGS: the values and the set of keys it defines.
func gocdnEnvSettings() (map[string]any, error) {
	raw := strings.TrimSpace(config.Env(gocdnEnv))
	if raw == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// gocdnLoad returns the saved settings over the defaults, with PURSAE_CDN_SETTINGS on top. Secrets
// stay sealed (gocdnReveal opens them).
func (s *Server) gocdnLoad() gocdnSettings {
	st := gocdnDefaults()
	if v := s.settings.Get(gocdnOption); v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &st)
		}
	}
	if env, err := gocdnEnvSettings(); err != nil {
		slog.Warn("gocdn: "+gocdnEnv+" is not valid JSON", "err", err)
	} else if env != nil {
		raw, _ := json.Marshal(env)
		_ = json.Unmarshal(raw, &st)
	}
	return s.gocdnTrackConn(gocdnSanitize(st))
}

func gocdnSanitize(st gocdnSettings) gocdnSettings {
	if gocdnProviderByKey(st.Provider) == nil {
		st.Provider = "minio"
	}
	switch st.LocalDelivery {
	case "redirect", "proxy", "signed":
	default:
		st.LocalDelivery = "redirect"
	}
	st.Prefix = strings.TrimLeft(strings.TrimSpace(st.Prefix), "/")
	if st.Prefix != "" && !strings.HasSuffix(st.Prefix, "/") {
		st.Prefix += "/"
	}
	st.CDNDomain = strings.TrimRight(strings.TrimSpace(st.CDNDomain), "/")
	st.AssetsURL = strings.TrimRight(strings.TrimSpace(st.AssetsURL), "/")
	st.ImgCDNURL = strings.TrimRight(strings.TrimSpace(st.ImgCDNURL), "/")
	if st.ImgCacheMaxMB < 0 {
		st.ImgCacheMaxMB = 0
	}
	clamp := func(v *int, lo, hi, def int) {
		if *v < lo || *v > hi {
			*v = def
		}
	}
	clamp(&st.ImgMaxW, 16, 8192, 2560)
	clamp(&st.ImgMaxH, 16, 8192, 2560)
	clamp(&st.ImgQuality, 1, 100, 82)
	clamp(&st.ImgRewriteQ, 1, 100, 80)
	clamp(&st.ImgMaxSource, 1, 400, 60)
	clamp(&st.ImgMaxFrames, 1, 1000, 200)
	clamp(&st.MultipartMB, 5, 5120, 16)
	switch st.SSEType {
	case "s3", "kms", "customer":
	case "":
		if st.SSE {
			st.SSEType = "s3"
		}
	default:
		st.SSEType = ""
	}
	st.SSE = st.SSEType == "s3"
	st.SSEKMSKey = strings.TrimSpace(st.SSEKMSKey)
	if !gocdnKMSKeyRe.MatchString(st.SSEKMSKey) {
		st.SSEKMSKey = ""
	}
	st.StorageClass = strings.ToUpper(strings.TrimSpace(st.StorageClass))
	if !gocdnStorageClassRe.MatchString(st.StorageClass) {
		st.StorageClass = ""
	}
	if st.PurgeProvider == "" && st.CF {
		st.PurgeProvider = "cloudflare"
	}
	if gocdnPurgerByKey(st.PurgeProvider) == nil {
		st.PurgeProvider = ""
	}
	st.CF = st.PurgeProvider == "cloudflare"
	st.PurgeZone = strings.TrimSpace(st.PurgeZone)
	st.PurgeKeyID = strings.TrimSpace(st.PurgeKeyID)
	st.PurgeExtra = gocdnCleanExtras(st.PurgeProvider, st.PurgeExtra)
	return st
}

// gocdnKMSKeyRe: a key ID, alias (alias/name) or ARN.
var gocdnKMSKeyRe = regexp.MustCompile(`^[A-Za-z0-9:/_+=,.@-]{0,1000}$`)

var gocdnStorageClassRe = regexp.MustCompile(`^[A-Z_]{0,40}$`)

// gocdnPutOptions are the headers of every object GoCDN writes for an uploads path.
func gocdnPutOptions(st gocdnSettings, rel string) gocdn.PutOptions {
	o := gocdn.PutOptions{ContentType: gocdnMime(rel), CacheControl: "public, max-age=31536000, immutable", StorageClass: st.StorageClass}
	if st.PublicRead {
		o.ACL = "public-read"
	}
	if mediaIsPrivateRel(rel) { // private files: no public ACL, no shared caching
		o.CacheControl = "private, no-store"
		if st.PublicRead {
			o.ACL = "private"
		}
	}
	switch st.SSEType {
	case "s3":
		o.SSE = "AES256"
	case "kms":
		o.SSE, o.SSEKMSKeyID = "aws:kms", st.SSEKMSKey
	}
	return o
}

// gocdnUploadOptions add the multipart threshold: parts are the threshold's size, 5–64 MB.
func gocdnUploadOptions(st gocdnSettings, rel string) gocdn.UploadOptions {
	th := int64(st.MultipartMB) << 20
	return gocdn.UploadOptions{PutOptions: gocdnPutOptions(st, rel), Threshold: th, PartSize: min(max(th, gocdn.MinPartSize), 64<<20)}
}

// gocdnLocked is the set of JSON keys defined by PURSAE_CDN_SETTINGS (shown read-only).
func gocdnLocked() map[string]bool {
	env, _ := gocdnEnvSettings()
	out := map[string]bool{}
	for k := range env {
		out[k] = true
	}
	return out
}

// gocdnSave stores the settings (the caller keeps sealed secrets sealed) and purges the page cache:
// media and asset URLs on every page may change.
func (s *Server) gocdnSave(ctx context.Context, st gocdnSettings) error {
	st = gocdnSanitize(st)
	// A new endpoint is checked; an unchanged one (or one PURSAE_CDN_SETTINGS defines) is left to
	// the dial-time check, so other tabs still save on a site whose stored endpoint is refused.
	if ep := strings.TrimSpace(st.Endpoint); ep != "" && !gocdnLocked()["endpoint"] {
		stored, _ := s.settings.Get(gocdnOption).(map[string]any)
		if old, _ := stored["endpoint"].(string); strings.TrimSpace(old) != ep {
			if err := gocdnCheckEndpoint(ep); err != nil {
				return err
			}
		}
	}
	if st.ImgKey == "" {
		st.ImgKey = gocdnRandomKey()
	}
	st.SecretKey = s.gocdnSeal(st.SecretKey)
	st.CFToken = s.gocdnSeal(st.CFToken)
	st.SSECKey = s.gocdnSeal(st.SSECKey)
	st.PurgeSecret = s.gocdnSeal(st.PurgeSecret)
	st.PurgeExtra = append([]gocdnPurgeConf(nil), st.PurgeExtra...)
	for i := range st.PurgeExtra {
		st.PurgeExtra[i].Secret = s.gocdnSeal(st.PurgeExtra[i].Secret)
	}
	st.Previous = append([]gocdnConn(nil), st.Previous...)
	for i := range st.Previous {
		st.Previous[i].SecretKey = s.gocdnSeal(st.Previous[i].SecretKey)
		st.Previous[i].SSECKey = s.gocdnSeal(st.Previous[i].SSECKey)
	}
	st.ImgKey = s.gocdnSeal(st.ImgKey)
	// Keys defined by PURSAE_CDN_SETTINGS are never written to the database: what was stored before
	// for them stays as it was.
	raw, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if locked := gocdnLocked(); len(locked) > 0 {
		stored, _ := s.settings.Get(gocdnOption).(map[string]any)
		for k := range locked {
			if v, ok := stored[k]; ok {
				m[k] = v
			} else {
				delete(m, k)
			}
		}
	}
	if err := s.settings.Set(ctx, map[string]any{gocdnOption: m}); err != nil {
		return err
	}
	s.gocdnMarkSeen(ctx, st)
	s.gocdnPurge(ctx, true)
	return nil
}

// --- secrets ---------------------------------------------------------------------------------

func (s *Server) gocdnKey() []byte {
	secret := s.cfg.Secret
	if secret == "" {
		secret = s.settings.String("auth_secret")
	}
	k := sha256.Sum256([]byte("wpgo-gocdn|" + secret))
	return k[:]
}

// gocdnSeal encrypts a secret for storage; empty and already sealed values pass through.
func (s *Server) gocdnSeal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, gocdnSealPrefix) {
		return plain
	}
	block, _ := aes.NewCipher(s.gocdnKey())
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return gocdnSealPrefix + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil))
}

// gocdnReveal decrypts a stored secret; values from PURSAE_CDN_SETTINGS are plain and pass through.
func (s *Server) gocdnReveal(v string) string {
	raw, ok := strings.CutPrefix(v, gocdnSealPrefix)
	if !ok {
		return v
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	block, _ := aes.NewCipher(s.gocdnKey())
	gcm, _ := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize() {
		return ""
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		slog.Warn("gocdn: a stored secret cannot be decrypted (was the server secret changed?)")
		return ""
	}
	return string(plain)
}

// gocdnImgKey is the HMAC key of signed image URLs: a random key stored (sealed) with the
// settings, so it can be rotated without touching the server secret; derived from the server
// secret while none is stored (PURSAE_CDN_SETTINGS may leave it out).
func (s *Server) gocdnImgKey(st gocdnSettings) []byte {
	if k := s.gocdnReveal(st.ImgKey); k != "" {
		return []byte(k)
	}
	k := sha256.Sum256(append([]byte("wpgo-gocdn-img|"), s.gocdnKey()...))
	return k[:]
}

func gocdnRandomKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- the bucket ------------------------------------------------------------------------------

// gocdnEndpoint is the S3 API endpoint: the one entered, or the provider's for the region.
func gocdnEndpoint(st gocdnSettings) string {
	if st.Endpoint != "" {
		return strings.TrimRight(st.Endpoint, "/")
	}
	p := gocdnProviderByKey(st.Provider)
	if p == nil || p.Endpoint == "" || strings.Contains(p.Endpoint, "<") {
		return ""
	}
	return strings.ReplaceAll(p.Endpoint, "{region}", gocdnRegion(st))
}

func gocdnRegion(st gocdnSettings) string {
	if st.Region != "" {
		return st.Region
	}
	if p := gocdnProviderByKey(st.Provider); p != nil {
		return p.Region
	}
	return "us-east-1"
}

// gocdnClient is the S3 client for the settings.
func (s *Server) gocdnClient(st gocdnSettings) (*gocdn.Client, error) {
	httpClient := gocdnStorageHTTP
	if ep := gocdnEnvEndpoint(); ep != "" && ep == gocdnEndpoint(st) {
		httpClient = gocdnEnvStorageHTTP
	}
	c := &gocdn.Client{Endpoint: gocdnEndpoint(st), Region: gocdnRegion(st), Bucket: st.Bucket, AccessKey: st.AccessKey,
		SecretKey: s.gocdnReveal(st.SecretKey), PathStyle: st.PathStyle, HTTP: httpClient}
	if c.Endpoint == "" {
		return nil, errors.New("no endpoint: enter the S3 endpoint URL of your provider")
	}
	if st.SSEType == "customer" {
		key, err := gocdn.ParseSSECKey(s.gocdnReveal(st.SSECKey))
		if err != nil {
			return nil, errors.New("SSE-C is on but no valid customer key is set")
		}
		c.SSECKey = key
	}
	return c, c.Validate()
}

// gocdnConfigured reports whether a bucket and credentials are set.
func (s *Server) gocdnConfigured(st gocdnSettings) bool {
	_, err := s.gocdnClient(st)
	return err == nil
}

// gocdnPublicBase is the URL objects are delivered from: the delivery (CDN) domain, else the
// bucket's public URL. Keys are appended after a slash.
func gocdnPublicBase(st gocdnSettings) string {
	if st.CDNDomain != "" {
		if !strings.Contains(st.CDNDomain, "://") {
			return "https://" + st.CDNDomain
		}
		return st.CDNDomain
	}
	if p := gocdnProviderByKey(st.Provider); p != nil && p.Public != "" && st.Endpoint == "" {
		return strings.NewReplacer("{bucket}", st.Bucket, "{region}", gocdnRegion(st)).Replace(p.Public)
	}
	ep := gocdnEndpoint(st)
	if ep == "" {
		return ""
	}
	if st.PathStyle {
		return ep + "/" + st.Bucket
	}
	if u, err := url.Parse(ep); err == nil {
		u.Host = st.Bucket + "." + u.Host
		return strings.TrimRight(u.String(), "/")
	}
	return ""
}

// gocdnObjectURL is the delivery URL of an object key.
func gocdnObjectURL(st gocdnSettings, key string) string {
	base := gocdnPublicBase(st)
	if base == "" {
		return ""
	}
	return base + gocdn.EscapePath("/"+key)
}

// gocdnNewKey is the key a file is uploaded under. A private file (media_private.go) gets a
// folder name nobody can guess, so it stays unreachable in a bucket that is public by policy; with
// "Object versioning" every upload gets a time-stamped folder, so a file written again under the
// same name has a new URL and no CDN or browser shows the old one.
func (s *Server) gocdnNewKey(st gocdnSettings, rel string) string {
	rel = strings.TrimLeft(rel, "/")
	dir, file := path.Split(rel)
	if mediaIsPrivateRel(rel) {
		m := hmac.New(sha256.New, s.mediaPrivateKey())
		m.Write([]byte("key|" + rel))
		dir += hex.EncodeToString(m.Sum(nil)[:16]) + "/"
	}
	if st.ObjectVersioning {
		dir += time.Now().UTC().Format("20060102150405") + "/"
	}
	return st.Prefix + dir + file
}

// gocdnKeyFits reports whether key is a key gocdnNewKey could have made for rel under the current
// prefix (the plain key, or one with the private / version folders).
func gocdnKeyFits(st gocdnSettings, key, rel string) bool {
	rel = strings.TrimLeft(rel, "/")
	if key == st.Prefix+rel {
		return !mediaIsPrivateRel(rel)
	}
	dir, file := path.Split(rel)
	rest, ok := strings.CutPrefix(key, st.Prefix+dir)
	if !ok || !strings.HasSuffix(rest, "/"+file) {
		return false
	}
	extra := strings.Split(strings.TrimSuffix(rest, "/"+file), "/")
	return len(extra) >= 1 && len(extra) <= 2 && !slices.Contains(extra, "")
}

// --- runtime state -----------------------------------------------------------------------------

// gocdnRuntime is per-server state that has no place in the Server struct (a shared file): the
// background job.
type gocdnRuntime struct {
	mu       sync.Mutex
	seen     *gocdnConn // the connection last seen in effect (plain secrets), nil until loaded
	pending  *gocdnConn // a replaced connection being remembered (gocdnTrackConn)
	job      *gocdnJobState
	reRemove map[string]bool // fetched-back copies waiting to be deleted again
	orphans  []string        // the last orphan scan's result (gocdnScanOrphans)

	// Writers publish objects and their mappings together with respect to untracked-object cleanup.
	objectWrites sync.RWMutex
}

var gocdnRuntimes sync.Map // *Server → *gocdnRuntime

func (s *Server) gocdnRT() *gocdnRuntime {
	v, _ := gocdnRuntimes.LoadOrStore(s, &gocdnRuntime{})
	return v.(*gocdnRuntime)
}

// --- plugin plumbing -------------------------------------------------------------------------

// gocdnPluginRow is GoCDN's row on the Plugins screen.
func (s *Server) gocdnPluginRow() pluginRow {
	r := pluginRow{Slug: gocdnSlug, Name: "Pursae CDN", Version: builtinPluginVersion, Native: true, Active: s.gocdnOn(),
		Description: "Offload the media library to Amazon S3, Cloudflare R2, DigitalOcean Spaces, Backblaze B2 or any S3-compatible storage, deliver it from a CDN, resize and convert images to WebP on the fly with signed URLs, serve theme and core assets from a CDN and purge the page cache and Cloudflare. While it is off, nothing of it runs.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/gocdn"
	} else if s.settings.Bool(gocdnFallbackOption) {
		r.Description += " Files that were removed from the server are still served from the bucket while it is off; activate it and run \"Download back to the server\" to bring them back."
	}
	return r
}

// gocdnPluginAction is the Plugins screen's Activate / Deactivate. Offloaded files keep being
// tracked while it is off and URLs point back at /wp-content/uploads. When files exist only in the
// bucket, the Deactivate link leads to a confirmation screen first, and any deactivation keeps
// answering those files from the bucket (the uploads fallback, gocdn_fetch.go).
func (s *Server) gocdnPluginAction(c *gin.Context, action string) {
	if action != "activate" {
		if c.Request.Method == http.MethodGet && c.Query("gocdn-confirm") == "" && s.gocdnHasRemoved(c) {
			c.Redirect(http.StatusFound, "/wp-admin/gocdn?page=deactivate")
			return
		}
		if err := s.gocdnDeactivate(c); err != nil {
			c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(err.Error()))
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/plugins?deactivate=true")
		return
	}
	if err := s.settings.Set(c, map[string]any{gocdnSetting: true, gocdnFallbackOption: false}); err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+url.QueryEscape(err.Error()))
		return
	}
	s.purgeAll(c)
	c.Redirect(http.StatusFound, "/wp-admin/plugins?activate=true")
}

func (s *Server) gocdnOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.gocdnOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// registerGoCDN wires the admin screens; they answer 404 while the plugin is off. The image
// service and the uploads fallback run in gocdnServe, before the page cache.
func (s *Server) registerGoCDN(admin *gin.RouterGroup) {
	admin.GET("/gocdn", s.gocdnOnly(), s.gocdnAdmin)
	admin.POST("/gocdn", s.gocdnOnly(), s.gocdnAdmin)
}

// gocdnMenuPages are the screens of the GoCDN menu.
var gocdnMenuPages = []struct{ Page, Title string }{
	{"", "Storage"}, {"offload", "Media Offload"}, {"images", "Image Service"}, {"assets", "Assets CDN"}, {"purge", "Purge"},
}

// gocdnMenu adds the GoCDN top-level menu before Settings and "Remote Storage" under Media.
func (s *Server) gocdnMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.gocdnOn() || u == nil || !u.Can("manage_options", nil) {
		return menu
	}
	top := &menuTop{Slug: "gocdn", Title: "Pursae CDN", ID: "toplevel_page_gocdn", Icon: "dashicons-cloud-upload"}
	for i, p := range gocdnMenuPages {
		slug := "gocdn"
		if p.Page != "" {
			slug += "?page=" + p.Page
		}
		ms := &menuSub{Slug: slug, Title: template.HTML(p.Title), First: i == 0, Current: slug == screen}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	for _, m := range menu {
		if m.ID == "menu-media" {
			ms := &menuSub{Slug: "gocdn?page=remote", Title: "Remote Storage", Current: screen == "gocdn?page=remote"}
			m.Sub = append(m.Sub, ms)
			if ms.Current {
				m.Current = true
				for _, sub := range m.Sub[:len(m.Sub)-1] {
					sub.Current = false
				}
			}
		}
	}
	for i, m := range menu {
		if m.ID == "menu-settings" {
			top.First, m.First = m.First, false
			return append(menu[:i], append([]*menuTop{top}, menu[i:]...)...)
		}
	}
	return append(menu, top)
}

// gocdnServe runs in Server.Handler before the page cache: the image service (/cdn-img/… and
// /wp-content/uploads/…?w=…) and /wp-content/uploads for files that live only in the bucket. It
// reports whether it answered.
func (s *Server) gocdnServe(w http.ResponseWriter, r *http.Request) bool {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !s.settings.Installed() {
		return false
	}
	p := r.URL.Path
	if !s.gocdnOn() {
		// Deactivated with files left only in the bucket: keep their URLs working.
		if s.settings.Bool(gocdnFallbackOption) && strings.HasPrefix(p, "/wp-content/uploads/") {
			return s.gocdnServeUpload(w, r, s.gocdnLoad(), strings.TrimPrefix(p, "/wp-content/uploads/"))
		}
		return false
	}
	switch {
	case strings.HasPrefix(p, gocdnImgPath):
		st := s.gocdnLoad()
		if !st.Img {
			http.NotFound(w, r)
			return true
		}
		s.gocdnImage(w, r, st, strings.TrimPrefix(p, gocdnImgPath))
		return true
	case strings.HasPrefix(p, "/wp-content/uploads/"):
		rel := strings.TrimPrefix(p, "/wp-content/uploads/")
		st := s.gocdnLoad()
		if st.Img && gocdn.HasImageParams(r.URL.Query()) {
			s.gocdnImage(w, r, st, rel)
			return true
		}
		return s.gocdnServeUpload(w, r, st, rel)
	}
	return false
}
