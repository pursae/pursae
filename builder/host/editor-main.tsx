/**
 * The editor proper, loaded by ./editor-entry once the UI language is set (see there).
 *
 * Reads window.__WEB_BUILDER__ (written by the editor screen), installs the CMS host with setHost()
 * before the builder first renders, then mounts upstream's App and the host layer (notices, media
 * dialog) into #root.
 */
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from '@/App'
import { setHost } from '@/builder/host'
import { setThemeLibrarySeam } from '@/builder/themeLibraryHost'
import { setSiteStyleSeam } from '@/builder/siteStyleHost'
import { createSiteStyleSeam } from './site-style'
import { createCmsHost, fontsBase, readBoot, type CmsHost } from './cms-host'
import { HostLayer, isolateHostKeys } from './HostLayer'
import { installTemplateUi } from './TemplateBar'
import { t } from './i18n'
import { ThemeLibraryController } from './theme-library'
import { ThemeLibraryDialog } from './ThemeLibraryDialog'
import { SiteSettingsController } from './site-settings'
import { SiteSettingsDialog, SiteSettingsMenuRow } from './SiteSettingsDialog'
import { PageToolsController } from './page-tools'
import { PageToolsDialog, PageToolsMenuRow } from './PageToolsDialog'
import { PopupController, RevisionsController } from './tools'
import { PopupDialog, PopupMenuRow, RevisionsDialog, RevisionsMenuRow, ScheduleDialog, ScheduleMenuRow } from './HostToolsDialogs'
import { installElements } from './elements'

function mountPoint(): HTMLElement {
  let el = document.getElementById('root')
  if (!el) {
    el = document.createElement('div')
    el.id = 'root'
    document.body.appendChild(el)
  }
  return el
}

/**
 * The fonts css2 endpoint needs the wp_rest nonce in `_wpnonce` (a stylesheet link cannot send
 * headers), but upstream builds links as `${host.fontsCss}?family=…`. Add the current nonce to every
 * stylesheet link that points at the endpoint, before the browser requests it.
 */
function installFontNonce(host: CmsHost) {
  const base = fontsBase(host.boot)
  const desc = Object.getOwnPropertyDescriptor(HTMLLinkElement.prototype, 'href')
  if (!base || !desc?.set || !desc.get) return
  const withNonce = (v: string) => {
    if (typeof v !== 'string' || !v.startsWith(base + '?') || /[?&]_wpnonce=/.test(v)) return v
    return `${v}&_wpnonce=${encodeURIComponent(host.rest.nonce())}`
  }
  Object.defineProperty(HTMLLinkElement.prototype, 'href', {
    configurable: true,
    enumerable: desc.enumerable,
    get: desc.get,
    set(this: HTMLLinkElement, v: string) {
      desc.set!.call(this, withNonce(v))
    },
  })
}

function fail(message: string) {
  const el = mountPoint()
  el.innerHTML = ''
  const box = document.createElement('div')
  box.setAttribute('role', 'alert')
  box.style.cssText = 'max-width:520px;margin:15vh auto;padding:24px;font:14px/1.5 system-ui,sans-serif;color:#eef0f3;background:#17181c;border:1px solid #393c45;border-radius:12px'
  const h = document.createElement('strong')
  h.textContent = 'Pursae Web Builder could not start'
  const p = document.createElement('p')
  p.textContent = message
  box.append(h, p)
  el.append(box)
}

async function boot() {
  let host: CmsHost
  try {
    host = createCmsHost(readBoot())
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e))
    return
  }
  // Template mode: the type badge, display conditions and "Preview as" in the top bar.
  installTemplateUi(host)
  // Site settings (patch 0014): a row in the builder menu opens a dialog with what the Customizer
  // manages while the builder is off (site identity, homepage, additional CSS).
  const siteSettings = new SiteSettingsController({ rest: host.rest, pickImage: host.media.pick ? () => host.media.pick!('image') : undefined })
  // Page tools: Languages (Pursae Multi Language) and SEO (Pursae SEO) tabs, shown for what the page reports
  // (GET documents/{id}/plugins). Never in template mode; the row hides itself while there is no tab.
  const pageTools = host.templates
    ? null
    : new PageToolsController({
        rest: host.rest,
        postId: host.boot.document.postId,
        flush: () => host.autosaver.flush(),
        pending: () => host.autosaver.pending && !host.session.stopped,
        pickImage: host.media.pick ? () => host.media.pick!('image') : undefined,
      })
  const siteRow = !!host.boot.caps.editThemeOptions
  // Host tools (tools.ts): scheduled publish, the published revisions and, in a popup template,
  // the popup settings.
  const revisions = new RevisionsController({
    rest: host.rest,
    postId: host.boot.document.postId,
    baseRev: () => host.session.baseRev,
    stop: () => host.session.stop('reload'),
    reload: () => location.reload(),
    ask: async (message) => (await host.ui.ask({ tone: 'warning', title: t('Restore this revision?'), message, actions: [{ id: 'restore', label: t('Restore'), primary: true }, { id: 'cancel', label: t('Cancel') }] })) === 'restore',
  })
  const popup = host.boot.template?.type === 'popup' ? new PopupController(host.rest, host.boot.document.postId) : null
  host.menuExtra = (close) => (
    <>
      {siteRow && <SiteSettingsMenuRow controller={siteSettings} close={close} />}
      {pageTools && <PageToolsMenuRow controller={pageTools} close={close} />}
      {popup && <PopupMenuRow c={popup} close={close} />}
      <ScheduleMenuRow c={host.schedule} close={close} />
      <RevisionsMenuRow c={revisions} close={close} />
    </>
  )
  setHost(host)
  window.__WEB_BUILDER_HOST__ = host
  // Site style (patch 0022): the Globals panel follows / saves it; new designs start with it.
  setSiteStyleSeam(createSiteStyleSeam(host.rest, host.boot, (s) => setSiteStyleSeam({ ...s })))
  // Pages of code sections: the element editor's canvas and navigator branch (patch 0020).
  installElements()
  installFontNonce(host)
  // Prefs and the library seed the builder's initial state; never block the editor on them for long.
  await Promise.race([host.start().catch(() => undefined), new Promise((r) => setTimeout(r, 4000))])

  window.addEventListener('beforeunload', (e) => {
    if (!host.autosaver.pending || host.session.stopped) return
    void host.autosaver.flush()
    e.preventDefault()
    e.returnValue = ''
  })
  window.addEventListener('pagehide', () => {
    // keepalive requests survive the unload (a plain fetch is cancelled with the page).
    if (host.autosaver.pending && !host.session.stopped) void host.autosaver.flush({ keepalive: true })
    void host.storage.flushPrefs({ keepalive: true })
    host.dispose()
  })

  const root = mountPoint()
  const layer = document.createElement('div')
  layer.id = 'wb-host-layer'
  document.body.appendChild(layer)
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
  // Below the host layer: the media library it draws must stay on top of this dialog.
  const siteLayer = document.createElement('div')
  siteLayer.id = 'wb-site-settings-layer'
  document.body.insertBefore(siteLayer, layer)
  createRoot(siteLayer).render(<SiteSettingsDialog controller={siteSettings} />)
  isolateHostKeys(siteLayer)
  if (pageTools) {
    const toolsLayer = document.createElement('div')
    toolsLayer.id = 'wb-page-tools-layer'
    document.body.insertBefore(toolsLayer, layer)
    createRoot(toolsLayer).render(<PageToolsDialog controller={pageTools} />)
    isolateHostKeys(toolsLayer)
    void pageTools.probe()
  }
  {
    const toolLayer = document.createElement('div')
    toolLayer.id = 'wb-host-tools-layer'
    document.body.insertBefore(toolLayer, layer)
    createRoot(toolLayer).render(
      <>
        <ScheduleDialog c={host.schedule} />
        <RevisionsDialog c={revisions} />
        {popup && <PopupDialog c={popup} />}
      </>,
    )
    isolateHostKeys(toolLayer)
  }
  createRoot(layer).render(<HostLayer host={host} />)
  isolateHostKeys(layer) // after createRoot: the layer's React listener runs first

  // Theme library (patch 0013): the "Browse theme library" card of the Items panel opens a dialog
  // of its own (the server answers 403 to users without edit_theme_options).
  {
    const themes = new ThemeLibraryController({ rest: host.rest, boot: host.boot })
    const themeLayer = document.createElement('div')
    themeLayer.id = 'wb-theme-library-layer'
    document.body.appendChild(themeLayer)
    createRoot(themeLayer).render(<ThemeLibraryDialog controller={themes} />)
    isolateHostKeys(themeLayer)
    setThemeLibrarySeam({ title: t('Browse theme library'), blurb: t('Ready-made full-site themes · header, footer and pages'), open: themes.open })
  }
}

void boot()
