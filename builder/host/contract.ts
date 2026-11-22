/**
 * Pursae Web Builder — integration contract, TypeScript half.
 *
 * The prose and field tables live in docs/developer/web-builder-contract.md; the Go half is
 * internal/webbuilder/contract.go. A change to one is a change to all three, in one commit.
 *
 * The document model (DocState, Section, PageSettings, SavedComponent, …) is imported from the
 * pinned upstream (`@` = builder/upstream/src), so the editor and the contract cannot drift; the
 * Go mirror was copied from upstream src/builder/types.ts at UPSTREAM_TYPES_SHA.
 *
 * Erasable syntax only (no enums / namespaces), so the file also runs under type stripping.
 */
import type { DocState, PageSettings, SavedComponent, Section } from '@/builder/types'

export type { DocState, PageSettings, SavedComponent, Section }

/* ---------- Versions and handshake ---------- */

/** REST/boot schema version; sent as X-WB-Api on every web-builder/v2 request. 2 = Theme Builder. */
export const API_VERSION = 2
/**
 * Static markup + data-wb-* runtime version; stamped into RenderResult.renderer. Bumped only for an
 * incompatible markup change (a stored snapshot would render wrong or be misread); additive
 * attributes, cell types and section kinds keep it.
 */
export const RENDERER_VERSION = 1
export const API_HEADER = 'X-WB-Api'
export const NONCE_HEADER = 'X-WP-Nonce'
export const BOOT_GLOBAL = '__WEB_BUILDER__'
export const UPSTREAM_TYPES_SHA = '22ff4976a1416accd3b7370e176d7206bf390aee'

/* ---------- REST ---------- */

export const REST_NAMESPACE = 'web-builder/v2'

/** Route builders, relative to Boot.rest.root (which ends with "/wp-json/"). */
export const routes = {
  document: (id: number) => `web-builder/v2/documents/${id}`,
  autosave: (id: number) => `web-builder/v2/documents/${id}/autosave`,
  publish: (id: number) => `web-builder/v2/documents/${id}/publish`,
  lock: (id: number) => `web-builder/v2/documents/${id}/lock`,
  revisions: (id: number) => `web-builder/v2/documents/${id}/revisions`,
  revision: (id: number, rid: number) => `web-builder/v2/documents/${id}/revisions/${rid}`,
  revisionRestore: (id: number, rid: number) => `web-builder/v2/documents/${id}/revisions/${rid}/restore`,
  library: () => 'web-builder/v2/library',
  libraryItem: (id: string) => `web-builder/v2/library/${encodeURIComponent(id)}`,
  prefs: () => 'web-builder/v2/prefs',
  fontsCss: () => 'web-builder/v2/fonts/css2',
  form: (postId: number, sectionId: string) => `web-builder/v2/forms/${postId}/${encodeURIComponent(sectionId)}`,
  aiStatus: () => 'web-builder/v2/ai/status',
  aiLlm: (provider: AiProviderId, path: string) => `web-builder/v2/ai/llm/${provider}/${path}`,
  aiImages: () => 'web-builder/v2/ai/images',
  templates: () => 'web-builder/v2/templates',
  templateConditions: (id: number) => `web-builder/v2/templates/${id}/conditions`,
  templatePreview: (id: number) => `web-builder/v2/templates/${id}/preview`,
  /** Popup templates: when a popup opens and how often (GET/PUT, webbuilder PopupSettings). */
  templatePopup: (id: number) => `web-builder/v2/templates/${id}/popup`,
  templateConflicts: (id: number) => `web-builder/v2/templates/${id}/conflicts`,
  templateExport: (id: number) => `web-builder/v2/templates/${id}/export`,
  templateImport: () => 'web-builder/v2/templates/import',
  navigator: () => 'web-builder/v2/navigator',
  siteSettings: () => 'web-builder/v2/site-settings',
  /** Page tools (page-tools.ts): which plugin tabs the page has, Pursae Multi Language, Pursae SEO. */
  documentPlugins: (id: number) => `web-builder/v2/documents/${id}/plugins`,
  documentLanguages: (id: number) => `web-builder/v2/documents/${id}/languages`,
  documentTranslations: (id: number) => `web-builder/v2/documents/${id}/translations`,
  /** Pursae SEO's own route (plugin namespace seogo/v1, not web-builder/v2). */
  seoMeta: (id: number) => `seogo/v1/meta/${id}`,
  previewContext: () => 'web-builder/v2/preview-context',
  media: () => 'wp/v2/media',
} as const

/* ---------- Asset prefixes ---------- */

export const EDITOR_ASSETS_PATH = '/wp-content/plugins/web-builder/dist/editor/'
export const EDITOR_NOAI_ASSETS_PATH = '/wp-content/plugins/web-builder/dist/editor-noai/'
export const SITE_ASSETS_PATH = '/wp-content/web-builder/site/'
export const FONTS_UPLOAD_PREFIX = '/wp-content/uploads/web-builder/fonts/'

/* ---------- Limits ---------- */

export const LIMITS = {
  maxDocBytes: 4 << 20,
  maxSnapshotHtml: 2 << 20,
  maxCssBytes: 1 << 20,
  inlineCssMax: 24 << 10,
  maxSections: 200,
  maxItems: 500,
  maxDepth: 4,
  maxStringBytes: 64 << 10,
  maxHtmlSectionLen: 256 << 10,
  maxDataUriBytes: 32 << 10,
  maxLibraryItems: 500,
  maxPrefsBytes: 16 << 10,
  maxRevisionsListed: 50,
  maxFormBodyBytes: 64 << 10,
  maxAiBodyBytes: 8 << 20,
  lockTtlSeconds: 150,
  autosaveMinMillis: 2000,
  lockRefreshSeconds: 60,
  aiMaxTokens: 8192,
  aiImageMaxCount: 12,
  aiImageDefaultCount: 6,
  formMinMillis: 2000,
  formNameMax: 200,
  formEmailMax: 254,
  formPhoneMax: 50,
  formMessageMax: 5000,
  formOtherMax: 200,
} as const

export const ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/
export const PREF_KEY_PATTERN = /^[a-z0-9:._-]{1,64}$/

/* ---------- Errors (WordPress REST error shape) ---------- */

export const ERR = {
  apiMismatch: 'wb_api_mismatch', // 409, data.reload = true
  revConflict: 'wb_rev_conflict', // 409
  detached: 'wb_detached', // 409
  locked: 'wb_locked', // 409
  tooLarge: 'wb_too_large', // 413
  invalidDoc: 'wb_invalid_doc', // 422
  notFound: 'wb_not_found', // 404
  notEnabled: 'wb_not_enabled', // 404
  formInvalid: 'wb_form_invalid', // 422
  rateLimited: 'wb_rate_limited', // 429
  aiBudget: 'wb_ai_budget', // 429
  aiPath: 'wb_ai_path', // 400
  aiModel: 'wb_ai_model', // 422
  aiNoKey: 'wb_ai_no_key', // 503
  aiUpstream: 'wb_ai_upstream', // 502
  aiTimeout: 'wb_ai_timeout', // 504
  badCondition: 'wb_bad_condition', // 422, data.errors (path /conditions/<i>, code bad_condition)
  badTemplate: 'wb_bad_template', // 422
  badPreview: 'wb_bad_preview', // 422
  forbidden: 'rest_forbidden', // 401 / 403
  invalidNonce: 'rest_cookie_invalid_nonce', // 403
} as const
export type ErrCode = (typeof ERR)[keyof typeof ERR]

export interface UserRef {
  id: number
  name: string
}

export interface LockInfo {
  user: UserRef
  /** RFC 3339 */
  since: string
}

export interface ValidationError {
  /** JSON pointer into the request body, e.g. /doc/sections/3/items/0/href */
  path: string
  code: 'required' | 'unknown_kind' | 'bad_id' | 'too_long' | 'too_many' | 'too_deep' | 'bad_url' | 'bad_css' | 'bad_value' | 'bad_renderer' | 'bad_condition'
  message: string
}

export interface RestErrorData {
  status: number
  /** wb_api_mismatch: reload the editor. */
  reload?: boolean
  apiVersion?: number
  /** wb_rev_conflict */
  rev?: number
  modified?: string
  modifiedBy?: UserRef
  /** wb_locked */
  lock?: LockInfo
  /** wb_too_large */
  limit?: number
  /** wb_invalid_doc */
  errors?: ValidationError[]
  /** wb_form_invalid: field name → reason (required | invalid | too_long) */
  fields?: Record<string, string>
}

export interface RestError {
  code: ErrCode | string
  message: string
  data: RestErrorData
}

/* ---------- renderStatic ---------- */

export type Feature = 'menu' | 'acc' | 'tabs' | 'dismiss' | 'form' | 'motion' | 'embed'

export interface FontNeed {
  family: string
  weights: number[]
  italic: boolean
}

/** What upstream live/renderStatic(doc, opts) returns. */
export interface RenderResult {
  /** One root <div class="wb-page …" data-wb-root>…</div>. */
  html: string
  /** Section custom CSS (style.css), scoped to each section; privileged. */
  css: string
  /** Tailwind candidate classes used by html. */
  classes: string[]
  fonts: FontNeed[]
  /** Sorted, unique; empty = the page needs no runtime. */
  features: Feature[]
  renderer: number
}

/** Host-supplied options of renderStatic (the renderer never reads window globals). */
export interface RenderOptions {
  postId: number
  /** Absolute action URL of a form section (Boot.rest.root + routes.form(postId, sectionId)). */
  formAction: (sectionId: string) => string
  /** Home URL, no trailing slash; relative links in the doc stay relative. */
  siteUrl: string
}

/* ---------- Documents ---------- */

/** future = scheduled: PublishRequest.date says when the page goes live. */
export type PublishStatus = 'publish' | 'draft' | 'pending' | 'private' | 'future'

export interface DraftState {
  doc: DocState
  page: PageSettings
  baseRev: number
  savedAt: string
}

/** GET documents/{id}; also Boot.document. */
export interface DocumentResponse {
  postId: number
  postType: string
  status: string
  /** RFC 3339 publish date ("" = none yet); in the future while status is future. */
  date?: string
  /** 0 = never published from the builder. */
  rev: number
  /** null = new page: the editor starts from its default doc. */
  doc: DocState | null
  page: PageSettings
  /** The current user's autosave, only when newer than doc. */
  draft: DraftState | null
  detached: boolean
  /** posts.modified_at */
  modified: string
  /** When the stored doc was last written ("" without one); a draft is newer than this. */
  docModified?: string
  renderer: number
  /** Another user's live lock, else null. */
  lock: LockInfo | null
}

export interface AutosaveRequest {
  doc: DocState
  page: PageSettings
  baseRev: number
}

export interface AutosaveResponse {
  savedAt: string
  /** Current published rev; > baseRev means someone else published meanwhile. */
  rev: number
}

export interface PublishRequest {
  baseRev: number
  status: PublishStatus
  /** RFC 3339; required with status future (a past date publishes now). */
  date?: string
  /** Required on a detached page: drops the outside edits. */
  reattach?: boolean
  doc: DocState
  page: PageSettings
  render: RenderResult
  /** Tailwind compile() output for render.classes. */
  utilityCss: string
}

export type Stripped = 'html-sections' | 'custom-css' | 'markup' | 'css'

export interface PublishResponse {
  rev: number
  /** pending without publish rights; future for a scheduled page. */
  status: string
  /** RFC 3339 publish date ("" = none). */
  date?: string
  permalink: string
  revisionId: number
  modified: string
  stripped: Stripped[]
}

export interface LockRequest {
  take?: boolean
}

export interface LockResponse {
  lock: LockInfo | null
  expires: string
}

export interface RevisionSummary {
  id: number
  rev: number
  author: UserRef
  date: string
  title: string
  renderer: number
}

export interface RevisionResponse {
  id: number
  rev: number
  doc: DocState
  page: PageSettings
}

/** Restore writes the revision as the caller's draft and returns it as a DraftState. */
export interface RestoreRequest {
  baseRev: number
}

export interface LibraryItem extends SavedComponent {
  author: UserRef
  created: string
}

export interface LibraryCreateRequest {
  name: string
  section: Section
}

export type Prefs = Record<string, unknown>

export interface FormResponse {
  ok: boolean
  message: string
  /** Absolute URL of this site to open after a sent form (optional). */
  redirect?: string
}

/* ---------- Boot: window.__WEB_BUILDER__ ---------- */

export type AiProviderId = 'anthropic' | 'openai' | 'google' | 'mistral' | 'ollama'

export interface AiModel {
  id: string
  name: string
}

export interface AiProviderInfo {
  id: AiProviderId
  models: AiModel[]
}

export interface BootAi {
  enabled: boolean
  /** Configured providers only; empty = the demo engine only, nothing leaves the browser. */
  providers: AiProviderInfo[]
  /** Absolute ".../wp-json/web-builder/v2/ai/llm/"; "" when off. */
  llmBase: string
  /** Absolute ai/images URL; "" when off. */
  imagesUrl: string
  /** Always false for now: AI tools may not write html sections or custom CSS. */
  allowCode: boolean
}

export interface Boot {
  apiVersion: number
  rendererVersion: number
  document: DocumentResponse
  user: UserRef
  rest: { root: string; namespace: string; nonce: string }
  links: { site: string; permalink: string; preview: string; exit: string; editPost: string; settings: string }
  caps: { publish: boolean; uploadFiles: boolean; unfilteredHtml: boolean; editOthers: boolean; editThemeOptions?: boolean }
  /** WordPress locale, e.g. en_US, tr_TR. */
  locale: string
  fonts: { cssUrl: string; local: string[] }
  media: { maxUploadBytes: number; types: string[] }
  /** Absolute SITE_ASSETS_PATH. */
  siteAssets: string
  ai: BootAi
  /** Theme Builder (API 2): null unless the post is a wb_template. */
  template: TemplateBoot | null
  site: SiteBoot
  /** Sample data for dynamic widgets: the template's preview object, else the post itself. */
  dynamic: DynContext
  /**
   * The header and footer templates the site draws around this page when they are made of code
   * sections (the element editor shows them, locked, in the page's frame); null for a template, a
   * canvas page, or when the theme draws no parts.
   */
  codeParts: CodeParts | null
  /**
   * Local-style breakpoints (max-width px) of the design kits the page's code sections and its
   * code parts use, by kit slug; a section without a kit uses DEFAULT_BREAKPOINTS.
   */
  kitBreakpoints?: Record<string, Breakpoints>
  /**
   * The site style (doc.site values without `global`) that designs with `site.global` follow; null
   * while none was saved. Saved through site-settings (`site_style`, edit_theme_options).
   */
  siteStyle?: SiteStyle | null
}

/** Boot.siteStyle: the doc.site keys a site style sets. */
export interface SiteStyle {
  brand: string
  fontPair: string
  corners: string
  headingFont?: string
  headingWeight?: number
  bodyFont?: string
  bodyWeight?: number
}

/** Boot.kitBreakpoints values: tablet and mobile max-width thresholds in px. */
export interface Breakpoints {
  tablet: number
  mobile: number
}

/** Boot.codeParts. */
export interface CodeParts {
  /** null: no header template, or one not made of code sections. */
  header: CodeTemplate | null
  footer: CodeTemplate | null
}

/** One template of CodeParts: its published code sections, as stored. */
export interface CodeTemplate {
  id: number
  title: string
  /** Absolute; opens the template in the editor. */
  editUrl: string
  sections: Section[]
}

declare global {
  interface Window {
    __WEB_BUILDER__?: Boot
  }
}

/* ---------- Media (existing core route wp/v2/media) ---------- */

/** The subset of a wp/v2/media item the host reads. Upload: multipart field "file". */
export interface MediaItem {
  id: number
  source_url: string
  alt_text: string
  mime_type: string
  media_details: { width?: number; height?: number; sizes?: Record<string, { source_url: string; width: number; height: number }> }
}

/* ---------- AI proxy ---------- */

export const AI_PROVIDER_PATHS: Record<AiProviderId, readonly string[]> = {
  anthropic: ['v1/messages'],
  openai: ['v1/chat/completions'],
  google: ['v1beta/openai/chat/completions'],
  mistral: ['v1/chat/completions'],
  ollama: ['v1/chat/completions'],
}

export interface AiStatusResponse {
  enabled: boolean
  providers: AiProviderInfo[]
  budget: { daily: number; used: number }
}

/** Same shapes as upstream ai/images.ts ImageQuery / ImageResult, plus mediaId. */
export interface AiImageRequest {
  query: string
  count?: number
  orientation?: 'landscape' | 'portrait' | 'square'
}

export interface AiImage {
  url: string
  mediaId: number
  width?: number
  height?: number
  alt: string
  credit: string
  license: string
  source: string
}

export interface AiImageResponse {
  images: AiImage[]
  error?: string
}

/* ---------- data-wb-* runtime ---------- */

export const ATTR = {
  root: 'data-wb-root',
  menu: 'data-wb-menu',
  acc: 'data-wb-acc',
  accSingle: 'data-wb-acc-single',
  tabs: 'data-wb-tabs',
  tab: 'data-wb-tab',
  panel: 'data-wb-panel',
  autoplay: 'data-wb-autoplay',
  dismiss: 'data-wb-dismiss',
  dismissBtn: 'data-wb-dismiss-btn',
  form: 'data-wb-form',
  formStatus: 'data-wb-form-status',
  state: 'data-wb-state',
  motion: 'data-wb-motion',
  dur: 'data-wb-dur',
  delay: 'data-wb-delay',
  ease: 'data-wb-ease',
  repeat: 'data-wb-repeat',
  stagger: 'data-wb-stagger',
  staggerItem: 'data-wb-stagger-item',
  parallax: 'data-wb-parallax',
  parallaxOn: 'data-wb-parallax-on',
  scrollFade: 'data-wb-scroll-fade',
  scrollScale: 'data-wb-scroll-scale',
  hover: 'data-wb-hover',
  hoverOn: 'data-wb-hover-on',
  sticky: 'data-wb-sticky',
  embed: 'data-wb-embed',
} as const

export const CLASS_JS = 'wb-js'
export const CLASS_IN = 'wb-in'
export const FORM_HONEYPOT = 'wb_hp'
export const FORM_TIMING = 'wb_t'
export const FORM_SENT_QUERY = 'wb_form'
export const FORM_FIELDS = ['name', 'email', 'phone', 'message', 'date', 'choice', 'company', 'subject'] as const
export type FormField = (typeof FORM_FIELDS)[number]
/** A design's form bound to the endpoint may also use field_<name> (code bindings). */
export const FORM_FIELD_PATTERN = /^field_[a-z0-9_]{1,40}$/

/** postMessage from a data-wb-embed iframe reporting its height. */
export interface EmbedHeightMessage {
  __wb: string
  h: number
}

/* ---------- Build output ---------- */

export interface ManifestEntry {
  js: string
  css: string[]
}

export interface Manifest {
  editor: ManifestEntry
  'editor-noai': ManifestEntry
  site: { runtime: string; css: string }
}

export interface BuildInfo {
  upstreamSha: string
  patches: Record<string, string>
  apiVersion: number
  rendererVersion: number
}


/* ---------- Theme Builder (contract doc §11) ---------- */

export const TEMPLATE_POST_TYPE = 'wb_template'
export const TEMPLATE_CAP = 'edit_theme_options'
export const HELLO_PURSAE_THEME = 'hello-pursae'
export const PREVIEW_QUERY = 'wb_template_preview'
export const THEME_BUILDER_PAGE = 'web-builder-templates'
export const MENU_LOCATION_PRIMARY = 'primary'
export const MENU_LOCATION_FOOTER = 'footer'

export const TEMPLATE_TYPES = ['header', 'footer', 'single-post', 'single-page', 'single', 'single-product', 'front-page', 'archive', 'product-archive', 'search-results', 'error-404', 'popup'] as const
export type TemplateType = (typeof TEMPLATE_TYPES)[number]

export const TEMPLATE_TYPE_LABELS: Record<TemplateType, string> = {
  header: 'Header',
  footer: 'Footer',
  'single-post': 'Single Post',
  'single-page': 'Single Page',
  single: 'Single',
  'front-page': 'Front Page',
  archive: 'Archive',
  'search-results': 'Search Results',
  'error-404': '404 Page',
  popup: 'Popup',
  'single-product': 'Single Product',
  'product-archive': 'Product Archive',
}

/** Main template types of Pursae Commerce store views, with the store view kinds each renders on
 * (generic main types never render on a store view; the cart, checkout and account get none). */
export const STORE_TEMPLATE_KINDS: Partial<Record<TemplateType, readonly string[]>> = {
  'single-product': ['product'],
  'product-archive': ['shop', 'product_cat', 'product_tag', 'product_attr'],
}
export const isStoreTemplate = (t: TemplateType) => t in STORE_TEMPLATE_KINDS

export type TemplateLocation = 'header' | 'main' | 'footer' | 'popup'
export const templateLocation = (t: TemplateType): TemplateLocation => (t === 'header' ? 'header' : t === 'footer' ? 'footer' : t === 'popup' ? 'popup' : 'main')

/** Default rules written at create; 'single' gets include/singular/<postType> from the request. */
export const DEFAULT_CONDITIONS: Partial<Record<TemplateType, readonly string[]>> = {
  header: ['include/general'],
  footer: ['include/general'],
  'single-post': ['include/singular/post'],
  'single-page': ['include/singular/page'],
  'front-page': ['include/singular/front_page'],
  archive: ['include/archive'],
  'search-results': ['include/archive/search'],
  'error-404': ['include/404'],
  popup: ['include/general'],
  'single-product': ['include/singular/product'],
  'product-archive': ['include/archive/type/product', 'include/archive/product_cat', 'include/archive/product_tag'],
}

export const STARTER_IDS = ['blank', 'header', 'footer', 'single', 'archive', 'search', 'product', 'product-archive'] as const
export type StarterId = (typeof STARTER_IDS)[number]

export const DEFAULT_STARTERS: Record<TemplateType, StarterId> = {
  header: 'header',
  footer: 'footer',
  'single-post': 'single',
  'single-page': 'single',
  single: 'single',
  'front-page': 'blank',
  archive: 'archive',
  'search-results': 'search',
  'error-404': 'blank',
  popup: 'blank',
  'single-product': 'product',
  'product-archive': 'product-archive',
}

/** Display conditions: "<include|exclude>/<scope>", grammar in §11.2. */
export const CONDITION_LIMITS = { maxConditions: 50, maxConditionBytes: 200, priorityMin: -100, priorityMax: 100 } as const
export const CONDITION_PATTERN =
  /^(include|exclude)\/(general|404|singular(\/front_page|\/[a-z0-9_-]{1,32}(\/[1-9][0-9]{0,18}|\/in\/[a-z0-9_-]{1,32}\/[1-9][0-9]{0,18}|\/child_of\/[1-9][0-9]{0,18}|\/by_author\/[1-9][0-9]{0,18})?)?|archive(\/home|\/date|\/search|\/author(\/[1-9][0-9]{0,18})?|\/type\/[a-z0-9_-]{1,32}|\/[a-z0-9_-]{1,32}(\/[1-9][0-9]{0,18})?)?|user\/(logged_in|logged_out|role\/[a-z0-9_-]{1,32}))$/
export const RESERVED_SINGULAR_WORDS = ['front_page', 'in', 'child_of', 'by_author'] as const
export const RESERVED_ARCHIVE_WORDS = ['home', 'date', 'search', 'author', 'type'] as const

/** Preview object: post:<id> | term:<tax>:<id> | author:<id> | type:<ptype> | search:<q> | front | home | 404 | latest. */
export type PreviewObject = string
export const DEFAULT_PREVIEWS: Record<TemplateType, PreviewObject> = {
  header: 'front',
  footer: 'front',
  'single-post': 'latest',
  'single-page': 'latest',
  single: 'latest',
  'front-page': 'front',
  archive: 'home',
  'search-results': 'search:a',
  'error-404': '404',
  popup: 'front',
  'single-product': 'latest',
  'product-archive': 'type:product',
}

/** PageSettings.layout (upstream PageSettings gains it in patch 0008). '' / absent = defaultPageLayout. */
export type PageLayout = 'site' | 'canvas'

/* Dynamic widgets: layout cells whose Item.title is a DynWidget. Patch 0008 adds these shapes to
 * upstream types.ts (CellType, Item.dyn, SectionKind 'loop', PageSettings.layout); they must stay
 * identical to the declarations here. */

export const DYN_WIDGETS = [
  'site-logo', 'site-title', 'site-tagline', 'nav-menu', 'search-form', 'copyright',
  'post-title', 'post-content', 'post-excerpt', 'post-image', 'post-meta', 'post-terms', 'post-date', 'post-author', 'author-box', 'post-nav', 'post-comments',
  'archive-title', 'archive-desc', 'search-query', 'pagination',
  'language-switcher',
  'shop-products', 'shop-categories', 'shop-add-to-cart', 'shop-cart', 'shop-mini-cart', 'shop-notices', 'shop-cart-page', 'shop-checkout-page',
  'product-title', 'product-images', 'product-price', 'product-add-to-cart', 'product-rating', 'product-stock', 'product-meta', 'product-short-desc', 'product-content', 'product-tabs', 'product-related', 'product-upsells', 'product-sale-badge',
  'shop-archive-products', 'shop-archive-title', 'shop-archive-desc', 'shop-breadcrumb',
] as const
export type DynWidget = (typeof DYN_WIDGETS)[number]

/** Pursae Commerce widgets (contract §11.12); their settings are read from the stored doc via data-wb-item. */
export const SHOP_WIDGETS = [
  'shop-products', 'shop-categories', 'shop-add-to-cart', 'shop-cart', 'shop-mini-cart', 'shop-notices', 'shop-cart-page', 'shop-checkout-page',
  'product-title', 'product-images', 'product-price', 'product-add-to-cart', 'product-rating', 'product-stock', 'product-meta', 'product-short-desc', 'product-content', 'product-tabs', 'product-related', 'product-upsells', 'product-sale-badge',
  'shop-archive-products', 'shop-archive-title', 'shop-archive-desc', 'shop-breadcrumb',
] as const satisfies readonly DynWidget[]
export type CellType = '' | 'heading' | 'text' | 'button' | 'image' | 'layout' | DynWidget

export interface DynSettings {
  tag?: 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6' | 'p' | 'div' | 'span'
  link?: boolean
  /** WordPress date format, /^[A-Za-z ,.:/-]{1,32}$/; '' = the site's date_format. */
  format?: string
  size?: 'thumbnail' | 'medium' | 'large' | 'full'
  /** width / height, 0.2…5 */
  ratio?: number
  fields?: ('date' | 'modified' | 'author' | 'comments' | 'terms')[]
  /** ≤ 8 characters */
  sep?: string
  tax?: string
  location?: string
  menuLayout?: 'row' | 'column' | 'burger'
  /** Container width (px) below which a row menu becomes a burger; 0 = never. */
  burgerAt?: number
  /** 5…100 */
  words?: number
  prefix?: string
  placeholder?: string
  button?: string
  /** ≤ 200 characters */
  empty?: string
  /** language-switcher: a select; shop-cart: the mini cart in a disclosure panel */
  dropdown?: boolean
  /** language-switcher */
  flags?: boolean
  /** language-switcher (default true) */
  names?: boolean
  /** language-switcher */
  hideCurrent?: boolean
  /** shop-products */
  source?: (typeof SHOP_SOURCES)[number]
  /** 1…6 */
  columns?: number
  /** 1…24 */
  limit?: number
  orderBy?: (typeof SHOP_ORDER_BYS)[number]
  order?: 'desc' | 'asc'
  /** product_cat ids, SHOP_IDS_PATTERN */
  cats?: string
  /** product_tag ids */
  tags?: string
  /** product ids (source 'ids') */
  ids?: string
  /** product.* widgets, shop-add-to-cart: a product id; 0 / absent = the current product */
  product?: number
  paginate?: boolean
  showPrice?: boolean
  hideEmpty?: boolean
  /** shop-categories: '' every level, '0' top level, or a category id */
  parent?: string
  showTotal?: boolean
  /** product-images: false = the featured image only */
  gallery?: boolean
}

export const SHOP_SOURCES = ['recent', 'featured', 'sale', 'best-selling', 'top-rated', 'ids'] as const
export const SHOP_ORDER_BYS = ['date', 'title', 'price', 'price-desc', 'popularity', 'rating', 'rand', 'menu_order'] as const
export const SHOP_LIMITS = { maxColumns: 6, maxLimit: 24, maxIds: 24 } as const
export const SHOP_IDS_PATTERN = /^[1-9][0-9]{0,18}(,[1-9][0-9]{0,18}){0,23}$/

export const DYN_LIMITS = { maxSepRunes: 8, maxEmptyRunes: 200, minWords: 5, maxWords: 100, minRatio: 0.2, maxRatio: 5 } as const
export const DYN_FORMAT_PATTERN = /^[A-Za-z ,.:/-]{1,32}$/

/** Section.content keys of a 'loop' section (all strings); card = items[0].sub, grid = section.grid. */
export interface LoopContent {
  source?: 'main' | 'query' | 'related'
  postType?: string
  /** '1'…'24' */
  perPage?: string
  orderBy?: 'date' | 'modified' | 'title' | 'menu_order' | 'comment_count' | 'rand'
  order?: 'desc' | 'asc'
  /** 'category:12,post_tag:4', ≤ 20 pairs */
  terms?: string
  /** '<user id>' | '' */
  author?: string
  exclude?: 'current' | ''
  /** '0'…'50' */
  offset?: string
  /** source main only */
  pagination?: 'numbers' | 'prev-next' | 'none'
  empty?: string
}

export const LOOP_LIMITS = { maxPerPage: 24, maxTerms: 20, maxOffset: 50, maxDepth: 2, maxIterations: 50 } as const

/** nav / footer Section.content binding keys. */
export const CONTENT_MENU = 'menu'
export const CONTENT_LOGO_SOURCE = 'logoSource'
export const LOGO_SOURCE_SITE = 'site'

/** Snapshot placeholder attributes. Not data-wb-menu (the runtime disclosure menu): data-wb-menu-loc. */
export const DYN_ATTR = {
  slot: 'data-wb-slot',
  if: 'data-wb-if',
  ifNot: 'data-wb-if-not',
  loop: 'data-wb-loop',
  loopItem: 'data-wb-loop-item',
  loopEmpty: 'data-wb-loop-empty',
  menuLoc: 'data-wb-menu-loc',
  loopPart: 'data-wb-loop-part',
  format: 'data-wb-format',
  size: 'data-wb-size',
  tax: 'data-wb-tax',
  sep: 'data-wb-sep',
  words: 'data-wb-words',
  field: 'data-wb-field',
  search: 'data-wb-search',
  item: 'data-wb-item',
} as const

export const CLASS_CURRENT = 'is-current'
export const SEARCH_INPUT_NAME = 's'

export type SlotMode = 'text' | 'html' | 'attr'
export const SLOTS = {
  'site.title': 'text',
  'site.tagline': 'text',
  'site.url': 'attr',
  'site.logo': 'attr',
  'site.year': 'text',
  'menu.label': 'text',
  'menu.url': 'attr',
  'menu.link': 'attr',
  'post.field': 'text',
  'post.field-url': 'attr',
  'post.field-html': 'html',
  'post.field-attrs': 'attr',
  'post.title-text': 'text',
  'post.term-names': 'text',
  'post.title': 'html',
  'post.url': 'attr',
  'post.content': 'html',
  'post.excerpt': 'html',
  'post.date': 'text',
  'post.modified': 'text',
  'post.author': 'text',
  'post.author-url': 'attr',
  'post.avatar': 'attr',
  'post.author-bio': 'text',
  'post.image': 'attr',
  'post.terms': 'html',
  'post.comments-count': 'text',
  'post.comments-url': 'attr',
  'post.nav': 'html',
  'post.comments': 'html',
  'archive.title': 'html',
  'archive.description': 'html',
  'search.query': 'text',
  'search.count': 'text',
  'loop.pagination': 'html',
  'site.languages': 'html',
  'shop.archive-title': 'text',
  'shop.cart-url': 'attr',
  'shop.checkout-url': 'attr',
  'shop.products': 'html',
  'shop.categories': 'html',
  'shop.archive': 'html',
  'shop.archive-desc': 'html',
  'shop.breadcrumb': 'html',
  'shop.notices': 'html',
  'shop.cart-count': 'html',
  'shop.cart-subtotal': 'html',
  'shop.mini-cart': 'html',
  'shop.cart-page': 'html',
  'shop.checkout-page': 'html',
  'product.title': 'text',
  'product.on-sale': 'text',
  'product.sku': 'text',
  'product.url': 'attr',
  'product.image': 'attr',
  'product.gallery': 'html',
  'product.price': 'html',
  'product.rating': 'html',
  'product.short-description': 'html',
  'product.description': 'html',
  'product.add-to-cart': 'html',
  'product.add-to-cart-button': 'html',
  'product.meta': 'html',
  'product.stock': 'html',
  'product.tabs': 'html',
  'product.related': 'html',
  'product.upsells': 'html',
} as const satisfies Record<string, SlotMode>
export type SlotName = keyof typeof SLOTS
// HTML attribute names, not UI text. i18n-ignore
export const SLOT_ATTRS = ['href', 'src', 'srcset', 'sizes', 'width', 'height', 'alt', 'datetime', 'aria-current', 'class', 'target'] as const

/* ---------- Sample data: preview-context and Boot.dynamic ---------- */

export interface DynImage {
  url: string
  width: number
  height: number
  alt: string
}

export interface DynPost {
  id: number
  type: string
  title: string
  url: string
  excerpt: string
  /** UGC-sanitized; shown read-only. */
  contentHtml: string
  /** RFC 3339 */
  date: string
  /** date in the site's date_format */
  dateText: string
  modified: string
  modifiedText: string
  author: { name: string; url: string; avatar: string; bio: string }
  image: DynImage | null
  terms: Record<string, { name: string; url: string }[]>
  commentsCount: number
  commentsUrl: string
}

export interface DynMenuItem {
  id: number
  label: string
  url: string
  current: boolean
  /** Always [] in v1 (top level only). */
  children: DynMenuItem[]
}

export interface MenuLocation {
  slug: string
  name: string
}

export type DynViewKind = 'single' | 'page' | 'front-page' | 'home' | 'archive' | 'search' | '404'

export interface DynContext {
  object: PreviewObject
  site: { title: string; tagline: string; home: string; logo: DynImage | null; year: string }
  locations: MenuLocation[]
  menus: Record<string, DynMenuItem[]>
  view: { kind: DynViewKind; archiveTitle: string; archiveDesc: string; search: string; total: number }
  post: DynPost | null
  posts: DynPost[]
  pagination: { current: number; total: number }
  /** Pursae Commerce sample data; null (or absent from older servers) while it is off. */
  shop?: DynShop | null
  /** Pursae Multi Language languages; [] (or absent) while it is off. */
  languages?: DynLanguage[] | null
}

export interface DynLanguage {
  slug: string
  name: string
  /** emoji, '' without one */
  flag: string
  url: string
  current: boolean
}

export interface DynProduct {
  id: number
  /** simple | variable | grouped | external */
  type: string
  title: string
  url: string
  sku: string
  /** the store's price markup (trusted) */
  priceHtml: string
  onSale: boolean
  inStock: boolean
  stockHtml: string
  rating: number
  ratingCount: number
  /** UGC-sanitized */
  shortHtml: string
  /** UGC-sanitized */
  contentHtml: string
  image: DynImage | null
  gallery: DynImage[]
  /** product_cat ids */
  categories: number[]
  addToCartText: string
}

export interface DynProductCat {
  id: number
  name: string
  url: string
  count: number
  parent: number
  image: DynImage | null
}

export interface DynShop {
  /** the preview product (single-product template, product doc), null elsewhere */
  product: DynProduct | null
  /** newest catalog products, ≤ 24 */
  products: DynProduct[]
  /** product_cat, ≤ 100 */
  categories: DynProductCat[]
  cart: { count: number; subtotalHtml: string }
  shopUrl: string
  cartUrl: string
  checkoutUrl: string
}

/* ---------- Boot additions ---------- */

export interface TemplatePreview {
  object: PreviewObject
  label: string
  /** Front URL of the object with ?wb_template_preview=<id>. */
  url: string
}

export interface TemplateBoot {
  type: TemplateType
  conditions: string[]
  priority: number
  starter: StarterId
  preview: TemplatePreview
  /** Appearance › Theme Builder */
  manageUrl: string
}

export interface PostTypeInfo {
  slug: string
  label: string
  /** Archive URL, '' without one. */
  archive: string
  hierarchical: boolean
}

export interface TaxonomyInfo {
  slug: string
  label: string
  types: string[]
}

/** GET/PUT templates/{id}/popup (webbuilder PopupSettings). */
export type PopupTrigger = 'load' | 'scroll' | 'exit' | 'click'
export type PopupFrequency = 'always' | 'session' | 'once' | 'days'
export type PopupPosition = 'center' | 'top' | 'bottom'
export interface PopupSettings {
  trigger: PopupTrigger
  /** seconds, 0…POPUP_LIMITS.maxDelay */
  delay: number
  /** percent 1…100 (trigger scroll) */
  scroll: number
  /** trigger click: "#id" or ".class" (POPUP_SELECTOR_PATTERN) */
  selector: string
  frequency: PopupFrequency
  /** 1…POPUP_LIMITS.maxDays (frequency days) */
  days: number
  /** bump to show a once/days popup again to everyone */
  version: number
  position: PopupPosition
  /** px, POPUP_LIMITS.minWidth…maxWidth */
  width: number
  overlay: boolean
  afterConsent: boolean
}
export const POPUP_LIMITS = { maxDelay: 300, maxDays: 365, minWidth: 200, maxWidth: 1600 } as const
export const POPUP_SELECTOR_PATTERN = /^[#.][A-Za-z_][A-Za-z0-9_-]{0,63}$/

/** One template as a file (GET templates/{id}/export, POST templates/import). */
export interface TemplateFile {
  format: 'web-builder-template'
  version: 1
  type: TemplateType
  title: string
  conditions: string[]
  priority: number
  popup?: PopupSettings
  doc: DocState
}

export interface RoleInfo {
  slug: string
  label: string
}

export interface SiteBoot {
  title: string
  tagline: string
  home: string
  logo: DynImage | null
  locations: MenuLocation[]
  menus: Record<string, DynMenuItem[]>
  /** Hello Pursae is active: pages may use layout 'site'. */
  themeParts: boolean
  postTypes: PostTypeInfo[]
  taxonomies: TaxonomyInfo[]
  /** The site's roles, for the audience rules (user/role/<slug>). */
  roles?: RoleInfo[]
}

/* ---------- Template REST payloads ---------- */

export interface TemplateSummary {
  id: number
  title: string
  type: TemplateType
  /** publish = active, draft = disabled */
  status: string
  conditions: string[]
  priority: number
  author: UserRef
  modified: string
  editUrl: string
  previewUrl: string
}

/** One template of the editor's page switcher (GET navigator; edit_theme_options only). */
export interface NavigatorTemplate {
  id: number
  title: string
  type: TemplateType
  typeLabel: string
  status: string
  editUrl: string
  /** a draft template is not shown on the site */
  disabled: boolean
}

export interface NavigatorPost {
  id: number
  title: string
  postType: string
  typeLabel: string
  status: string
  editUrl: string
}

/** GET navigator: at most 200 templates and 100 posts per enabled post type, newest modified first. */
export interface NavigatorResponse {
  templates: NavigatorTemplate[]
  posts: NavigatorPost[]
}

export interface TemplateCreateRequest {
  type: TemplateType
  title: string
  starter?: StarterId
  /** Required for 'single' (a public custom post type), refused otherwise. */
  postType?: string
}

export interface TemplateCreateResponse {
  id: number
  editUrl: string
}

export interface TemplateConditionsRequest {
  conditions: string[]
  priority: number
}
export type TemplateConditionsResponse = TemplateConditionsRequest

export interface TemplatePreviewRequest {
  object: PreviewObject
}

export interface TemplatePreviewResponse {
  preview: TemplatePreview
  dynamic: DynContext
}

export interface TemplateConflict {
  id: number
  title: string
  type: TemplateType
  conditions: string[]
  priority: number
  editUrl: string
}
