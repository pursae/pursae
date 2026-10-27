package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed defaults/block-patterns.json
var blockPatternsJSON []byte

var patternsOnce sync.Once
var patternData struct {
	Patterns   []map[string]any `json:"patterns"`
	Categories []map[string]any `json:"categories"`
}

func loadPatterns() {
	patternsOnce.Do(func() { _ = json.Unmarshal(blockPatternsJSON, &patternData) })
}

// corePatterns are WordPress' bundled block patterns plus Twenty Sixteen's own.
func corePatterns() []map[string]any {
	loadPatterns()
	return patternData.Patterns
}

func corePatternCategories() []map[string]any {
	loadPatterns()
	return patternData.Categories
}

// oEmbed ----------------------------------------------------------------------

type oembedProvider struct {
	re       *regexp.Regexp
	endpoint string
}

// oembedProviders is a subset of WordPress' WP_oEmbed provider list.
var oembedProviders = []oembedProvider{
	{regexp.MustCompile(`^https?://((m|www)\.)?youtube\.com/(watch|playlist|shorts|live)`), "https://www.youtube.com/oembed"},
	{regexp.MustCompile(`^https?://youtu\.be/`), "https://www.youtube.com/oembed"},
	{regexp.MustCompile(`^https?://(.+\.)?vimeo\.com/`), "https://vimeo.com/api/oembed.json"},
	{regexp.MustCompile(`^https?://(www\.)?(twitter|x)\.com/\w{1,15}/status(es)?/`), "https://publish.twitter.com/oembed"},
	{regexp.MustCompile(`^https?://(www\.)?soundcloud\.com/`), "https://soundcloud.com/oembed"},
	{regexp.MustCompile(`^https?://(open|play)\.spotify\.com/`), "https://embed.spotify.com/oembed/"},
	{regexp.MustCompile(`^https?://(www\.)?flickr\.com/`), "https://www.flickr.com/services/oembed/"},
	{regexp.MustCompile(`^https?://(www\.)?(dailymotion\.com|dai\.ly)/`), "https://www.dailymotion.com/services/oembed"},
	{regexp.MustCompile(`^https?://(www\.)?tiktok\.com/`), "https://www.tiktok.com/oembed"},
	{regexp.MustCompile(`^https?://(www\.)?reddit\.com/r/`), "https://www.reddit.com/oembed"},
}

var oembedCache sync.Map // url -> cached response

type oembedEntry struct {
	data    map[string]any
	expires time.Time
}

// fetchOEmbed resolves a URL through its provider, caching results for a day like WordPress' oEmbed cache.
func fetchOEmbed(ctx context.Context, target string, maxWidth string) (map[string]any, error) {
	key := target + "|" + maxWidth
	if v, ok := oembedCache.Load(key); ok && v.(oembedEntry).expires.After(time.Now()) {
		return v.(oembedEntry).data, nil
	}
	var endpoint string
	for _, p := range oembedProviders {
		if p.re.MatchString(target) {
			endpoint = p.endpoint
			break
		}
	}
	if endpoint == "" {
		return nil, fmt.Errorf("no provider")
	}
	q := url.Values{"url": {target}, "format": {"json"}}
	if maxWidth != "" {
		q.Set("maxwidth", maxWidth)
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "WordPress Go/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider status %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return nil, err
	}
	oembedCache.Store(key, oembedEntry{data: data, expires: time.Now().Add(24 * time.Hour)})
	return data, nil
}

func (api *restAPI) oembedProxy(c *gin.Context, r *restRequest) {
	if restCurrent(c) == nil || !restCurrent(c).Can("edit_posts", nil) {
		restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to make proxied oEmbed requests.")
		return
	}
	data, err := fetchOEmbed(c.Request.Context(), c.Query("url"), c.Query("maxwidth"))
	if err != nil {
		restErr(c, http.StatusNotFound, "oembed_invalid_url", "Not Found")
		return
	}
	restJSON(c, http.StatusOK, data)
}

// RegisterREST wires every route (called from routes()).
func (s *Server) restRoutes() *restAPI {
	api := &restAPI{s: s}
	api.registerPosts()
	api.registerTerms()
	api.registerUsers()
	api.registerMedia()
	api.registerComments()
	api.registerMisc()
	api.registerWidgets()
	api.registerAppPasswords()
	api.registerMenus()
	api.registerOEmbed()
	return api
}
