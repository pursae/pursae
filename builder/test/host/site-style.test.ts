import { describe, expect, it, vi } from 'vitest'
import type { RestClient } from '../../host/rest'
import { createSiteStyleSeam } from '../../host/site-style'
import { followStyle, sameStyle, setSiteStyleSeam, styleOf, withSiteStyle } from '@/builder/siteStyleHost'
import { historyReducer, initHistory } from '@/builder/store'
import type { DocState } from '@/builder/types'

function fakeRest(handler: (method: string, route: string, body: unknown) => unknown) {
  const request = vi.fn(async (method: string, route: string, opts?: { json?: unknown }) => ({ data: await handler(method, route, opts?.json), headers: new Headers(), status: 200 }))
  return { request } as unknown as RestClient & { request: typeof request }
}

const style = { brand: '#0f9b6c', fontPair: 'modern' as const, corners: 'sharp' as const, bodyFont: 'Inter' }

describe('site style helpers', () => {
  it('compares and copies only the style keys', () => {
    expect(styleOf({ ...style, global: true, headingFont: '' })).toEqual(style)
    expect(sameStyle({ ...style, global: false }, style)).toBe(true)
    expect(sameStyle({ ...style, brand: '#000000' }, style)).toBe(false)
    expect(followStyle(style)).toEqual({ ...style, global: true })
  })

  it('starts a new design with the site style, when there is one', () => {
    const doc: DocState = { sections: [], site: { brand: '#4058e3', fontPair: 'elegant', corners: 'soft' } }
    setSiteStyleSeam(null)
    expect(withSiteStyle(doc)).toBe(doc)
    setSiteStyleSeam({ value: style, canEdit: false, save: async () => '' })
    expect(withSiteStyle(doc).site).toEqual({ ...style, global: true })
    setSiteStyleSeam(null)
  })

  it('stops following when a colour, font or corner of the design changes', () => {
    const doc: DocState = { sections: [], site: followStyle(style) as DocState['site'] }
    let h = initHistory(doc)
    h = historyReducer(h, { type: 'site', patch: { brand: '#111111' } })
    expect(h.entries[h.cursor].doc.site).toMatchObject({ brand: '#111111', global: false })
    h = historyReducer(h, { type: 'site', patch: { ...style, global: true } })
    expect(h.entries[h.cursor].doc.site.global).toBe(true)
  })
})

describe('createSiteStyleSeam', () => {
  it('reads the boot style and saves through site-settings', async () => {
    const rest = fakeRest((method, route, body) => {
      expect(method).toBe('PUT')
      expect(route).toBe('web-builder/v2/site-settings')
      expect(body).toEqual({ site_style: { ...style, brand: '#222222' } })
      return { site_style: { ...style, brand: '#222222' }, site_style_outdated: 3 }
    })
    const changed = vi.fn()
    const seam = createSiteStyleSeam(rest, { siteStyle: style, caps: { publish: true, uploadFiles: true, unfilteredHtml: false, editOthers: true, editThemeOptions: true } }, changed)
    expect(seam.value).toEqual(style)
    expect(seam.canEdit).toBe(true)
    const msg = await seam.save({ ...style, brand: '#222222' })
    expect(msg).toContain('3')
    expect(seam.value?.brand).toBe('#222222')
    expect(changed).toHaveBeenCalledTimes(1)
  })

  it('has no style and no editing without one and without edit_theme_options', () => {
    const seam = createSiteStyleSeam(fakeRest(() => ({})), { siteStyle: null, caps: { publish: true, uploadFiles: true, unfilteredHtml: false, editOthers: false } })
    expect(seam.value).toBeNull()
    expect(seam.canEdit).toBe(false)
  })
})
