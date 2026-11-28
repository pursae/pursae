package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wpgo/internal/cache"
)

// Front proxy purges. With "Front Server" set to LiteSpeed, nginx or a generic CDN, pages carry
// cacheable headers and tags (internal/cache/hints.go). Every purge of the in-process cache is
// forwarded to the purge endpoint configured in Cache Settings as a PURGE request, so the proxy
// does not serve a changed page for the rest of its TTL:
//
//   - purge by URL: PURGE <endpoint>/<path of the page>
//   - purge by tags: PURGE <endpoint> with Surrogate-Key (Varnish, Fastly), Cache-Tag (Cloudflare)
//     and X-LiteSpeed-Purge headers listing the prefixed tags
//   - purge everything: PURGE <endpoint> with X-Purge-All: 1 and X-LiteSpeed-Purge: *
//
// Without an endpoint the proxy's copy lifetime is capped (Config.ProxyMaxAge) instead.

// proxyPurgeConfigured reports whether purges are forwarded for this hint and endpoint.
func proxyPurgeConfigured(h cache.ServerHint, endpoint string) bool {
	switch h {
	case cache.HintLiteSpeed, cache.HintNginx, cache.HintGeneric:
	default:
		return false
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// cacheProxyWarning is the notice on Cache Settings when a front server is set but nothing purges it.
func (s *Server) cacheProxyWarning() string {
	h := cache.ServerHint(s.settings.String("wpgo_cache_server"))
	if h != cache.HintLiteSpeed && h != cache.HintNginx && h != cache.HintGeneric {
		return ""
	}
	if proxyPurgeConfigured(h, s.settings.String("wpgo_cache_purge_url")) {
		return ""
	}
	return "No purge URL is set: the front server may keep a changed page for up to the proxy lifetime below. Set a purge URL to purge it when content changes."
}

// proxyPurgeRequest builds the PURGE request for p, or nil when there is nothing to send.
func proxyPurgeRequest(ctx context.Context, endpoint, prefix string, p cache.Purge) (*http.Request, error) {
	target := strings.TrimSpace(endpoint)
	switch {
	case p.URL != "":
		if pu, err := url.Parse(p.URL); err == nil {
			base, err := url.Parse(target)
			if err != nil {
				return nil, err
			}
			base.RawPath = strings.TrimRight(base.EscapedPath(), "/") + "/" + strings.TrimLeft(pu.EscapedPath(), "/")
			base.Path, _ = url.PathUnescape(base.RawPath) // EscapedPath produces valid escapes.
			base.RawQuery = pu.RawQuery
			target = base.String()
		}
	}
	req, err := http.NewRequestWithContext(ctx, "PURGE", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wpgo-cache-purge/1")
	switch {
	case p.All:
		req.Header.Set("X-Purge-All", "1")
		req.Header.Set("X-LiteSpeed-Purge", "*")
	case len(p.Tags) > 0:
		var keys, ls []string
		for _, t := range p.Tags {
			keys = append(keys, prefix+t)
			ls = append(ls, "tag="+prefix+t)
		}
		req.Header.Set("Surrogate-Key", strings.Join(keys, " "))
		req.Header.Set("Cache-Tag", strings.Join(keys, ","))
		req.Header.Set("X-LiteSpeed-Purge", strings.Join(ls, ", "))
	case p.URL == "":
		return nil, nil
	}
	return req, nil
}

// proxyPurgeClient: the purge URL is set by a site administrator (Cache Settings, REST, MCP), so
// like every admin-set endpoint it reaches loopback and private addresses only with
// PURSAE_ALLOW_PRIVATE_ENDPOINTS=1 (a local Varnish or nginx on 127.0.0.1 or a Docker network is the
// server operator's call, not the admin's); link-local, metadata and reserved addresses are refused
// always. The check runs at every connection (endpoint_guard.go). What it can send is fixed:
// PURGE to the configured host only (redirects are never followed) with the purge headers.
var proxyPurgeClient = func() *http.Client {
	client := endpointClient(5*time.Second, privateEndpointsAllowed, privateEndpointsEnv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}()

// proxyPurgeURLProblem is the save-time check of the purge URL (Cache Settings and the REST API):
// an http(s) URL whose host, when an IP literal, is public (private and loopback addresses need
// PURSAE_ALLOW_PRIVATE_ENDPOINTS=1; link-local, metadata and reserved ones are refused always).
func proxyPurgeURLProblem(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return errors.New("the purge URL must be an http(s) URL such as http://127.0.0.1:6081")
	}
	if u.Fragment != "" {
		return errors.New("the purge URL must not contain a fragment")
	}
	if err := endpointHostOK(u, privateEndpointsAllowed(), privateEndpointsEnv); err != nil {
		return fmt.Errorf("purge URL: %w", err)
	}
	return nil
}

func sendProxyPurge(req *http.Request) error {
	resp, err := proxyPurgeClient.Do(req)
	if err != nil {
		return fmt.Errorf("front proxy purge: %w", err)
	}
	defer resp.Body.Close()
	// nginx's ngx_cache_purge answers 404 when the page is not in its cache: nothing to purge.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("front proxy purge: HTTP %d", resp.StatusCode)
	}
	return nil
}

// forwardProxyPurge sends a purge to the configured front proxy, in the background.
func (s *Server) forwardProxyPurge(p cache.Purge) {
	endpoint := s.settings.String("wpgo_cache_purge_url")
	if !proxyPurgeConfigured(cache.ServerHint(s.settings.String("wpgo_cache_server")), endpoint) {
		return
	}
	prefix := "wpgo_"
	if c := s.currentCache(); c != nil {
		prefix = c.Config().TagPrefix
	}
	req, err := proxyPurgeRequest(context.Background(), endpoint, prefix, p)
	if err != nil || req == nil {
		return
	}
	safeGo("wpgocache_proxy.forwardProxyPurge", func() {
		if err := sendProxyPurge(req); err != nil {
			slog.Warn("page cache: forwarding a purge to the front proxy", "err", err)
		}
	})
}
