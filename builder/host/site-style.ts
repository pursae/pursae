/**
 * Site style of the CMS host (docs/developer/web-builder.md, "Site style"): one brand colour, font
 * pair, fonts and corners for the whole site, the Site Editor's global styles. Boot.siteStyle is the
 * stored style; a design whose site.global is true follows it (the server puts the current style
 * into it when it is loaded). Saving goes through PUT site-settings ("site_style"); designs that follow
 * the style keep their published look until they are published again (Regenerate pages).
 */
import type { SiteStyle, SiteStyleSeam } from '@/builder/siteStyleHost'
import type { Boot } from './contract'
import { t } from './i18n'
import type { RestClient } from './rest'
import { siteSettingsRoute, type SiteSettings } from './site-settings'
import { errorText } from './theme-library'

export function createSiteStyleSeam(rest: RestClient, boot: Pick<Boot, 'siteStyle' | 'caps'>, onChange?: (s: SiteStyleSeam) => void): SiteStyleSeam {
  const seam: SiteStyleSeam = {
    value: (boot.siteStyle as SiteStyle | null | undefined) ?? null,
    canEdit: !!boot.caps.editThemeOptions,
    async save(style) {
      let res: SiteSettings
      try {
        res = (await rest.request<SiteSettings>('PUT', siteSettingsRoute(), { json: { site_style: style } })).data
      } catch (e) {
        throw new Error(errorText(e, t('The site style could not be saved.')))
      }
      const next = { ...seam, value: (res.site_style as SiteStyle | null | undefined) ?? style }
      Object.assign(seam, next)
      onChange?.(seam)
      const n = res.site_style_outdated ?? 0
      return n > 0 ? t('Site style saved. {n} pages and templates that follow it keep the old look until they are published again (Settings › Web Builder › Regenerate pages).', { n }) : t('Site style saved')
    },
  }
  return seam
}
