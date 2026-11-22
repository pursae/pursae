/**
 * Site settings of the CMS host (docs/developer/web-builder-contract.md, web-builder/v2/site-settings):
 * what the Customizer manages while the builder is off, moved into the editor: site title and
 * tagline, site icon, logo, homepage and additional CSS. The server validates with the Customizer's
 * own rules and stores the values in the same places, so both screens always agree.
 *
 * State lives in SiteSettingsController (SiteSettingsDialog.tsx draws it); the "Site settings" row of
 * the builder menu (BuilderHost.menuExtra, patch 0014) only calls open().
 */
import type { MediaAsset } from '@/builder/host'
import { t } from './i18n'
import type { RestClient } from './rest'
import { errorText } from './theme-library'

export const siteSettingsRoute = () => 'web-builder/v2/site-settings'

export interface SiteImage {
  id: number
  url: string
}

export interface SitePage {
  id: number
  title: string
}

/**
 * Design tokens (option web_builder_tokens): colours and fonts printed as --wb-color-<name> and
 * --wb-font-<name> on every page with builder parts. The server drops invalid values.
 */
export interface DesignTokens {
  colors: Partial<Record<TokenColor, string>>
  fonts: Partial<Record<TokenFont, string>>
}
export const TOKEN_COLORS = ['primary', 'secondary', 'accent', 'text', 'background'] as const
export const TOKEN_FONTS = ['heading', 'body'] as const
export type TokenColor = (typeof TOKEN_COLORS)[number]
export type TokenFont = (typeof TOKEN_FONTS)[number]

const tokensOf = (x?: DesignTokens | null): DesignTokens => ({ colors: { ...(x?.colors ?? {}) }, fonts: { ...(x?.fonts ?? {}) } })
const tokensKey = (x?: DesignTokens | null) => {
  const n = tokensOf(x)
  const pick = (o: Record<string, string | undefined>, keys: readonly string[]) => keys.map((k) => (o[k] ?? '').trim())
  return JSON.stringify([pick(n.colors, TOKEN_COLORS), pick(n.fonts, TOKEN_FONTS)])
}

/** GET site-settings. */
export interface SiteSettings {
  blogname: string
  blogdescription: string
  site_icon: SiteImage | null
  custom_logo: SiteImage | null
  supports_logo: boolean
  show_on_front: 'posts' | 'page'
  page_on_front: number
  page_for_posts: number
  custom_css: string
  pages: SitePage[]
  can_upload: boolean
  /** Absent on an older server. */
  tokens?: DesignTokens
  /** The site style designs follow (site-style.ts); null while none was saved. Absent on an older server. */
  site_style?: Record<string, unknown> | null
  /** Designs that follow the site style and still show an older one. */
  site_style_outdated?: number
}

/** The fields the dialog edits (and PUT sends): images by attachment id, 0 removes. */
export interface SiteDraft {
  blogname: string
  blogdescription: string
  site_icon: SiteImage | null
  custom_logo: SiteImage | null
  show_on_front: 'posts' | 'page'
  page_on_front: number
  page_for_posts: number
  custom_css: string
  tokens: DesignTokens
}

export interface SiteSettingsState {
  open: boolean
  loading: boolean
  saving: boolean
  /** Load / save problem. */
  error: string
  /** Shown after a successful save. */
  saved: boolean
  data: SiteSettings | null
  draft: SiteDraft | null
}

const INITIAL: SiteSettingsState = { open: false, loading: false, saving: false, error: '', saved: false, data: null, draft: null }

export interface SiteSettingsDeps {
  rest: RestClient
  /** Opens the media library (host.media.pick); absent = no library button. */
  pickImage?: () => Promise<MediaAsset | null>
}

const draftOf = (d: SiteSettings): SiteDraft => ({
  blogname: d.blogname,
  blogdescription: d.blogdescription,
  site_icon: d.site_icon,
  custom_logo: d.custom_logo,
  show_on_front: d.show_on_front,
  page_on_front: d.page_on_front,
  page_for_posts: d.page_for_posts,
  custom_css: d.custom_css,
  tokens: tokensOf(d.tokens),
})

/** The PUT body: only what differs from the loaded settings. */
export function diffDraft(data: SiteSettings, draft: SiteDraft): Record<string, string | number | DesignTokens> {
  const out: Record<string, string | number | DesignTokens> = {}
  const base = draftOf(data)
  for (const key of Object.keys(draft) as (keyof SiteDraft)[]) {
    const a = base[key]
    const b = draft[key]
    if (key === 'site_icon' || key === 'custom_logo') {
      const x = (a as SiteImage | null)?.id ?? 0
      const y = (b as SiteImage | null)?.id ?? 0
      if (x !== y) out[key] = y
    } else if (key === 'tokens') {
      if (tokensKey(a as DesignTokens) !== tokensKey(b as DesignTokens)) out.tokens = tokensOf(b as DesignTokens)
    } else if (a !== b) {
      out[key] = b as string | number
    }
  }
  return out
}

export class SiteSettingsController {
  private state: SiteSettingsState = INITIAL
  private loadSeq = 0
  private listeners = new Set<() => void>()

  constructor(private d: SiteSettingsDeps) {}

  subscribe = (f: () => void) => {
    this.listeners.add(f)
    return () => {
      this.listeners.delete(f)
    }
  }
  getState = () => this.state

  private set(patch: Partial<SiteSettingsState>) {
    this.state = { ...this.state, ...patch }
    this.listeners.forEach((f) => f())
  }

  get canPick() {
    return !!this.d.pickImage
  }

  /** True when the draft differs from what the server has. */
  get dirty() {
    const { data, draft } = this.state
    return !!data && !!draft && Object.keys(diffDraft(data, draft)).length > 0
  }

  open = () => {
    this.set({ open: true, error: '', saved: false })
    void this.load()
  }

  close = () => {
    if (this.state.saving) return
    this.set({ open: false })
  }

  async load() {
    const draft = this.state.draft
    const seq = ++this.loadSeq
    this.set({ loading: true, error: '' })
    try {
      const r = await this.d.rest.request<SiteSettings>('GET', siteSettingsRoute())
      if (seq !== this.loadSeq) return
      const next = draftOf(r.data)
      const latest = this.state.draft
      if (draft && latest) {
        for (const key of Object.keys(draft) as (keyof SiteDraft)[]) {
          if (latest[key] !== draft[key]) Object.assign(next, { [key]: latest[key] })
        }
      }
      this.set({ loading: false, data: r.data, draft: next })
    } catch (e) {
      if (seq !== this.loadSeq) return
      const edited = draft && this.state.draft !== draft
      this.set({ loading: false, ...(edited ? {} : { data: null, draft: null }), error: errorText(e, t('The site settings could not be loaded.')) })
    }
  }

  edit = (patch: Partial<SiteDraft>) => {
    if (!this.state.draft) return
    this.set({ draft: { ...this.state.draft, ...patch }, saved: false, error: '' })
  }

  /** Chooses an image from the media library for the site icon or the logo. */
  pick = async (field: 'site_icon' | 'custom_logo') => {
    if (!this.d.pickImage) return
    const a = await this.d.pickImage()
    if (a?.id) this.edit({ [field]: { id: a.id, url: a.url } })
  }

  save = async () => {
    const { data, draft } = this.state
    if (!data || !draft || this.state.saving) return
    const body = diffDraft(data, draft)
    if (!Object.keys(body).length) return
    this.set({ saving: true, error: '', saved: false })
    try {
      const r = await this.d.rest.request<SiteSettings>('PUT', siteSettingsRoute(), { json: body })
      const next = draftOf(r.data)
      const latest = this.state.draft
      if (latest) {
        for (const key of Object.keys(draft) as (keyof SiteDraft)[]) {
          if (latest[key] !== draft[key]) Object.assign(next, { [key]: latest[key] })
        }
      }
      this.set({ saving: false, saved: true, data: r.data, draft: next })
    } catch (e) {
      this.set({ saving: false, error: errorText(e, t('The site settings could not be saved.')) })
    }
  }
}
