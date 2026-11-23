// Package webbuilder holds the Pursae Web Builder pieces that do not need the server: the
// integration contract (this file), doc validation, the snapshot and CSS sanitizers, the
// doc → fallback HTML generator and the form schema.
//
// contract.go is the Go half of the integration contract. The prose and the field tables live in
// docs/developer/web-builder-contract.md; the TypeScript half is builder/host/contract.ts. The
// three are kept in step by hand: a change to one is a change to all three, in one commit.
//
// The document model types (DocState and below) mirror the upstream editor's
// src/builder/types.ts at the SHA in UpstreamTypesSHA. They are used to read and validate a doc;
// the stored doc is the client's JSON with privileged fields removed at the JSON level, so fields
// a newer editor adds survive a round trip through an older server.
package webbuilder

import "encoding/json"

// Versions and handshake.
const (
	// APIVersion is the REST/boot schema version. Every web-builder/v2 request from the editor
	// carries it in APIHeader; a mismatch answers 409 ErrAPIMismatch with data.reload=true. Bump it
	// on any incompatible change to a payload in this file. 2 = Theme Builder (boot template, site
	// and dynamic blocks, template routes, Item.dyn, PageSettings.layout, the loop section).
	APIVersion = 2
	// RendererVersion is the version of the static markup and the data-wb-* runtime contract.
	// Snapshots with an older renderer are listed by Web Builder › Settings › Regenerate pages.
	// Bump it only for an incompatible change: one that makes a stored snapshot render wrong or
	// makes the runtime or the server misread it. Additive markup (new data-wb-* attributes, new
	// cell types or section kinds an older snapshot never contains) keeps the version.
	RendererVersion = 1
	// APIHeader is the request header carrying APIVersion.
	APIHeader = "X-WB-Api"
	// NonceHeader is the core REST nonce header (action "wp_rest").
	NonceHeader = "X-WP-Nonce"
	// BootGlobal is the window property holding Boot on the editor page.
	BootGlobal = "__WEB_BUILDER__"
	// UpstreamTypesSHA is the pursae/Builder commit the doc model types were copied from.
	UpstreamTypesSHA = "22ff4976a1416accd3b7370e176d7206bf390aee"
)

// REST namespace and routes (relative to /wp-json/). {id} and {rid} are decimal integers,
// {section} matches IDPattern, {provider} is one of the AIProvider values and {path} one of
// AIProviderPaths.
const (
	RESTNamespace = "web-builder/v2"

	RouteDocument        = "/web-builder/v2/documents/{id}"                         // GET
	RouteAutosave        = "/web-builder/v2/documents/{id}/autosave"                // PUT, DELETE
	RoutePublish         = "/web-builder/v2/documents/{id}/publish"                 // POST
	RouteLock            = "/web-builder/v2/documents/{id}/lock"                    // POST
	RouteRevisions       = "/web-builder/v2/documents/{id}/revisions"               // GET
	RouteRevision        = "/web-builder/v2/documents/{id}/revisions/{rid}"         // GET
	RouteRevisionRestore = "/web-builder/v2/documents/{id}/revisions/{rid}/restore" // POST
	RouteLibrary         = "/web-builder/v2/library"                                // GET, POST
	RouteLibraryItem     = "/web-builder/v2/library/{id}"                           // DELETE
	RoutePrefs           = "/web-builder/v2/prefs"                                  // GET, PUT
	RouteFontsCSS        = "/web-builder/v2/fonts/css2"                             // GET (text/css)
	RouteForm            = "/web-builder/v2/forms/{id}/{section}"                   // POST, public
	RouteAIStatus        = "/web-builder/v2/ai/status"                              // GET, AI on only
	RouteAILLM           = "/web-builder/v2/ai/llm/{provider}/{path}"               // POST, AI on only
	RouteAIImages        = "/web-builder/v2/ai/images"                              // POST, AI on only
	// Theme Builder (§11 of the contract doc).
	RouteTemplates          = "/web-builder/v2/templates"                 // GET (?type=), POST
	RouteTemplateConditions = "/web-builder/v2/templates/{id}/conditions" // PUT
	RouteTemplatePreview    = "/web-builder/v2/templates/{id}/preview"    // PUT
	RouteTemplateConflicts  = "/web-builder/v2/templates/{id}/conflicts"  // GET
	RouteTemplatePopup      = "/web-builder/v2/templates/{id}/popup"      // GET, PUT (PopupSettings)
	RouteTemplateExport     = "/web-builder/v2/templates/{id}/export"     // GET (TemplateFile)
	RouteTemplateImport     = "/web-builder/v2/templates/import"          // POST (TemplateFile)
	RoutePreviewContext     = "/web-builder/v2/preview-context"           // GET ?object=
	RouteNavigator          = "/web-builder/v2/navigator"                 // GET: the editor's page switcher
	MediaRoute              = "/wp/v2/media"                              // existing core route
)

// Asset URL prefixes and embed directories (web/web-builder/...).
const (
	// EditorAssetsPath serves web/web-builder/editor/ (AI build). 404 when AI is off.
	EditorAssetsPath = "/wp-content/plugins/web-builder/dist/editor/"
	// EditorNoAIAssetsPath serves web/web-builder/editor-noai/.
	EditorNoAIAssetsPath = "/wp-content/plugins/web-builder/dist/editor-noai/"
	// SiteAssetsPath serves web/web-builder/site/ (public runtime and base CSS), immutable.
	SiteAssetsPath = "/wp-content/web-builder/site/"
	// EmbedDir is the directory under web/ that holds the built bundles.
	EmbedDir = "web-builder"
	// ManifestFile and BuildFile sit in EmbedDir.
	ManifestFile = "manifest.json"
	BuildFile    = "BUILD.json"
	// PageCSSDir is the uploads sub-directory for page CSS files above InlineCSSMax.
	PageCSSDir = "web-builder/css"
	// FontsDir is the uploads sub-directory of localized Google Fonts (shared with the old builder).
	FontsDir = "web-builder/fonts"
)

// Bundle names (keys of Manifest).
const (
	BundleEditor     = "editor"
	BundleEditorNoAI = "editor-noai"
	BundleSite       = "site"
)

// Post and user meta keys, options.
const (
	PostMarkerMeta = "_web_builder" // "1" on every post that has a web_builder_docs row
	EditLockMeta   = "_edit_lock"   // WordPress format "<unix>:<user id>"
	UserPrefsMeta  = "_web_builder_prefs"
	PluginSlug     = "golementor" // plugin identity is kept: slug, option and URL action
	PluginOption   = "native_golementor"
	EditAction     = "web-builder" // /wp-admin/post?post={id}&action=web-builder
)

// Limits.
const (
	MaxDocBytes         = 4 << 20   // request body of autosave / publish / restore
	MaxSnapshotHTML     = 2 << 20   // RenderResult.HTML
	MaxCSSBytes         = 1 << 20   // utility CSS + section CSS together
	InlineCSSMax        = 24 << 10  // page CSS at or below this is inlined in <style id="wb-css">
	MaxSections         = 200       // top-level sections
	MaxItems            = 500       // items per section
	MaxDepth            = 4         // Item.Sub nesting
	MaxStringBytes      = 64 << 10  // any content / item string
	MaxHTMLSectionLen   = 256 << 10 // content["html"] of a 'html' section
	MaxCodeSectionLen   = 512 << 10 // content["html"] / content["head"] of a 'code' section
	MaxDataURIBytes     = 32 << 10  // data:image/* URI in src / url()
	MaxLibraryItems     = 500       // saved components per site
	MaxPrefsBytes       = 16 << 10  // all prefs of one user, JSON-encoded
	MaxRevisionsListed  = 50        // per page of GET revisions
	MaxFormBodyBytes    = 64 << 10
	MaxAIBodyBytes      = 8 << 20
	LockTTLSeconds      = 150 // same as WordPress' wp_check_post_lock window
	AutosaveMinMillis   = 2000
	LockRefreshSeconds  = 60
	AIMaxTokens         = 8192 // max_tokens cap on proxied LLM requests
	AIRatePerMinute     = 30   // per-user LLM requests per minute
	AITimeoutSeconds    = 120  // whole proxied request
	AIFirstByteSeconds  = 30
	AIImageMaxBytes     = 10 << 20
	AIImageMaxCount     = 12
	AIImageDefaultCount = 6
	FormMinMillis       = 2000 // wb_t below this = spam
	FormNameMax         = 200
	FormEmailMax        = 254
	FormPhoneMax        = 50
	FormMessageMax      = 5000
	FormOtherMax        = 200                     // date, choice
	IDPattern           = `^[A-Za-z0-9_-]{1,64}$` // section, item and saved-component ids
	PrefKeyPattern      = `^[a-z0-9:._-]{1,64}$`
)

// REST error codes (WordPress error shape: {"code","message","data":{"status",...}}).
const (
	ErrAPIMismatch  = "wb_api_mismatch"  // 409, data.reload=true, data.apiVersion
	ErrRevConflict  = "wb_rev_conflict"  // 409, data.rev, data.modified, data.modifiedBy
	ErrDetached     = "wb_detached"      // 409, publish without reattach on a detached page
	ErrLocked       = "wb_locked"        // 409, data.lock (another user holds the edit lock)
	ErrTooLarge     = "wb_too_large"     // 413, data.limit
	ErrInvalidDoc   = "wb_invalid_doc"   // 422, data.errors []ValidationError
	ErrNotFound     = "wb_not_found"     // 404
	ErrNotBuilder   = "wb_not_enabled"   // 404, post type not enabled for the builder / plugin off
	ErrFormInvalid  = "wb_form_invalid"  // 422, data.fields map[field]reason
	ErrRateLimited  = "wb_rate_limited"  // 429, Retry-After header
	ErrAIBudget     = "wb_ai_budget"     // 429, the site's daily token budget is spent
	ErrAIPath       = "wb_ai_path"       // 400, path not allowlisted for the provider
	ErrAIModel      = "wb_ai_model"      // 422, model not allowlisted
	ErrAINoKey      = "wb_ai_no_key"     // 503, provider has no key / URL configured
	ErrAIUpstream   = "wb_ai_upstream"   // 502, provider unreachable or rejected the server key
	ErrAITimeout    = "wb_ai_timeout"    // 504
	ErrBadCondition = "wb_bad_condition" // 422, data.errors []ValidationError (path /conditions/<i>, code VErrCondition)
	ErrBadTemplate  = "wb_bad_template"  // 422, unknown template type / starter / postType, or a status a template cannot take
	ErrBadPreview   = "wb_bad_preview"   // 422, malformed preview object; 404 wb_not_found when it names nothing
	ErrForbidden    = "rest_forbidden"   // 401 / 403, core code
	ErrInvalidNonce = "rest_cookie_invalid_nonce"
)

// Environment variables (read once by internal/config).
const (
	EnvAI                = "PURSAE_BUILDER_AI" // unset/""/1/true/on/yes = on; 0/false/off/no = off; anything else = off + warning
	EnvAIAnthropic       = "PURSAE_BUILDER_AI_ANTHROPIC_KEY"
	EnvAIOpenAI          = "PURSAE_BUILDER_AI_OPENAI_KEY"
	EnvAIGemini          = "PURSAE_BUILDER_AI_GEMINI_KEY"
	EnvAIMistral         = "PURSAE_BUILDER_AI_MISTRAL_KEY"
	EnvAIOllamaURL       = "PURSAE_BUILDER_AI_OLLAMA_URL"
	EnvAIModels          = "PURSAE_BUILDER_AI_MODELS"       // "provider:model,provider:model"; unset = DefaultAIModels
	EnvAIDailyBudget     = "PURSAE_BUILDER_AI_DAILY_BUDGET" // tokens (input+output) per site per UTC day; unset = DefaultAIDailyBudget; 0 = no cap
	DefaultAIDailyBudget = 1_000_000
)

// AIProvider ids are the upstream ids (providers.ts); "google" is Gemini.
type AIProvider string

const (
	AIAnthropic AIProvider = "anthropic"
	AIOpenAI    AIProvider = "openai"
	AIGoogle    AIProvider = "google"
	AIMistral   AIProvider = "mistral"
	AIOllama    AIProvider = "ollama"
)

// AIProviderBases are the fixed upstream bases; Ollama's comes from EnvAIOllamaURL.
var AIProviderBases = map[AIProvider]string{
	AIAnthropic: "https://api.anthropic.com",
	AIOpenAI:    "https://api.openai.com",
	AIGoogle:    "https://generativelanguage.googleapis.com",
	AIMistral:   "https://api.mistral.ai",
}

// AIProviderPaths is the path allowlist of RouteAILLM ({path} without the leading slash).
var AIProviderPaths = map[AIProvider][]string{
	AIAnthropic: {"v1/messages"},
	AIOpenAI:    {"v1/chat/completions"},
	AIGoogle:    {"v1beta/openai/chat/completions"},
	AIMistral:   {"v1/chat/completions"},
	AIOllama:    {"v1/chat/completions"},
}

// DefaultAIModels is the model allowlist when EnvAIModels is unset (upstream providers.ts catalog).
var DefaultAIModels = map[AIProvider][]string{
	AIAnthropic: {"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"},
	AIOpenAI:    {"gpt-4.1", "gpt-4.1-mini", "gpt-4o"},
	AIGoogle:    {"gemini-2.5-flash", "gemini-2.5-pro"},
	AIMistral:   {"mistral-large-latest", "mistral-small-latest"},
	AIOllama:    {"qwen2.5", "llama3.1"},
}

// Feature names in RenderResult.Features; each maps to one data-wb-* behaviour of the runtime.
type Feature string

const (
	FeatureMenu    Feature = "menu"
	FeatureAcc     Feature = "acc"
	FeatureTabs    Feature = "tabs"
	FeatureDismiss Feature = "dismiss"
	FeatureForm    Feature = "form"
	FeatureMotion  Feature = "motion"
	FeatureEmbed   Feature = "embed"
)

// Runtime attributes (see the contract doc, "data-wb-* runtime").
const (
	AttrRoot        = "data-wb-root"
	AttrMenu        = "data-wb-menu"
	AttrAcc         = "data-wb-acc"
	AttrAccSingle   = "data-wb-acc-single"
	AttrTabs        = "data-wb-tabs"
	AttrTab         = "data-wb-tab"
	AttrPanel       = "data-wb-panel"
	AttrAutoplay    = "data-wb-autoplay"
	AttrDismiss     = "data-wb-dismiss"
	AttrDismissBtn  = "data-wb-dismiss-btn"
	AttrForm        = "data-wb-form"
	AttrFormStatus  = "data-wb-form-status"
	AttrState       = "data-wb-state"
	AttrMotion      = "data-wb-motion"
	AttrDur         = "data-wb-dur"
	AttrDelay       = "data-wb-delay"
	AttrEase        = "data-wb-ease"
	AttrRepeat      = "data-wb-repeat"
	AttrStagger     = "data-wb-stagger"
	AttrStaggerItem = "data-wb-stagger-item"
	AttrParallax    = "data-wb-parallax"
	AttrParallaxOn  = "data-wb-parallax-on"
	AttrScrollFade  = "data-wb-scroll-fade"
	AttrScrollScale = "data-wb-scroll-scale"
	AttrHover       = "data-wb-hover"
	AttrHoverOn     = "data-wb-hover-on"
	AttrSticky      = "data-wb-sticky"
	AttrEmbed       = "data-wb-embed"
	ClassJS         = "wb-js" // set on <html> before first paint when the runtime will run
	ClassIn         = "wb-in" // set by the runtime when a motion element has entered
	FormHoneypot    = "wb_hp"
	FormTiming      = "wb_t"
	FormNonce       = "wb_nonce" // signed token or session nonce, required on POST
	FormSentQuery   = "wb_form"  // ?wb_form=sent|error after a no-JS submit (303 back to the page)
	// A design's form bound to the form endpoint (codebind.go): the form carries data-wb-form and
	// data-wb-code-form="<key>"; the design's success and error elements data-wb-form-done and
	// data-wb-form-error with the same key.
	AttrCodeForm  = "data-wb-code-form"
	AttrFormDone  = "data-wb-form-done"
	AttrFormError = "data-wb-form-error"
)

// Form field names a data-wb-form may carry (input name attributes). A design's form bound to the
// endpoint (codebind.go) maps its inputs to these or to field_<name> (FormFieldPattern).
var FormFields = []string{"name", "email", "phone", "message", "date", "choice", "company", "subject"}

// ---------------------------------------------------------------------------------------------
// Document model (upstream src/builder/types.ts). Optional scalars are pointers so that an
// explicit 0 / false survives; optional strings use omitempty.

type Box4 struct {
	T float64 `json:"t"`
	R float64 `json:"r"`
	B float64 `json:"b"`
	L float64 `json:"l"`
}

type CellBox struct {
	Width   *float64 `json:"width,omitempty"`
	Padding *Box4    `json:"padding,omitempty"`
	Bg      string   `json:"bg,omitempty"`
	Radius  *float64 `json:"radius,omitempty"`
	Valign  string   `json:"valign,omitempty"` // start | center | end
	Halign  string   `json:"halign,omitempty"` // start | center | end
}

type MotionSettings struct {
	Entrance       string   `json:"entrance,omitempty"` // EntranceId
	Duration       *float64 `json:"duration,omitempty"`
	Delay          *float64 `json:"delay,omitempty"`
	Easing         string   `json:"easing,omitempty"` // smooth | ease | ease-out | ease-in-out | spring | linear
	Repeat         *bool    `json:"repeat,omitempty"`
	Stagger        *bool    `json:"stagger,omitempty"`
	StaggerDelay   *float64 `json:"staggerDelay,omitempty"`
	Parallax       *float64 `json:"parallax,omitempty"`       // -100…100
	ParallaxTarget string   `json:"parallaxTarget,omitempty"` // bg | content
	ScrollFade     *bool    `json:"scrollFade,omitempty"`
	ScrollScale    *bool    `json:"scrollScale,omitempty"`
	Sticky         *bool    `json:"sticky,omitempty"`
	Hover          string   `json:"hover,omitempty"`       // none | lift | glow | scale
	HoverTarget    string   `json:"hoverTarget,omitempty"` // self | items
}

// ElStyle merges every upstream `interface ElStyle` declaration.
// ElStyleMobile holds a free-form node's own phone sizes (patch 0017; unset = automatic scaling).
type ElStyleMobile struct {
	FontSize   *float64 `json:"fontSize,omitempty"`
	LineHeight *float64 `json:"lineHeight,omitempty"`
	Padding    *Box4    `json:"padding,omitempty"`
	Hide       *bool    `json:"hide,omitempty"` // not shown on phones
}

type ElStyle struct {
	M             *ElStyleMobile    `json:"m,omitempty"` // free-form nodes: sizes on phones
	Margin        *Box4             `json:"margin,omitempty"`
	Mt            *float64          `json:"mt,omitempty"`
	Mb            *float64          `json:"mb,omitempty"`
	Ml            *float64          `json:"ml,omitempty"`
	Mr            *float64          `json:"mr,omitempty"`
	Padding       *Box4             `json:"padding,omitempty"`
	FontSize      *float64          `json:"fontSize,omitempty"`
	FontWeight    *float64          `json:"fontWeight,omitempty"`
	LineHeight    *float64          `json:"lineHeight,omitempty"`
	LetterSpacing *float64          `json:"letterSpacing,omitempty"`
	Color         string            `json:"color,omitempty"`
	Bg            string            `json:"bg,omitempty"`
	Radius        *float64          `json:"radius,omitempty"`
	Align         string            `json:"align,omitempty"` // left | center | right
	Width         *float64          `json:"width,omitempty"`
	WidthUnit     string            `json:"widthUnit,omitempty"` // px | % | vw
	MaxWidth      *float64          `json:"maxWidth,omitempty"`
	MaxWidthUnit  string            `json:"maxWidthUnit,omitempty"` // px | %
	Height        *float64          `json:"height,omitempty"`
	HeightUnit    string            `json:"heightUnit,omitempty"` // px | vh
	Fit           string            `json:"fit,omitempty"`        // cover | contain
	IconColor     string            `json:"iconColor,omitempty"`
	IconColors    map[string]string `json:"iconColors,omitempty"`
	Font          string            `json:"font,omitempty"`
	Border        string            `json:"border,omitempty"`
	Shadow        string            `json:"shadow,omitempty"`
	Transform     string            `json:"transform,omitempty"` // none | uppercase | lowercase | capitalize
	Italic        *bool             `json:"italic,omitempty"`
	Underline     *bool             `json:"underline,omitempty"`
	BgImage       string            `json:"bgImage,omitempty"`
	BgSize        string            `json:"bgSize,omitempty"`
	BgPos         string            `json:"bgPos,omitempty"`
	List          string            `json:"list,omitempty"`
	Opacity       *float64          `json:"opacity,omitempty"`
	IconSize      *float64          `json:"iconSize,omitempty"`
	Tile          *bool             `json:"tile,omitempty"`
	TileSize      *float64          `json:"tileSize,omitempty"`
	TileOpacity   *float64          `json:"tileOpacity,omitempty"`
	Hide          *bool             `json:"hide,omitempty"`
	Motion        *MotionSettings   `json:"motion,omitempty"`
	Removed       *bool             `json:"removed,omitempty"`
}

type NodeLayout struct {
	Dir     string   `json:"dir,omitempty"` // row | column | grid
	Cols    *float64 `json:"cols,omitempty"`
	ColsT   *float64 `json:"colsT,omitempty"` // grids: columns on tablets (patch 0017)
	ColsM   *float64 `json:"colsM,omitempty"` // grids: columns on phones
	Gap     *float64 `json:"gap,omitempty"`
	GapY    *float64 `json:"gapY,omitempty"`
	Align   string   `json:"align,omitempty"`   // start | center | end | stretch
	Justify string   `json:"justify,omitempty"` // start | center | end | space-between
	Wrap    *bool    `json:"wrap,omitempty"`
	Stack   *bool    `json:"stack,omitempty"`
	Clip    *bool    `json:"clip,omitempty"`
	Menu    *bool    `json:"menu,omitempty"`
	Self    string   `json:"self,omitempty"` // start | center | end | stretch
	Basis   *float64 `json:"basis,omitempty"`
	Ratio   *float64 `json:"ratio,omitempty"`
	RatioM  *float64 `json:"ratioM,omitempty"` // images: width / height on phones (patch 0017)
	BasisM  *float64 `json:"basisM,omitempty"` // row cells: share of the row on phones (patch 0017)
}

type Item struct {
	ID     string      `json:"id"`
	Title  string      `json:"title"`
	Text   string      `json:"text"`
	Icon   string      `json:"icon,omitempty"`
	Box    *CellBox    `json:"box,omitempty"`
	Href   string      `json:"href,omitempty"`
	Node   string      `json:"node,omitempty"` // NodeType: box | heading | text | button | link | image | icon | input | divider
	Parent string      `json:"parent,omitempty"`
	Tag    string      `json:"tag,omitempty"`
	Lay    *NodeLayout `json:"lay,omitempty"`
	Sub    *Section    `json:"sub,omitempty"`
	// Dyn holds the settings of a dynamic layout cell (Title is one of DynWidgets); §11.4.
	Dyn *DynSettings `json:"dyn,omitempty"`
}

type ContainerBox struct {
	ContentWidth  string  `json:"contentWidth"` // boxed | full
	Width         float64 `json:"width"`
	WidthUnit     string  `json:"widthUnit"` // px | % | vw
	MinHeight     float64 `json:"minHeight"`
	MinHeightUnit string  `json:"minHeightUnit"` // px | vh
	Direction     string  `json:"direction"`     // row | column | row-reverse | column-reverse
	Justify       string  `json:"justify"`       // start | center | end | space-between | space-around | space-evenly
	Align         string  `json:"align"`         // stretch | start | center | end
	Wrap          bool    `json:"wrap"`
	StackMobile   bool    `json:"stackMobile"`
	GapX          float64 `json:"gapX"`
	GapY          float64 `json:"gapY"`
	GridCols      float64 `json:"gridCols"`
	GridRows      float64 `json:"gridRows"`
	Margin        Box4    `json:"margin"`
	Padding       Box4    `json:"padding"`
	BorderStyle   string  `json:"borderStyle"` // none | solid | dashed | dotted
	BorderWidth   float64 `json:"borderWidth"`
	BorderColor   string  `json:"borderColor"`
	Radius        float64 `json:"radius"`
	Shadow        string  `json:"shadow"` // none | sm | md | lg
	ZIndex        float64 `json:"zIndex"`
}

type SectionStyle struct {
	Palette       string          `json:"palette"` // brand | dark | light | soft | sand
	Align         string          `json:"align"`   // left | center
	Padding       float64         `json:"padding"`
	TitleSize     float64         `json:"titleSize"`
	TitleWeight   *float64        `json:"titleWeight,omitempty"`
	LetterSpacing *float64        `json:"letterSpacing,omitempty"`
	TitleColor    string          `json:"titleColor,omitempty"`
	BodySize      *float64        `json:"bodySize,omitempty"`
	Bg            string          `json:"bg,omitempty"`
	HideOn        []string        `json:"hideOn"` // Device: desktop | tablet | mobile
	CSSID         string          `json:"cssId,omitempty"`
	CSS           string          `json:"css,omitempty"`  // privileged (unfiltered_html)
	Anim          string          `json:"anim,omitempty"` // legacy: none | fade | up | zoom | left
	Motion        *MotionSettings `json:"motion,omitempty"`
}

// GridSettings is upstream Partial<GridSettings>: every field optional.
type GridSettings struct {
	Cols        *float64 `json:"cols,omitempty"`
	ColsT       *float64 `json:"colsT,omitempty"`
	ColsM       *float64 `json:"colsM,omitempty"`
	GapX        *float64 `json:"gapX,omitempty"`
	GapY        *float64 `json:"gapY,omitempty"`
	Pad         *float64 `json:"pad,omitempty"`
	Radius      *float64 `json:"radius,omitempty"`
	Bg          string   `json:"bg,omitempty"`
	Border      *bool    `json:"border,omitempty"`
	BorderW     *float64 `json:"borderW,omitempty"`
	BorderColor string   `json:"borderColor,omitempty"`
	Shadow      string   `json:"shadow,omitempty"` // none | sm | md | lg
	ShowIcon    *bool    `json:"showIcon,omitempty"`
	IconSize    *float64 `json:"iconSize,omitempty"`
	IconColor   string   `json:"iconColor,omitempty"`
}

// BgSettings is upstream Partial<BgSettings>: every field optional.
type BgSettings struct {
	Type         string   `json:"type,omitempty"` // color | gradient | image
	GradFrom     string   `json:"gradFrom,omitempty"`
	GradTo       string   `json:"gradTo,omitempty"`
	GradAngle    *float64 `json:"gradAngle,omitempty"`
	Image        string   `json:"image,omitempty"`
	Overlay      *float64 `json:"overlay,omitempty"`
	Decor        string   `json:"decor,omitempty"`    // none | globe | rings | waves | blob | dots | grid | custom
	DecorSVG     string   `json:"decorSvg,omitempty"` // sanitized to the SVG subset
	DecorColor   string   `json:"decorColor,omitempty"`
	DecorOpacity *float64 `json:"decorOpacity,omitempty"`
	DecorScale   *float64 `json:"decorScale,omitempty"`
	DecorX       *float64 `json:"decorX,omitempty"`
	DecorY       *float64 `json:"decorY,omitempty"`
	DecorRotate  *float64 `json:"decorRotate,omitempty"`
	DecorAnim    string   `json:"decorAnim,omitempty"` // none | spin | float | pulse
}

type Section struct {
	ID      string             `json:"id"`
	Kind    string             `json:"kind"` // one of SectionKinds
	Name    string             `json:"name"`
	Hidden  bool               `json:"hidden"`
	Content map[string]string  `json:"content"`
	Items   []Item             `json:"items"`
	Style   SectionStyle       `json:"style"`
	Box     *ContainerBox      `json:"box,omitempty"`
	Els     map[string]ElStyle `json:"els,omitempty"`
	Grid    *GridSettings      `json:"grid,omitempty"`
	Bg      *BgSettings        `json:"bg,omitempty"`
	// Local: a code section's local styles (local.go); privileged like content.html.
	Local LocalStyles `json:"local,omitempty"`
}

type SiteSettings struct {
	Brand         string   `json:"brand"`
	FontPair      string   `json:"fontPair"` // elegant | modern | plain
	Corners       string   `json:"corners"`  // sharp | soft | round
	HeadingFont   string   `json:"headingFont,omitempty"`
	HeadingWeight *float64 `json:"headingWeight,omitempty"`
	BodyFont      string   `json:"bodyFont,omitempty"`
	BodyWeight    *float64 `json:"bodyWeight,omitempty"`
	// Global: the design follows the site style (Boot.SiteStyle); the server puts the style's
	// values above into it whenever it is loaded.
	Global bool `json:"global,omitempty"`
}

type DocState struct {
	Sections []Section    `json:"sections"`
	Site     SiteSettings `json:"site"`
}

type PageSettings struct {
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Indexable   bool   `json:"indexable"`
	// Layout is PageLayoutSite or PageLayoutCanvas; "" = the default of DefaultPageLayout. It only
	// matters while Hello Pursae renders the front (§11.6). Templates ignore it.
	Layout string `json:"layout,omitempty"`
}

// SavedComponent is a library item; ID is the web_builder_library id as a decimal string.
type SavedComponent struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Section Section `json:"section"`
}

// SectionKinds is the upstream SectionKind union. KindHTML sections are privileged.
var SectionKinds = []string{"nav", "hero", "banner", "features", "media", "steps", "quote", "faq", "cta", "footer", "text", "image", "form", "layout", "html", "custom", "loop", "code"}

const (
	KindHTML = "html"
	// KindCode is a code section (docs/developer/web-builder-code-sections.md): the design's own
	// markup, privileged like KindHTML, top-level only. content.html is the body markup, published
	// inline (not in an iframe) in place of the snapshot's placeholder; content.head is markup for
	// the document head (stylesheets, preloads, head scripts), printed once per page; content.kit is
	// the design kit its files come from.
	KindCode   = "code"
	KindForm   = "form"
	KindCustom = "custom"
	KindLayout = "layout"
	// KindLoop is the Theme Builder post loop (§11.4), validated by doc.go.
	KindLoop   = "loop"
	KindNav    = "nav"
	KindFooter = "footer"
)

// ---------------------------------------------------------------------------------------------
// Render result (renderStatic in the editor) and publish.

// FontNeed is one font family the snapshot uses.
type FontNeed struct {
	Family  string `json:"family"`
	Weights []int  `json:"weights"`
	Italic  bool   `json:"italic"`
}

// RenderResult is what the upstream renderStatic(doc, opts) returns.
type RenderResult struct {
	HTML     string     `json:"html"`    // one root <div class="wb-page …" data-wb-root>…</div>
	CSS      string     `json:"css"`     // section custom CSS (style.css), scoped; privileged
	Classes  []string   `json:"classes"` // Tailwind candidate classes used by HTML
	Fonts    []FontNeed `json:"fonts"`
	Features []Feature  `json:"features"` // sorted, unique; empty = no runtime
	Renderer int        `json:"renderer"` // RendererVersion of the editor that rendered it
}

// DraftState is a user's autosave.
type DraftState struct {
	Doc     DocState     `json:"doc"`
	Page    PageSettings `json:"page"`
	BaseRev int          `json:"baseRev"`
	SavedAt string       `json:"savedAt"` // RFC 3339
}

// PostStatus values the builder sends; the server may answer with any core status.
const (
	StatusPublish = "publish"
	StatusDraft   = "draft"
	StatusPending = "pending"
	StatusPrivate = "private"
	// StatusFuture schedules the publish: PublishRequest.Date is when the page goes live. The
	// snapshot is stored at once; the core's publish_future_post cron flips the status.
	StatusFuture = "future"
)

type UserRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type LockInfo struct {
	User  UserRef `json:"user"`
	Since string  `json:"since"` // RFC 3339
}

// DocumentResponse answers GET RouteDocument (and is embedded in Boot).
type DocumentResponse struct {
	PostID   int64        `json:"postId"`
	PostType string       `json:"postType"`
	Status   string       `json:"status"`
	Date     string       `json:"date"`     // RFC 3339 publish date of the post ("" = none yet); in the future while Status is future
	Rev      int          `json:"rev"`      // 0 = never published from the builder
	Doc      *DocState    `json:"doc"`      // null = new page, the editor starts from its default
	Page     PageSettings `json:"page"`     // from the post when there is no doc yet
	Draft    *DraftState  `json:"draft"`    // the current user's autosave, only if newer than the doc
	Detached bool         `json:"detached"` // posts.content was edited outside the builder
	Modified string       `json:"modified"` // RFC 3339, posts.modified_at
	// DocModified is when the stored doc was last written (RFC 3339, "" without a doc); the editor
	// keeps a draft only when it is newer than this.
	DocModified string    `json:"docModified"`
	Renderer    int       `json:"renderer"` // renderer of the stored snapshot
	Lock        *LockInfo `json:"lock"`     // another user's live lock, else null
}

// AutosaveRequest is the body of PUT RouteAutosave.
type AutosaveRequest struct {
	Doc     DocState     `json:"doc"`
	Page    PageSettings `json:"page"`
	BaseRev int          `json:"baseRev"`
}

// AutosaveResponse: Rev is the current published rev, so the editor can tell when someone
// else published since BaseRev.
type AutosaveResponse struct {
	SavedAt string `json:"savedAt"`
	Rev     int    `json:"rev"`
}

// PublishRequest is the body of POST RoutePublish.
type PublishRequest struct {
	BaseRev    int          `json:"baseRev"`
	Status     string       `json:"status"`             // publish | draft | pending | private | future
	Date       string       `json:"date,omitempty"`     // RFC 3339; required with status future (a past date publishes now)
	Reattach   bool         `json:"reattach,omitempty"` // required on a detached page
	Doc        DocState     `json:"doc"`
	Page       PageSettings `json:"page"`
	Render     RenderResult `json:"render"`
	UtilityCSS string       `json:"utilityCss"` // Tailwind compile() of Render.Classes
}

// Stripped values name what the server removed from a publish or autosave.
const (
	StrippedHTMLSections = "html-sections"
	StrippedCustomCSS    = "custom-css"
	StrippedMarkup       = "markup" // the sanitizer changed the snapshot
	StrippedCSS          = "css"    // the CSS sanitizer dropped rules
)

type PublishResponse struct {
	Rev        int      `json:"rev"`
	Status     string   `json:"status"` // pending for users without publish_posts; future for a scheduled page
	Date       string   `json:"date"`   // RFC 3339 publish date ("" = none)
	Permalink  string   `json:"permalink"`
	RevisionID int64    `json:"revisionId"`
	Modified   string   `json:"modified"`
	Stripped   []string `json:"stripped"`
}

type LockRequest struct {
	Take bool `json:"take,omitempty"` // take over another user's lock
}

type LockResponse struct {
	Lock    *LockInfo `json:"lock"`    // the other holder, or null when the caller holds it
	Expires string    `json:"expires"` // RFC 3339, refresh before this
}

type RevisionSummary struct {
	ID       int64   `json:"id"` // post_revisions.id
	Rev      int     `json:"rev"`
	Author   UserRef `json:"author"`
	Date     string  `json:"date"`
	Title    string  `json:"title"`
	Renderer int     `json:"renderer"`
}

type RevisionResponse struct {
	ID   int64        `json:"id"`
	Rev  int          `json:"rev"`
	Doc  DocState     `json:"doc"`
	Page PageSettings `json:"page"`
}

// RestoreRequest: restoring writes the revision as the caller's draft and returns it; the
// editor then publishes it (a fresh snapshot needs the browser).
type RestoreRequest struct {
	BaseRev int `json:"baseRev"`
}

type LibraryItem struct {
	SavedComponent
	Author  UserRef `json:"author"`
	Created string  `json:"created"`
}

type LibraryCreateRequest struct {
	Name    string  `json:"name"`
	Section Section `json:"section"`
}

// Prefs is a user's editor preferences (recent fonts, layout width, …); keys match PrefKeyPattern.
type Prefs map[string]any

type ValidationError struct {
	Path    string `json:"path"` // JSON pointer into the request, e.g. /doc/sections/3/items/0/href
	Code    string `json:"code"` // ValidationCodes
	Message string `json:"message"`
}

const (
	VErrRequired  = "required"
	VErrKind      = "unknown_kind"
	VErrID        = "bad_id"
	VErrTooLong   = "too_long"
	VErrTooMany   = "too_many"
	VErrTooDeep   = "too_deep"
	VErrURL       = "bad_url"
	VErrCSS       = "bad_css" // e.g. "</style" in CSS
	VErrEnum      = "bad_value"
	VErrRenderer  = "bad_renderer"  // Render.Renderer != RendererVersion
	VErrCondition = "bad_condition" // a display condition rule (ErrBadCondition)
)

// FormResponse answers a JSON (Accept: application/json) form submit.
type FormResponse struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	Redirect string `json:"redirect,omitempty"` // absolute URL of this site to open after a sent form
}

// ---------------------------------------------------------------------------------------------
// Editor boot config: window.__WEB_BUILDER__ (JSON, "</" escaped as "<\/").

type BootREST struct {
	Root      string `json:"root"`      // absolute, ends with "/wp-json/"
	Namespace string `json:"namespace"` // RESTNamespace
	Nonce     string `json:"nonce"`     // wp_rest
}

type BootLinks struct {
	Site      string `json:"site"` // home URL, no trailing slash
	Permalink string `json:"permalink"`
	Preview   string `json:"preview"`
	Exit      string `json:"exit"`     // where the logo / Exit goes (the post's list screen)
	EditPost  string `json:"editPost"` // core editor of this post
	Settings  string `json:"settings"` // Web Builder › Settings, "" without manage_options
}

type BootCaps struct {
	Publish          bool `json:"publish"` // publish_posts / publish_pages for the type
	UploadFiles      bool `json:"uploadFiles"`
	UnfilteredHTML   bool `json:"unfilteredHtml"`   // html sections and custom CSS
	EditOthers       bool `json:"editOthers"`       // edit_others_posts: library delete of others' items
	EditThemeOptions bool `json:"editThemeOptions"` // edit_theme_options: the site settings dialog (site-settings route)
}

type BootFonts struct {
	CSSURL string   `json:"cssUrl"` // RouteFontsCSS absolute, with _wpnonce
	Local  []string `json:"local"`  // families already localized
}

type BootMedia struct {
	MaxUploadBytes int64    `json:"maxUploadBytes"`
	Types          []string `json:"types"` // accepted MIME types
}

type AIModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type AIProviderInfo struct {
	ID     AIProvider `json:"id"`
	Models []AIModel  `json:"models"`
}

// BootAI is present in every Boot. With AI off (PURSAE_BUILDER_AI=0) it is
// {"enabled":false,"providers":[],"llmBase":"","imagesUrl":"","allowCode":false}.
type BootAI struct {
	Enabled   bool             `json:"enabled"`
	Providers []AIProviderInfo `json:"providers"` // configured providers only; empty = demo engine
	LLMBase   string           `json:"llmBase"`   // absolute .../web-builder/v2/ai/llm/ ; "" when off
	ImagesURL string           `json:"imagesUrl"` // absolute RouteAIImages; "" when off
	AllowCode bool             `json:"allowCode"` // always false for now
}

// Boot is window.__WEB_BUILDER__.
type Boot struct {
	APIVersion      int              `json:"apiVersion"`
	RendererVersion int              `json:"rendererVersion"`
	Document        DocumentResponse `json:"document"`
	User            UserRef          `json:"user"`
	REST            BootREST         `json:"rest"`
	Links           BootLinks        `json:"links"`
	Caps            BootCaps         `json:"caps"`
	Locale          string           `json:"locale"` // WordPress locale, e.g. en_US, tr_TR
	Fonts           BootFonts        `json:"fonts"`
	Media           BootMedia        `json:"media"`
	SiteAssets      string           `json:"siteAssets"` // absolute SiteAssetsPath
	AI              BootAI           `json:"ai"`
	// Theme Builder (APIVersion 2). Template is null unless the post is a TemplatePostType.
	Template *TemplateBoot `json:"template"`
	Site     SiteBoot      `json:"site"`
	Dynamic  DynContext    `json:"dynamic"` // sample data for dynamic widgets (the template's preview object, else the post itself)
	// CodeParts: the header and footer templates the site draws around this page when they are
	// made of code sections (the element editor shows them, locked, in the page's frame); null for
	// a template, a canvas page, or when the active theme draws no parts.
	CodeParts *CodeParts `json:"codeParts"`
	// KitBreakpoints: the local-style breakpoints of every design kit the page's code sections and
	// its CodeParts use, by kit slug (a section without a kit uses DefaultBreakpoints).
	KitBreakpoints map[string]Breakpoints `json:"kitBreakpoints"`
	// SiteStyle: the site style designs can follow (SiteSettings.Global), null while none was
	// saved; saved through site-settings ("site_style", edit_theme_options).
	SiteStyle *SiteSettings `json:"siteStyle"`
}

// CodeParts are the header and footer templates around a page, as the editor draws them.
type CodeParts struct {
	Header *CodeTemplate `json:"header"` // null: no header template, or one not made of code sections
	Footer *CodeTemplate `json:"footer"`
}

// CodeTemplate is one template of CodeParts: its published code sections (Section JSON as stored).
type CodeTemplate struct {
	ID       int64             `json:"id"`
	Title    string            `json:"title"`
	EditURL  string            `json:"editUrl"` // absolute; opens the template in the editor
	Sections []json.RawMessage `json:"sections"`
}

// ---------------------------------------------------------------------------------------------
// AI proxy.

type AIStatusResponse struct {
	Enabled   bool             `json:"enabled"`
	Providers []AIProviderInfo `json:"providers"`
	Budget    AIBudget         `json:"budget"`
}

type AIBudget struct {
	Daily int64 `json:"daily"` // 0 = no cap
	Used  int64 `json:"used"`  // today, UTC
}

// AIImageRequest / AIImageResponse match upstream ai/images.ts (ImageQuery / ImageResult).
type AIImageRequest struct {
	Query       string `json:"query"`
	Count       int    `json:"count,omitempty"`       // 1…12, default 6
	Orientation string `json:"orientation,omitempty"` // landscape | portrait | square
}

type AIImage struct {
	URL     string `json:"url"` // Media Library URL after sideload
	MediaID int64  `json:"mediaId"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Alt     string `json:"alt"`
	Credit  string `json:"credit"`
	License string `json:"license"`
	Source  string `json:"source"` // original page URL
}

type AIImageResponse struct {
	Images []AIImage `json:"images"`
	Error  string    `json:"error,omitempty"`
}

// AIImageHosts are the only hosts /ai/images talks to (search APIs and image CDNs).
var AIImageHosts = []string{"api.openverse.org", "commons.wikimedia.org", "upload.wikimedia.org"}

// ---------------------------------------------------------------------------------------------
// Build output.

// ManifestEntry: paths are relative to the bundle directory.
type ManifestEntry struct {
	JS  string   `json:"js"`
	CSS []string `json:"css"`
}

// Manifest is web/web-builder/manifest.json.
type Manifest struct {
	Editor     ManifestEntry `json:"editor"`
	EditorNoAI ManifestEntry `json:"editor-noai"`
	Site       SiteManifest  `json:"site"`
}

type SiteManifest struct {
	Runtime string `json:"runtime"` // e.g. runtime.3f9a1c.js
	CSS     string `json:"css"`     // e.g. wb.8b2e4d.css (base CSS: .wb-page container, .wb-js rules, motion, hover)
}

// BuildInfo is web/web-builder/BUILD.json.
type BuildInfo struct {
	UpstreamSHA     string            `json:"upstreamSha"`
	Patches         map[string]string `json:"patches"` // file name → sha256 hex
	APIVersion      int               `json:"apiVersion"`
	RendererVersion int               `json:"rendererVersion"`
}

// ---------------------------------------------------------------------------------------------
// Sanitizer allowlist for snapshot HTML (S2 builds its bluemonday policy from these).

// AllowedElements are the HTML elements a snapshot may contain.
var AllowedElements = []string{
	"div", "section", "header", "footer", "nav", "main", "article", "aside",
	"h1", "h2", "h3", "h4", "h5", "h6", "p", "span", "a", "strong", "b", "em", "i", "u", "s",
	"small", "mark", "sub", "sup", "br", "hr", "ul", "ol", "li", "dl", "dt", "dd",
	"blockquote", "q", "cite", "figure", "figcaption", "img", "picture", "source", "video",
	"button", "form", "input", "textarea", "select", "option", "label", "fieldset", "legend",
	"details", "summary", "table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption",
	"time", "address", "code", "pre", "kbd", "iframe",
}

// AllowedSVGElements is the SVG subset (snapshot icons, decor layers, BgSettings.DecorSVG).
var AllowedSVGElements = []string{
	"svg", "g", "path", "circle", "ellipse", "line", "polyline", "polygon", "rect", "defs",
	"linearGradient", "radialGradient", "stop", "clipPath", "mask", "use", "title", "desc",
	"symbol", "text", "tspan",
}

// GlobalAttrs are allowed on every element, plus aria-* and data-wb-* (AttrPrefixes).
var GlobalAttrs = []string{"class", "style", "id", "title", "role", "lang", "dir", "hidden", "tabindex"}

var AttrPrefixes = []string{"aria-", "data-wb-"}

// ElementAttrs are per-element attributes on top of GlobalAttrs.
var ElementAttrs = map[string][]string{
	"a":        {"href", "target", "rel"},
	"img":      {"src", "srcset", "sizes", "alt", "width", "height", "loading", "decoding", "fetchpriority"},
	"source":   {"srcset", "sizes", "type", "media"},
	"video":    {"src", "poster", "controls", "autoplay", "muted", "loop", "playsinline", "preload", "width", "height"},
	"input":    {"type", "name", "placeholder", "required", "autocomplete", "value", "maxlength", "checked", "disabled", "readonly"},
	"textarea": {"name", "rows", "placeholder", "required", "maxlength"},
	"select":   {"name", "required"},
	"option":   {"value", "selected"},
	"label":    {"for"},
	"button":   {"type", "disabled"},
	"form":     {"method", "action", "novalidate"},
	"details":  {"open"},
	"time":     {"datetime"},
	"td":       {"colspan", "rowspan"},
	"th":       {"colspan", "rowspan", "scope"},
	"ol":       {"start", "reversed"},
	"iframe":   {"src", "srcdoc", "sandbox", "title", "loading", "allow", "allowfullscreen", "width", "height", "referrerpolicy"},
	"svg":      {"viewBox", "xmlns", "width", "height", "fill", "stroke", "stroke-width", "preserveAspectRatio", "focusable"},
}

// SVGAttrs are allowed on every AllowedSVGElements element.
var SVGAttrs = []string{
	"d", "cx", "cy", "r", "rx", "ry", "x", "y", "x1", "y1", "x2", "y2", "points", "width", "height",
	"transform", "opacity", "fill", "fill-opacity", "fill-rule", "clip-rule", "stroke",
	"stroke-width", "stroke-linecap", "stroke-linejoin", "stroke-opacity", "stroke-dasharray",
	"offset", "stop-color", "stop-opacity", "gradientUnits", "gradientTransform", "viewBox",
	"preserveAspectRatio", "clip-path", "mask", "href", "font-size", "font-weight", "text-anchor",
	"vector-effect",
}

// InputTypes are the allowed <input type> values.
var InputTypes = []string{"text", "email", "tel", "date", "hidden", "checkbox", "radio", "submit"}

// URLSchemes are allowed in href; src/srcset/poster allow only http, https, relative and
// data:image/* (DataImageTypes, at most MaxDataURIBytes). svg href/xlink:href, clip-path and
// mask allow only same-document references ("#id" / "url(#id)").
var URLSchemes = []string{"http", "https", "mailto", "tel"}

var DataImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/svg+xml"}

// VideoEmbedHosts are the iframe src hosts allowed for every author (https only, path /embed/… or /video/…).
var VideoEmbedHosts = []string{"www.youtube-nocookie.com", "www.youtube.com", "player.vimeo.com"}

// EmbedSandbox is the only sandbox value allowed on a srcdoc iframe (html sections; unfiltered_html
// authors only). allow-same-origin is never allowed.
const EmbedSandbox = "allow-scripts"

// ForbiddenElements are dropped with their content wherever they appear.
var ForbiddenElements = []string{"script", "style", "link", "meta", "base", "object", "embed", "applet", "frame", "frameset", "noscript", "template", "foreignObject", "animate", "animateMotion", "animateTransform", "set", "math", "portal"}

// RichTextElements are the inline tags upstream rich.ts keeps in content strings (a: href only).
var RichTextElements = []string{"b", "i", "u", "s", "code", "a", "br", "ul", "ol", "li"}

// CSS sanitizer rules.
var (
	// CSSAllowedAtRules; anything else (@import, @charset, @namespace, @document, …) is dropped.
	CSSAllowedAtRules = []string{"media", "container", "supports", "keyframes", "font-face", "layer", "property"}
	// CSSForbiddenTokens drop the whole declaration (matched case-insensitively after unescaping).
	CSSForbiddenTokens = []string{"expression(", "behavior", "-moz-binding", "javascript:", "vbscript:", "-o-link", "@import"}
)

// CSSFontFacePrefix: @font-face src url() must start with this (after the uploads base URL).
const CSSFontFacePrefix = "/wp-content/uploads/web-builder/fonts/"

// ---------------------------------------------------------------------------------------------
// Theme Builder (contract doc §11). Header, footer and main-content templates are posts of
// TemplatePostType edited in the builder; display conditions pick one per location and request.

// Identity: post type, meta keys, options, theme, query parameters, cache tags.
const (
	// TemplatePostType is the post type of a template (not public, no core editor, no archive).
	TemplatePostType = "wb_template"
	// TemplateCap guards every template path (list, create, edit, publish, conditions, trash):
	// caps.MapMetaCap maps wb_template to *_posts, so edit_post alone would admit authors.
	TemplateCap = "edit_theme_options"

	MetaTemplateType = "_wb_template_type" // one of TemplateTypes
	MetaConditions   = "_wb_conditions"    // JSON array of condition rules, e.g. ["include/general"]
	MetaPriority     = "_wb_priority"      // decimal PriorityMin…PriorityMax, default 0
	MetaPreview      = "_wb_preview"       // preview object (PreviewObject grammar), "" = the type's default
	MetaStarter      = "_wb_starter"       // the StarterID the template was created from
	MetaPopup        = "_wb_popup"         // popup templates: PopupSettings as JSON, "" = the defaults

	// HelloPursaeTheme is the bundled blank-canvas theme that renders the template parts.
	HelloPursaeTheme = "hello-pursae"
	// OptionPreviousTheme holds the stylesheet that was active before the plugin switched to
	// Hello Pursae; deactivation switches back to it (never golementor_previous_theme).
	OptionPreviousTheme = "web_builder_previous_theme"
	// OptionThemeNotice is the one-shot admin notice after the automatic switch.
	OptionThemeNotice = "web_builder_theme_notice"
	// OptionThemeOffer is true once the "Switch to Hello Pursae" offer is done: activation sets it,
	// and so do the offer's Dismiss and Switch links, shown to a site that had the plugin on before
	// the Theme Builder existed (such sites are never switched silently).
	OptionThemeOffer = "web_builder_theme_offer"

	// MenuLocationPrimary and MenuLocationFooter are Hello Pursae's menu locations.
	MenuLocationPrimary = "primary"
	MenuLocationFooter  = "footer"

	// PreviewQuery forces a template on a front URL (?wb_template_preview=<id>): TemplateCap
	// only, never cached.
	PreviewQuery = "wb_template_preview"

	// CacheTagTheme is on every page Hello Pursae renders with a template part; publishing a
	// template or changing its conditions purges everything (wbTemplatesChanged).
	CacheTagTheme = "wb-theme"
	// CacheTagLoop is on every page whose parts run a post loop; saving any public post purges it.
	CacheTagLoop = "wb-loop"

	// ThemeBuilderPage is Web Builder › Theme Builder: /wp-admin/themes?page=web-builder-templates.
	ThemeBuilderPage = "web-builder-templates"

	// PartCSSIDPrefix: each part's CSS is inlined as <style id="wb-css-<postID>">.
	PartCSSIDPrefix = "wb-css-"
	// ClassPart / ClassPartPrefix wrap each part on the front: div.wb-part.wb-part-<location>.
	ClassPart       = "wb-part"
	ClassPartPrefix = "wb-part-"
	// BodyClassSite is added to <body> when at least one part renders.
	BodyClassSite = "wb-site"
)

// TemplateType is the _wb_template_type of a template.
type TemplateType string

const (
	TemplateHeader     TemplateType = "header"
	TemplateFooter     TemplateType = "footer"
	TemplateSinglePost TemplateType = "single-post"
	TemplateSinglePage TemplateType = "single-page"
	TemplateSingle     TemplateType = "single" // a custom post type's single view; needs a postType at create
	TemplateFrontPage  TemplateType = "front-page"
	TemplateArchive    TemplateType = "archive"
	TemplateSearch     TemplateType = "search-results"
	Template404        TemplateType = "error-404"
	// TemplatePopup is a popup (modal) printed before </body> on every view its conditions match;
	// several popups can show on one page. Its trigger and frequency are PopupSettings.
	TemplatePopup TemplateType = "popup"
	// Pursae Commerce store views (StoreTemplateKinds); offered only while Pursae Commerce is on.
	TemplateSingleProduct  TemplateType = "single-product"
	TemplateProductArchive TemplateType = "product-archive"
)

// TemplateTypes in display order (Theme Builder tabs, Add New select).
var TemplateTypes = []TemplateType{TemplateHeader, TemplateFooter, TemplateSinglePost, TemplateSinglePage, TemplateSingle, TemplateSingleProduct, TemplateFrontPage, TemplateArchive, TemplateProductArchive, TemplateSearch, Template404, TemplatePopup}

// TemplateTypeLabels are the English UI labels (the admin and the editor badge).
var TemplateTypeLabels = map[TemplateType]string{
	TemplateHeader: "Header", TemplateFooter: "Footer", TemplateSinglePost: "Single Post",
	TemplateSinglePage: "Single Page", TemplateSingle: "Single", TemplateFrontPage: "Front Page",
	TemplateArchive: "Archive", TemplateSearch: "Search Results", Template404: "404 Page", TemplatePopup: "Popup",
	TemplateSingleProduct: "Single Product", TemplateProductArchive: "Product Archive",
}

// StoreTemplateKinds are the main template types that render on Pursae Commerce store views, with
// the store view kinds (QueryFacts.StoreKind: siteQuery.Store) each may render on. Every other
// main type never renders on a store view, and these types never render anywhere else; the cart,
// checkout and account pages get no main template.
var StoreTemplateKinds = map[TemplateType][]string{
	TemplateSingleProduct:  {StoreKindProduct},
	TemplateProductArchive: {StoreKindShop, StoreKindProductCat, StoreKindProductTag, StoreKindProductAttr},
}

// Store view kinds (siteQuery.Store) of the Pursae Commerce front.
const (
	StoreKindProduct     = "product"
	StoreKindShop        = "shop"
	StoreKindProductCat  = "product_cat"
	StoreKindProductTag  = "product_tag"
	StoreKindProductAttr = "product_attr"
	StoreKindCart        = "cart"
	StoreKindCheckout    = "checkout"
	StoreKindAccount     = "myaccount"
)

// IsStoreTemplate reports whether t is a store main template type (StoreTemplateKinds).
func IsStoreTemplate(t TemplateType) bool {
	_, ok := StoreTemplateKinds[t]
	return ok
}

// Template locations: header and footer types fill their own location, every other type fills
// the main location.
const (
	LocationHeader = "header"
	LocationMain   = "main"
	LocationFooter = "footer"
	LocationPopup  = "popup" // popup templates: any number can fill it on one page
)

// TemplateLocation returns the location a template type fills.
func TemplateLocation(t TemplateType) string {
	switch t {
	case TemplateHeader:
		return LocationHeader
	case TemplateFooter:
		return LocationFooter
	case TemplatePopup:
		return LocationPopup
	}
	return LocationMain
}

// View kinds of the front request, as the condition resolver sees them (QueryFacts.View).
const (
	ViewSingular = "singular" // a single post, page or custom post type entry (also a static front page)
	ViewArchive  = "archive"  // posts index, term, author, date, post type archive, search
	View404      = "404"
)

// TemplateTypeViews gates main templates by view before conditions are scored: a single
// template never renders an archive even with include/general. front-page also renders a
// front page that is the posts index. Header and footer render on every view.
var TemplateTypeViews = map[TemplateType][]string{
	TemplateHeader:     {ViewSingular, ViewArchive, View404},
	TemplateFooter:     {ViewSingular, ViewArchive, View404},
	TemplateSinglePost: {ViewSingular},
	TemplateSinglePage: {ViewSingular},
	TemplateSingle:     {ViewSingular},
	TemplateFrontPage:  {ViewSingular, ViewArchive},
	TemplateArchive:    {ViewArchive},
	TemplateSearch:     {ViewArchive},
	Template404:        {View404},
	TemplatePopup:      {ViewSingular, ViewArchive, View404},

	TemplateSingleProduct:  {ViewSingular},
	TemplateProductArchive: {ViewArchive},
}

// TemplateTypeArchives narrows the archive view gate: a type listed here renders on the archive
// view only for these archive kinds (QueryFacts.Archive). search-results never renders a term,
// date, author or post type archive, whatever its rules say.
var TemplateTypeArchives = map[TemplateType][]string{
	TemplateSearch:         {ArchiveSearch},
	TemplateProductArchive: {ArchivePostType, ArchiveTerm},
}

// DefaultConditions are written when a template is created. TemplateSingle has none here: its
// default is "include/singular/<postType>" from the create request.
var DefaultConditions = map[TemplateType][]string{
	TemplateHeader:     {"include/general"},
	TemplateFooter:     {"include/general"},
	TemplateSinglePost: {"include/singular/post"},
	TemplateSinglePage: {"include/singular/page"},
	TemplateFrontPage:  {"include/singular/front_page"},
	TemplateArchive:    {"include/archive"},
	TemplateSearch:     {"include/archive/search"},
	Template404:        {"include/404"},
	TemplatePopup:      {"include/general"},

	TemplateSingleProduct:  {"include/singular/product"},
	TemplateProductArchive: {"include/archive/type/product", "include/archive/product_cat", "include/archive/product_tag"},
}

// SearchTypeBonus is added to a search-results template's score on a search view, so it beats
// an archive template whose rule scores the same (include/archive on both).
const SearchTypeBonus = 1

// StarterID names a starter design (upstream dynamic/starters.ts) loaded into an empty template.
type StarterID string

const (
	StarterBlank   StarterID = "blank"
	StarterHeader  StarterID = "header"
	StarterFooter  StarterID = "footer"
	StarterSingle  StarterID = "single"
	StarterArchive StarterID = "archive"
	StarterSearch  StarterID = "search"

	StarterProduct        StarterID = "product"
	StarterProductArchive StarterID = "product-archive"
)

var StarterIDs = []StarterID{StarterBlank, StarterHeader, StarterFooter, StarterSingle, StarterArchive, StarterSearch, StarterProduct, StarterProductArchive}

// DefaultStarters: the starter a create request without `starter` gets.
var DefaultStarters = map[TemplateType]StarterID{
	TemplateHeader: StarterHeader, TemplateFooter: StarterFooter, TemplateSinglePost: StarterSingle,
	TemplateSinglePage: StarterSingle, TemplateSingle: StarterSingle, TemplateFrontPage: StarterBlank,
	TemplateArchive: StarterArchive, TemplateSearch: StarterSearch, Template404: StarterBlank, TemplatePopup: StarterBlank,
	TemplateSingleProduct: StarterProduct, TemplateProductArchive: StarterProductArchive,
}

// Display conditions. A rule is "<mode>/<scope>"; see the grammar in the contract doc §11.2.
const (
	CondInclude = "include"
	CondExclude = "exclude"

	MaxConditions     = 50  // rules per template
	MaxConditionBytes = 200 // bytes per rule
	PriorityMin       = -100
	PriorityMax       = 100

	// ConditionPattern is the whole grammar as one regular expression (the parser in
	// conditions.go is the authority; this is for clients and tests). <ptype> and <tax> match
	// SlugPattern minus the reserved words of the same position, <id> is a positive decimal.
	ConditionPattern = `^(include|exclude)/(general|404|singular(/front_page|/[a-z0-9_-]{1,32}(/[1-9][0-9]{0,18}|/in/[a-z0-9_-]{1,32}/[1-9][0-9]{0,18}|/child_of/[1-9][0-9]{0,18}|/by_author/[1-9][0-9]{0,18})?)?|archive(/home|/date|/search|/author(/[1-9][0-9]{0,18})?|/type/[a-z0-9_-]{1,32}|/[a-z0-9_-]{1,32}(/[1-9][0-9]{0,18})?)?|user/(logged_in|logged_out|role/[a-z0-9_-]{1,32}))$`
	SlugPattern      = `^[a-z0-9_-]{1,32}$`
)

// Scope words that cannot be a post type (after "singular/") or a taxonomy (after "archive/").
var (
	ReservedSingularWords = []string{"front_page", "in", "child_of", "by_author"}
	ReservedArchiveWords  = []string{"home", "date", "search", "author", "type"}
)

// Condition scores: the best matching include rule scores the template; any matching exclude
// rule vetoes it. Ties: score, then priority (higher), then modified_at (newer), then id (higher).
const (
	ScoreGeneral    = 10 // general
	ScoreView       = 20 // singular, archive
	ScoreType       = 30 // singular/<ptype>, archive/home|date|search|author|type/<ptype>|<tax>
	ScoreRelation   = 40 // singular/<ptype>/in/<tax>/<id>, …/child_of/<id>, …/by_author/<id>
	ScoreSpecial    = 45 // singular/front_page, 404
	ScoreSpecificID = 50 // singular/<ptype>/<id>, archive/<tax>/<id>, archive/author/<id>
)

// Preview objects (MetaPreview, ?object= of RoutePreviewContext, TemplatePreviewRequest):
// post:<id> | term:<tax>:<id> | author:<id> | type:<ptype> | search:<q> | front | home | 404 |
// latest. front is the site's front page ("/"), home the posts index (is_home), type:<ptype>
// the post type archive. latest is template-relative: single-post → the newest published post,
// single-page → the static front page or else the newest page, single → the newest entry of
// the template's post type (the first include/singular/<ptype> rule), single-product → the newest
// published product; elsewhere it is front.
const (
	PreviewPost   = "post:"
	PreviewTerm   = "term:"
	PreviewAuthor = "author:"
	PreviewType   = "type:"
	PreviewSearch = "search:"
	PreviewFront  = "front"
	PreviewHome   = "home"
	Preview404    = "404"
	PreviewLatest = "latest"

	MaxPreviewObjectBytes = 200
	MaxPreviewSearchRunes = 100
)

// DefaultPreviews is the preview object when MetaPreview is empty.
var DefaultPreviews = map[TemplateType]string{
	TemplateHeader: PreviewFront, TemplateFooter: PreviewFront, TemplateSinglePost: PreviewLatest,
	TemplateSinglePage: PreviewLatest, TemplateSingle: PreviewLatest, TemplateFrontPage: PreviewFront,
	TemplateArchive: PreviewHome, TemplateSearch: PreviewSearch + "a", Template404: Preview404, TemplatePopup: PreviewFront,
	TemplateSingleProduct: PreviewLatest, TemplateProductArchive: PreviewType + "product",
}

// Page layout (PageSettings.Layout) while Hello Pursae renders the front.
const (
	PageLayoutSite   = "site"   // the page shows the header and footer templates around its doc
	PageLayoutCanvas = "canvas" // the page is the whole document (its own nav/footer sections)
)

// DefaultPageLayout is the layout of a page whose PageSettings.Layout is "": canvas when the doc
// has a top-level nav or footer section (pages built before the Theme Builder), else site.
func DefaultPageLayout(d *DocState) string {
	if d != nil {
		for _, s := range d.Sections {
			if s.Kind == KindNav || s.Kind == KindFooter {
				return PageLayoutCanvas
			}
		}
	}
	return PageLayoutSite
}

// ---------------------------------------------------------------------------------------------
// Dynamic widgets: layout cells (Item.Title) whose content comes from the site at request time.

// DynWidget is a dynamic CellType.
type DynWidget string

const (
	DynSiteLogo     DynWidget = "site-logo"
	DynSiteTitle    DynWidget = "site-title"
	DynSiteTagline  DynWidget = "site-tagline"
	DynNavMenu      DynWidget = "nav-menu"
	DynSearchForm   DynWidget = "search-form"
	DynCopyright    DynWidget = "copyright"
	DynPostTitle    DynWidget = "post-title"
	DynPostContent  DynWidget = "post-content"
	DynPostExcerpt  DynWidget = "post-excerpt"
	DynPostImage    DynWidget = "post-image"
	DynPostMeta     DynWidget = "post-meta"
	DynPostTerms    DynWidget = "post-terms"
	DynPostDate     DynWidget = "post-date"
	DynPostAuthor   DynWidget = "post-author"
	DynAuthorBox    DynWidget = "author-box"
	DynPostNav      DynWidget = "post-nav"
	DynPostComments DynWidget = "post-comments"
	DynArchiveTitle DynWidget = "archive-title"
	DynArchiveDesc  DynWidget = "archive-desc"
	DynSearchQuery  DynWidget = "search-query"
	DynPagination   DynWidget = "pagination"
	// DynLanguageSwitcher is the Pursae Multi Language switcher (site group; empty while the
	// plugin is off). Its settings come from the stored doc through data-wb-item.
	DynLanguageSwitcher DynWidget = "language-switcher"

	// Pursae Commerce widgets (§11.12): empty while Pursae Commerce is off. Their settings come
	// from the stored doc through data-wb-item, never from the snapshot.
	DynShopProducts     DynWidget = "shop-products"
	DynShopCategories   DynWidget = "shop-categories"
	DynShopAddToCart    DynWidget = "shop-add-to-cart"
	DynShopCart         DynWidget = "shop-cart"
	DynShopMiniCart     DynWidget = "shop-mini-cart"
	DynShopNotices      DynWidget = "shop-notices"
	DynShopCartPage     DynWidget = "shop-cart-page"
	DynShopCheckoutPage DynWidget = "shop-checkout-page"

	DynProductTitle     DynWidget = "product-title"
	DynProductImages    DynWidget = "product-images"
	DynProductPrice     DynWidget = "product-price"
	DynProductAddToCart DynWidget = "product-add-to-cart"
	DynProductRating    DynWidget = "product-rating"
	DynProductStock     DynWidget = "product-stock"
	DynProductMeta      DynWidget = "product-meta"
	DynProductShortDesc DynWidget = "product-short-desc"
	DynProductContent   DynWidget = "product-content"
	DynProductTabs      DynWidget = "product-tabs"
	DynProductRelated   DynWidget = "product-related"
	DynProductUpsells   DynWidget = "product-upsells"
	DynProductSaleBadge DynWidget = "product-sale-badge"

	DynShopArchiveProducts DynWidget = "shop-archive-products"
	DynShopArchiveTitle    DynWidget = "shop-archive-title"
	DynShopArchiveDesc     DynWidget = "shop-archive-desc"
	DynShopBreadcrumb      DynWidget = "shop-breadcrumb"
)

var DynWidgets = []DynWidget{DynSiteLogo, DynSiteTitle, DynSiteTagline, DynNavMenu, DynSearchForm, DynCopyright, DynPostTitle, DynPostContent, DynPostExcerpt, DynPostImage, DynPostMeta, DynPostTerms, DynPostDate, DynPostAuthor, DynAuthorBox, DynPostNav, DynPostComments, DynArchiveTitle, DynArchiveDesc, DynSearchQuery, DynPagination,
	DynLanguageSwitcher,
	DynShopProducts, DynShopCategories, DynShopAddToCart, DynShopCart, DynShopMiniCart, DynShopNotices, DynShopCartPage, DynShopCheckoutPage,
	DynProductTitle, DynProductImages, DynProductPrice, DynProductAddToCart, DynProductRating, DynProductStock, DynProductMeta, DynProductShortDesc, DynProductContent, DynProductTabs, DynProductRelated, DynProductUpsells, DynProductSaleBadge,
	DynShopArchiveProducts, DynShopArchiveTitle, DynShopArchiveDesc, DynShopBreadcrumb}

// ShopWidgets are the Pursae Commerce widgets (§11.12).
var ShopWidgets = []DynWidget{DynShopProducts, DynShopCategories, DynShopAddToCart, DynShopCart, DynShopMiniCart, DynShopNotices, DynShopCartPage, DynShopCheckoutPage,
	DynProductTitle, DynProductImages, DynProductPrice, DynProductAddToCart, DynProductRating, DynProductStock, DynProductMeta, DynProductShortDesc, DynProductContent, DynProductTabs, DynProductRelated, DynProductUpsells, DynProductSaleBadge,
	DynShopArchiveProducts, DynShopArchiveTitle, DynShopArchiveDesc, DynShopBreadcrumb}

// StaticCellTypes are the upstream CellType values ("" is a plain text cell); a layout cell's
// Title is one of these or one of DynWidgets.
var StaticCellTypes = []string{"", "heading", "text", "button", "image", "layout"}

// DynSettings is Item.Dyn: the settings of a dynamic cell. Every field is optional; the server
// re-validates the ones that reach the snapshot as data-wb-* attributes.
type DynSettings struct {
	Tag         string   `json:"tag,omitempty"`         // DynTags
	Link        *bool    `json:"link,omitempty"`        // wrap in the post / site URL
	Format      string   `json:"format,omitempty"`      // WordPress date format, DynFormatPattern; "" = date_format
	Size        string   `json:"size,omitempty"`        // DynImageSizes
	Ratio       *float64 `json:"ratio,omitempty"`       // image aspect ratio (width / height), 0.2…5
	Fields      []string `json:"fields,omitempty"`      // post-meta: DynMetaFields, in order
	Sep         string   `json:"sep,omitempty"`         // separator, ≤ MaxDynSepRunes
	Tax         string   `json:"tax,omitempty"`         // post-terms taxonomy (SlugPattern), default category
	Location    string   `json:"location,omitempty"`    // nav-menu: a menu location of the theme
	MenuLayout  string   `json:"menuLayout,omitempty"`  // DynMenuLayouts
	BurgerAt    *float64 `json:"burgerAt,omitempty"`    // container width (px) below which a row menu becomes a burger, 0 = never
	Words       *float64 `json:"words,omitempty"`       // excerpt length, MinDynWords…MaxDynWords
	Prefix      string   `json:"prefix,omitempty"`      // text before the value ("Posted on", "Search results for:")
	Placeholder string   `json:"placeholder,omitempty"` // search-form input placeholder
	Button      string   `json:"button,omitempty"`      // search-form button label, "" = icon only
	Empty       string   `json:"empty,omitempty"`       // text when the value is empty, ≤ MaxDynEmptyRunes

	// language-switcher
	Dropdown    *bool `json:"dropdown,omitempty"`    // language-switcher: a select; shop-cart: the mini cart in a disclosure panel
	Flags       *bool `json:"flags,omitempty"`       // language-switcher: show flags
	Names       *bool `json:"names,omitempty"`       // language-switcher: show names (default true)
	HideCurrent *bool `json:"hideCurrent,omitempty"` // language-switcher: leave out the current language

	// Pursae Commerce widgets (§11.12).
	Source    string   `json:"source,omitempty"`    // shop-products: ShopSources, default recent
	Columns   *float64 `json:"columns,omitempty"`   // shop-products, shop-categories: 1…MaxShopColumns
	Limit     *float64 `json:"limit,omitempty"`     // shop-products, shop-categories: 1…MaxShopLimit
	OrderBy   string   `json:"orderBy,omitempty"`   // shop-products: ShopOrderBys
	Order     string   `json:"order,omitempty"`     // shop-products: LoopOrders
	Cats      string   `json:"cats,omitempty"`      // product_cat ids, ShopIDsPattern (≤ MaxShopIDs)
	Tags      string   `json:"tags,omitempty"`      // product_tag ids, ShopIDsPattern
	IDs       string   `json:"ids,omitempty"`       // shop-products source ids: product ids, ShopIDsPattern
	Product   *float64 `json:"product,omitempty"`   // product.* widgets, shop-add-to-cart: a product id; 0 / absent = the current product
	Paginate  *bool    `json:"paginate,omitempty"`  // shop-products: ?product-page= paging
	ShowPrice *bool    `json:"showPrice,omitempty"` // shop-add-to-cart: the price before the button
	HideEmpty *bool    `json:"hideEmpty,omitempty"` // shop-categories: leave out empty categories
	Parent    string   `json:"parent,omitempty"`    // shop-categories: "" every level, "0" top level, or a category id
	ShowTotal *bool    `json:"showTotal,omitempty"` // shop-cart: the subtotal after the count
	Gallery   *bool    `json:"gallery,omitempty"`   // product-images: false = the featured image only
}

// Pursae Commerce widget settings.
var (
	ShopSources  = []string{"recent", "featured", "sale", "best-selling", "top-rated", "ids"}
	ShopOrderBys = []string{"date", "title", "price", "price-desc", "popularity", "rating", "rand", "menu_order"}
)

const (
	MaxShopColumns = 6
	MaxShopLimit   = 24
	MaxShopIDs     = 24
	ShopIDsPattern = `^[1-9][0-9]{0,18}(,[1-9][0-9]{0,18}){0,23}$`
)

var (
	DynTags        = []string{"h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "span"}
	DynImageSizes  = []string{"thumbnail", "medium", "large", "full"}
	DynMetaFields  = []string{"date", "modified", "author", "comments", "terms"}
	DynMenuLayouts = []string{"row", "column", "burger"}
)

const (
	DynFormatPattern = `^[A-Za-z ,.:/-]{1,32}$`
	MaxDynSepRunes   = 8
	MaxDynEmptyRunes = 200
	MinDynWords      = 5
	MaxDynWords      = 100
	MinDynRatio      = 0.2
	MaxDynRatio      = 5
)

// Loop section (Section.Kind == KindLoop): the query is Section.Content, the card design is
// Items[0].Sub (a layout section), the grid is Section.Grid. The server always reads the query
// from the STORED doc, never from snapshot attributes.
const (
	LoopSource     = "source"     // LoopSources, default main
	LoopPostType   = "postType"   // query/related: a viewable post type, default post
	LoopPerPage    = "perPage"    // "1"…"24"; main: ignored (posts_per_page)
	LoopOrderBy    = "orderBy"    // LoopOrderBys, default date
	LoopOrder      = "order"      // desc | asc, default desc
	LoopTerms      = "terms"      // "category:12,post_tag:4", ≤ MaxLoopTerms pairs
	LoopAuthor     = "author"     // "<user id>" | ""
	LoopExclude    = "exclude"    // "current" | "" (leave out the singular post being viewed)
	LoopOffset     = "offset"     // "0"…"50"
	LoopPagination = "pagination" // LoopPaginations; only for source main
	LoopEmpty      = "empty"      // text when the loop has no posts, ≤ MaxDynEmptyRunes
	// LoopParent: query loops of code bindings only: the children of that entry ("<id>" | "").
	LoopParent = "parent"

	LoopSourceMain    = "main"    // the request's own posts (v.Posts, v.Pagination)
	LoopSourceQuery   = "query"   // a custom query from the keys above
	LoopSourceRelated = "related" // same post type, sharing terms of LoopTerms' taxonomies (default category) with the current post

	MaxLoopPerPage    = 24
	MaxLoopTerms      = 20
	MaxLoopOffset     = 50
	MaxLoopDepth      = 2   // loops nested in loops
	MaxLoopIterations = 50  // items per loop per request
	CompiledCacheSize = 256 // LRU of compiled snapshots, key postID:rev:sha256(html)[:16]
)

// CompiledCacheBytes bounds the snapshot text the compiled-snapshot LRU keeps (imported designs'
// pages are 100-200 KB each).
const CompiledCacheBytes = 32 << 20

var (
	LoopSources     = []string{LoopSourceMain, LoopSourceQuery, LoopSourceRelated}
	LoopOrderBys    = []string{"date", "modified", "title", "menu_order", "comment_count", "rand"}
	LoopOrders      = []string{"desc", "asc"}
	LoopPaginations = []string{"numbers", "prev-next", "none"}
)

// nav / footer section binding (Section.Content keys): the links come from a menu location
// (read-only in the editor) and the logo from the site's custom logo.
const (
	ContentMenu       = "menu"       // a menu location, e.g. MenuLocationPrimary; "" = the section's own links
	ContentLogoSource = "logoSource" // "site" = the custom logo / site title; "" = the section's own logo
	LogoSourceSite    = "site"
)

// Snapshot placeholder attributes (data-wb-*; the sanitizer already keeps them). In live mode
// the renderer emits placeholders only, never sample values; the server fills them per request.
const (
	AttrSlot      = "data-wb-slot"      // value: a SlotName; the element's content or attributes are filled
	AttrIf        = "data-wb-if"        // value: a SlotName; the element is removed when the slot is empty
	AttrIfNot     = "data-wb-if-not"    // value: a SlotName; the element is removed when the slot is not empty
	AttrLoop      = "data-wb-loop"      // value: the loop section id (the query comes from the stored doc)
	AttrLoopItem  = "data-wb-loop-item" // exactly one card template inside a data-wb-loop / data-wb-menu-loc element
	AttrLoopEmpty = "data-wb-loop-empty"
	// AttrMenuLoc binds a link list to a menu location. Not data-wb-menu: that is the runtime's
	// disclosure menu (<details data-wb-menu>) and its feature detection.
	AttrMenuLoc  = "data-wb-menu-loc"
	AttrLoopPart = "data-wb-loop-part" // "1/2" | "2/2": split one menu across two lists (split-logo navs)
	// AttrText fills only the element's own text: its first direct text node that is not blank
	// gets the (escaped) value of a text slot, its other children stay (a menu button's label
	// beside its chevron icon). Content bindings of code sections (codebind.go).
	AttrText = "data-wb-text"
	// AttrMenuSub, on an element inside a menu item template, repeats its data-wb-loop-item
	// children per sub-item of that item (a design's dropdown list).
	AttrMenuSub = "data-wb-menu-sub"
	// AttrMenuTree, on a data-wb-menu-loc list of a code section: the list shows the top-level
	// items only (each picks its template; sub-items go to its data-wb-menu-sub list). The value
	// "dropdown" (a builder nav's drop-down menu) puts all the descendants of a top-level item, depth
	// first, into its data-wb-menu-sub list, their links marked wb-menu-d1 / wb-menu-d2.
	AttrMenuTree = "data-wb-menu-tree"
	// AttrRaw on a post.title / post.content / post.image slot element: the stored value as the
	// design shows it (title without texturizing, imported HTML content as stored, image src and
	// alt only). Set by the code bindings.
	AttrRaw = "data-wb-raw"
	// AttrMeta on a post.field* / post.image slot element: the field may fall back to the post's
	// own (public) meta value of that name when no Pursae Custom Fields field has it (what a site
	// import stores, webbuilder.ConvertGroups). Set by the code bindings only: the snapshot
	// sanitizer drops it, so an author's own markup reads registered fields only.
	AttrMeta   = "data-wb-meta"
	AttrFormat = "data-wb-format" // DynSettings.Format
	AttrSize   = "data-wb-size"   // DynSettings.Size
	AttrTax    = "data-wb-tax"    // DynSettings.Tax
	AttrSep    = "data-wb-sep"    // DynSettings.Sep
	AttrWords  = "data-wb-words"  // DynSettings.Words
	AttrField  = "data-wb-field"  // post.field / post.field-url: the Pursae Custom Fields field name (FieldPattern)
	AttrSearch = "data-wb-search" // on the search <form method="get" action="<home>/"> with <input type="search" name="s">
	// AttrItem is the Item.id of the cell a slot element belongs to (IDPattern). Commerce and
	// language-switcher slots read that cell's DynSettings from the STORED doc by this id; an id
	// the doc does not have gets the widget defaults.
	AttrItem = "data-wb-item"
	// SearchInputName / SearchInputType: the only field of a data-wb-search form (sanitizer
	// exception, package TA; InputTypes stays as is for every other form).
	SearchInputName = "s"
	SearchInputType = "search"
)

// SlotName is the value of data-wb-slot / data-wb-if / data-wb-if-not.
type SlotName string

const (
	SlotSiteTitle   SlotName = "site.title"
	SlotSiteTagline SlotName = "site.tagline"
	SlotSiteURL     SlotName = "site.url"
	SlotSiteLogo    SlotName = "site.logo"
	SlotSiteYear    SlotName = "site.year"

	// SlotPostField prints a Pursae Custom Fields value of the post (data-wb-field = the field
	// name) as escaped text; SlotPostFieldURL sets href/src from a link, URL or image field.
	SlotPostField    SlotName = "post.field"
	SlotPostFieldURL SlotName = "post.field-url"

	SlotMenuLabel SlotName = "menu.label"
	SlotMenuURL   SlotName = "menu.url"
	// SlotMenuLink sets only href (and target when the item has one): a code section's menu item
	// keeps the design's look, the current item is marked by its active-link rule.
	SlotMenuLink SlotName = "menu.link"

	// Code bindings (codebind.go): the post title as stored (plain text), the names of the post's
	// terms as plain text (data-wb-tax, data-wb-sep), a custom field as HTML (sanitized like post
	// content).
	SlotPostTitleText SlotName = "post.title-text"
	SlotPostTermNames SlotName = "post.term-names"
	SlotPostFieldHTML SlotName = "post.field-html"
	// SlotPostFieldAttrs writes the attributes the fields <data-wb-field>_<attr> name (href, src,
	// srcset, alt, …: SlotAttrs), so one element takes several per-post values.
	SlotPostFieldAttrs SlotName = "post.field-attrs"

	SlotPostTitle         SlotName = "post.title"
	SlotPostURL           SlotName = "post.url"
	SlotPostContent       SlotName = "post.content"
	SlotPostExcerpt       SlotName = "post.excerpt"
	SlotPostDate          SlotName = "post.date"
	SlotPostModified      SlotName = "post.modified"
	SlotPostAuthor        SlotName = "post.author"
	SlotPostAuthorURL     SlotName = "post.author-url"
	SlotPostAvatar        SlotName = "post.avatar"
	SlotPostAuthorBio     SlotName = "post.author-bio"
	SlotPostImage         SlotName = "post.image"
	SlotPostTerms         SlotName = "post.terms"
	SlotPostCommentsCount SlotName = "post.comments-count"
	SlotPostCommentsURL   SlotName = "post.comments-url"
	SlotPostNav           SlotName = "post.nav"
	SlotPostComments      SlotName = "post.comments"

	SlotArchiveTitle       SlotName = "archive.title"
	SlotArchiveDescription SlotName = "archive.description"
	SlotSearchQuery        SlotName = "search.query"
	SlotSearchCount        SlotName = "search.count"
	SlotLoopPagination     SlotName = "loop.pagination"

	SlotSiteLanguages SlotName = "site.languages" // Pursae Multi Language switcher (html)

	// Pursae Commerce (§11.12).
	SlotShopArchiveTitle SlotName = "shop.archive-title"
	SlotShopCartURL      SlotName = "shop.cart-url"
	SlotShopCheckoutURL  SlotName = "shop.checkout-url"
	SlotShopProducts     SlotName = "shop.products"
	SlotShopCategories   SlotName = "shop.categories"
	SlotShopArchive      SlotName = "shop.archive"
	SlotShopArchiveDesc  SlotName = "shop.archive-desc"
	SlotShopBreadcrumb   SlotName = "shop.breadcrumb"
	SlotShopNotices      SlotName = "shop.notices"
	SlotShopCartCount    SlotName = "shop.cart-count"
	SlotShopCartSubtotal SlotName = "shop.cart-subtotal"
	SlotShopMiniCart     SlotName = "shop.mini-cart"
	SlotShopCartPage     SlotName = "shop.cart-page"
	SlotShopCheckoutPage SlotName = "shop.checkout-page"

	SlotProductTitle           SlotName = "product.title"
	SlotProductOnSale          SlotName = "product.on-sale"
	SlotProductSKU             SlotName = "product.sku"
	SlotProductURL             SlotName = "product.url"
	SlotProductImage           SlotName = "product.image"
	SlotProductGallery         SlotName = "product.gallery"
	SlotProductPrice           SlotName = "product.price"
	SlotProductRating          SlotName = "product.rating"
	SlotProductShortDesc       SlotName = "product.short-description"
	SlotProductDescription     SlotName = "product.description"
	SlotProductAddToCart       SlotName = "product.add-to-cart"
	SlotProductAddToCartButton SlotName = "product.add-to-cart-button"
	SlotProductMeta            SlotName = "product.meta"
	SlotProductStock           SlotName = "product.stock"
	SlotProductTabs            SlotName = "product.tabs"
	SlotProductRelated         SlotName = "product.related"
	SlotProductUpsells         SlotName = "product.upsells"
)

// SlotMode says how a slot fills its element.
type SlotMode string

const (
	// SlotText replaces the element's content with the escaped value (html.EscapeString). On a
	// <time>, post.date / post.modified also set datetime (RFC 3339).
	SlotText SlotMode = "text"
	// SlotHTML replaces the content with HTML from a trusted core generator only (theTitle,
	// theContent, theExcerpt, paginateLinks, the comments template, escaped term links).
	SlotHTML SlotMode = "html"
	// SlotAttr sets attributes from SlotAttrs (URLs through the scheme check) and keeps the content.
	SlotAttr SlotMode = "attr"
)

// Slots lists every slot with its mode. Unknown slots render empty.
var Slots = map[SlotName]SlotMode{
	SlotSiteTitle: SlotText, SlotSiteTagline: SlotText, SlotSiteURL: SlotAttr, SlotSiteLogo: SlotAttr, SlotSiteYear: SlotText,
	SlotMenuLabel: SlotText, SlotMenuURL: SlotAttr, SlotMenuLink: SlotAttr,
	SlotPostField: SlotText, SlotPostFieldURL: SlotAttr, SlotPostFieldHTML: SlotHTML,
	SlotPostTitleText: SlotText, SlotPostTermNames: SlotText, SlotPostFieldAttrs: SlotAttr,
	SlotPostTitle: SlotHTML, SlotPostURL: SlotAttr, SlotPostContent: SlotHTML, SlotPostExcerpt: SlotHTML,
	SlotPostDate: SlotText, SlotPostModified: SlotText, SlotPostAuthor: SlotText, SlotPostAuthorURL: SlotAttr,
	SlotPostAvatar: SlotAttr, SlotPostAuthorBio: SlotText, SlotPostImage: SlotAttr, SlotPostTerms: SlotHTML,
	SlotPostCommentsCount: SlotText, SlotPostCommentsURL: SlotAttr, SlotPostNav: SlotHTML, SlotPostComments: SlotHTML,
	SlotArchiveTitle: SlotHTML, SlotArchiveDescription: SlotHTML, SlotSearchQuery: SlotText, SlotSearchCount: SlotText,
	SlotLoopPagination: SlotHTML,
	SlotSiteLanguages:  SlotHTML,

	SlotShopArchiveTitle: SlotText, SlotShopCartURL: SlotAttr, SlotShopCheckoutURL: SlotAttr,
	SlotShopProducts: SlotHTML, SlotShopCategories: SlotHTML, SlotShopArchive: SlotHTML, SlotShopArchiveDesc: SlotHTML,
	SlotShopBreadcrumb: SlotHTML, SlotShopNotices: SlotHTML, SlotShopCartCount: SlotHTML, SlotShopCartSubtotal: SlotHTML,
	SlotShopMiniCart: SlotHTML, SlotShopCartPage: SlotHTML, SlotShopCheckoutPage: SlotHTML,
	SlotProductTitle: SlotText, SlotProductOnSale: SlotText, SlotProductSKU: SlotText, SlotProductURL: SlotAttr, SlotProductImage: SlotAttr,
	SlotProductGallery: SlotHTML, SlotProductPrice: SlotHTML, SlotProductRating: SlotHTML, SlotProductShortDesc: SlotHTML,
	SlotProductDescription: SlotHTML, SlotProductAddToCart: SlotHTML, SlotProductAddToCartButton: SlotHTML, SlotProductMeta: SlotHTML,
	SlotProductStock: SlotHTML, SlotProductTabs: SlotHTML, SlotProductRelated: SlotHTML, SlotProductUpsells: SlotHTML,
}

// SlotAttrs are the only attributes an attr slot (or a text slot on <time>) may write.
var SlotAttrs = []string{"href", "src", "srcset", "sizes", "width", "height", "alt", "datetime", "aria-current", "class", "target"}

// ClassCurrent is added to the element of the current menu item (with aria-current="page").
const ClassCurrent = "is-current"

// ---------------------------------------------------------------------------------------------
// Sample data for the editor: GET RoutePreviewContext and Boot.Dynamic.

type DynImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Alt    string `json:"alt"`
}

type DynAuthor struct {
	Name   string `json:"name"`
	URL    string `json:"url"`    // author archive
	Avatar string `json:"avatar"` // Media Library avatar URL, "" without one (no Gravatar)
	Bio    string `json:"bio"`
}

type DynTerm struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// DynPost is one post as dynamic widgets see it.
type DynPost struct {
	ID            int64                `json:"id"`
	Type          string               `json:"type"`
	Title         string               `json:"title"` // plain text
	URL           string               `json:"url"`
	Excerpt       string               `json:"excerpt"`     // plain text
	ContentHTML   string               `json:"contentHtml"` // UGC-sanitized; shown read-only in the editor
	Date          string               `json:"date"`        // RFC 3339
	DateText      string               `json:"dateText"`    // formatted with the site's date_format
	Modified      string               `json:"modified"`    // RFC 3339
	ModifiedText  string               `json:"modifiedText"`
	Author        DynAuthor            `json:"author"`
	Image         *DynImage            `json:"image"` // featured image, null without one
	Terms         map[string][]DynTerm `json:"terms"` // taxonomy → terms, in term order
	CommentsCount int                  `json:"commentsCount"`
	CommentsURL   string               `json:"commentsUrl"`
}

// DynMenuItem is one menu item; Children is always empty in v1 (top level only).
type DynMenuItem struct {
	ID       int64         `json:"id"`
	Label    string        `json:"label"`
	URL      string        `json:"url"`
	Current  bool          `json:"current"`
	Children []DynMenuItem `json:"children"`
}

type DynSite struct {
	Title   string    `json:"title"`
	Tagline string    `json:"tagline"`
	Home    string    `json:"home"` // home URL, no trailing slash
	Logo    *DynImage `json:"logo"` // custom_logo, null without one
	Year    string    `json:"year"` // current year in the site time zone
}

type MenuLocation struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// DynView kinds.
const (
	DynViewSingle    = "single"
	DynViewPage      = "page"
	DynViewFrontPage = "front-page"
	DynViewHome      = "home"
	DynViewArchive   = "archive"
	DynViewSearch    = "search"
	DynView404       = "404"
)

type DynView struct {
	Kind         string `json:"kind"`         // DynView* constants
	ArchiveTitle string `json:"archiveTitle"` // plain text
	ArchiveDesc  string `json:"archiveDesc"`  // plain text
	Search       string `json:"search"`       // the search query
	Total        int    `json:"total"`        // posts found by the main query
}

type DynPaging struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

// DynContext is the sample data of one preview object.
type DynContext struct {
	Object     string                   `json:"object"` // the preview object it was built for
	Site       DynSite                  `json:"site"`
	Locations  []MenuLocation           `json:"locations"` // the active theme's menu locations
	Menus      map[string][]DynMenuItem `json:"menus"`     // location → items (golylang-swapped)
	View       DynView                  `json:"view"`
	Post       *DynPost                 `json:"post"`  // the singular post, null elsewhere
	Posts      []DynPost                `json:"posts"` // the main query's posts (archives), ≤ MaxLoopPerPage
	Pagination DynPaging                `json:"pagination"`
	Shop       *DynShop                 `json:"shop"`      // Pursae Commerce sample data, null while it is off
	Languages  []DynLanguage            `json:"languages"` // Pursae Multi Language languages, [] while it is off
}

// DynLanguage is one language of the language switcher's sample.
type DynLanguage struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Flag    string `json:"flag"` // emoji, "" without one
	URL     string `json:"url"`
	Current bool   `json:"current"`
}

// DynProduct is one Pursae Commerce product as the editor draws it.
type DynProduct struct {
	ID            int64      `json:"id"`
	Type          string     `json:"type"` // simple, variable, grouped, external
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	SKU           string     `json:"sku"`
	PriceHTML     string     `json:"priceHtml"` // the store's price markup (trusted)
	OnSale        bool       `json:"onSale"`
	InStock       bool       `json:"inStock"`
	StockHTML     string     `json:"stockHtml"`
	Rating        float64    `json:"rating"`
	RatingCount   int        `json:"ratingCount"`
	ShortHTML     string     `json:"shortHtml"`   // UGC-sanitized
	ContentHTML   string     `json:"contentHtml"` // UGC-sanitized
	Image         *DynImage  `json:"image"`
	Gallery       []DynImage `json:"gallery"`
	Categories    []int64    `json:"categories"` // product_cat ids
	AddToCartText string     `json:"addToCartText"`
}

// DynProductCat is one product category.
type DynProductCat struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	URL    string    `json:"url"`
	Count  int       `json:"count"`
	Parent int64     `json:"parent"`
	Image  *DynImage `json:"image"`
}

type DynCart struct {
	Count        int    `json:"count"`
	SubtotalHTML string `json:"subtotalHtml"`
}

// DynShop is the Pursae Commerce part of DynContext.
type DynShop struct {
	Product     *DynProduct     `json:"product"`    // the preview product (single-product template, product doc), null elsewhere
	Products    []DynProduct    `json:"products"`   // newest catalog products, ≤ MaxShopLimit
	Categories  []DynProductCat `json:"categories"` // product_cat, ≤ 100
	Cart        DynCart         `json:"cart"`
	ShopURL     string          `json:"shopUrl"`
	CartURL     string          `json:"cartUrl"`
	CheckoutURL string          `json:"checkoutUrl"`
}

// ---------------------------------------------------------------------------------------------
// Boot additions (APIVersion 2).

type TemplatePreview struct {
	Object string `json:"object"` // preview object
	Label  string `json:"label"`  // "Post: Hello world", "Category: News", …
	URL    string `json:"url"`    // front URL of the object, with ?wb_template_preview=<id>
}

// TemplateBoot is Boot.Template on a template's editor page.
type TemplateBoot struct {
	Type       TemplateType    `json:"type"`
	Conditions []string        `json:"conditions"`
	Priority   int             `json:"priority"`
	Starter    StarterID       `json:"starter"` // loaded by the editor when the doc is empty
	Preview    TemplatePreview `json:"preview"`
	ManageURL  string          `json:"manageUrl"` // Web Builder › Theme Builder
}

type PostTypeInfo struct {
	Slug         string `json:"slug"`
	Label        string `json:"label"`        // plural name
	Archive      string `json:"archive"`      // archive URL, "" without one
	Hierarchical bool   `json:"hierarchical"` // child_of applies
}

type TaxonomyInfo struct {
	Slug  string   `json:"slug"`
	Label string   `json:"label"`
	Types []string `json:"types"` // post types it is attached to
}

// TemplateFile is one template as a file (GET RouteTemplate…/export, POST templates/import): the
// stored doc with the template's type, title, display conditions, priority and, for a popup, its
// settings. No snapshot: the importing site renders it in the editor.
type TemplateFile struct {
	Format     string          `json:"format"`  // TemplateFileFormat
	Version    int             `json:"version"` // TemplateFileVersion
	Type       TemplateType    `json:"type"`
	Title      string          `json:"title"`
	Conditions []string        `json:"conditions"`
	Priority   int             `json:"priority"`
	Popup      *PopupSettings  `json:"popup,omitempty"`
	Doc        json.RawMessage `json:"doc"`
}

const (
	TemplateFileFormat  = "web-builder-template"
	TemplateFileVersion = 1
)

// RoleInfo is one role of the site, for the audience rules (user/role/<slug>).
type RoleInfo struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

// SiteBoot is Boot.Site.
type SiteBoot struct {
	Title      string                   `json:"title"`
	Tagline    string                   `json:"tagline"`
	Home       string                   `json:"home"`
	Logo       *DynImage                `json:"logo"`
	Locations  []MenuLocation           `json:"locations"`
	Menus      map[string][]DynMenuItem `json:"menus"`
	ThemeParts bool                     `json:"themeParts"` // Hello Pursae is active: pages may use layout "site"
	PostTypes  []PostTypeInfo           `json:"postTypes"`  // viewable types: post, page, public custom types, product (Pursae Commerce)
	Taxonomies []TaxonomyInfo           `json:"taxonomies"` // category, post_tag; product_cat, product_tag with Pursae Commerce
	Roles      []RoleInfo               `json:"roles"`      // the site's roles whose slug fits a rule (SlugPattern), most capable first
}

// ---------------------------------------------------------------------------------------------
// Template REST payloads (TemplateCap on every route except RoutePreviewContext).

// TemplateSummary is one row of GET RouteTemplates (?type= filters; trash excluded).
type TemplateSummary struct {
	ID         int64        `json:"id"`
	Title      string       `json:"title"`
	Type       TemplateType `json:"type"`
	Status     string       `json:"status"` // publish = active, draft = disabled
	Conditions []string     `json:"conditions"`
	Priority   int          `json:"priority"`
	Author     UserRef      `json:"author"`
	Modified   string       `json:"modified"` // RFC 3339
	EditURL    string       `json:"editUrl"`
	PreviewURL string       `json:"previewUrl"`
}

// NavigatorTemplate is one template of the editor's page switcher (GET RouteNavigator).
type NavigatorTemplate struct {
	ID        int64        `json:"id"`
	Title     string       `json:"title"`
	Type      TemplateType `json:"type"`
	TypeLabel string       `json:"typeLabel"`
	Status    string       `json:"status"`
	EditURL   string       `json:"editUrl"`
	Disabled  bool         `json:"disabled"` // a draft template is not shown on the site
}

// NavigatorPost is one page or post of the editor's page switcher.
type NavigatorPost struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	PostType  string `json:"postType"`
	TypeLabel string `json:"typeLabel"`
	Status    string `json:"status"`
	EditURL   string `json:"editUrl"`
}

// NavigatorResponse is GET RouteNavigator: every template (edit_theme_options only, at most
// NavigatorMaxTemplates) and, per enabled post type, the newest-modified posts the user may edit
// (at most NavigatorMaxPosts per type). Trash is excluded.
type NavigatorResponse struct {
	Templates []NavigatorTemplate `json:"templates"`
	Posts     []NavigatorPost     `json:"posts"`
}

// Limits of the page switcher list.
const (
	NavigatorMaxTemplates = 200
	NavigatorMaxPosts     = 100
)

// TemplateCreateRequest is the body of POST RouteTemplates. PostType is required for
// TemplateSingle (a public custom post type, never post or page) and refused otherwise.
type TemplateCreateRequest struct {
	Type     TemplateType `json:"type"`
	Title    string       `json:"title"`             // "" = the type label
	Starter  StarterID    `json:"starter,omitempty"` // "" = DefaultStarters[type]
	PostType string       `json:"postType,omitempty"`
}

// TemplateCreateResponse answers 201: a draft with the type's default conditions.
type TemplateCreateResponse struct {
	ID      int64  `json:"id"`
	EditURL string `json:"editUrl"`
}

// TemplateConditionsRequest is the body of PUT RouteTemplateConditions; the response is the
// same shape, normalized (trimmed, duplicates removed, order kept).
type TemplateConditionsRequest struct {
	Conditions []string `json:"conditions"`
	Priority   int      `json:"priority"`
}

type TemplateConditionsResponse = TemplateConditionsRequest

// TemplatePreviewRequest is the body of PUT RouteTemplatePreview.
type TemplatePreviewRequest struct {
	Object string `json:"object"`
}

// TemplatePreviewResponse: the stored preview and its sample data.
type TemplatePreviewResponse struct {
	Preview TemplatePreview `json:"preview"`
	Dynamic DynContext      `json:"dynamic"`
}

// TemplateConflict is one item of GET RouteTemplateConflicts: a published template of the same
// location whose include rules can match a view this template's include rules match. A warning
// only; the scores decide at request time.
type TemplateConflict struct {
	ID         int64        `json:"id"`
	Title      string       `json:"title"`
	Type       TemplateType `json:"type"`
	Conditions []string     `json:"conditions"`
	Priority   int          `json:"priority"`
	EditURL    string       `json:"editUrl"`
}
