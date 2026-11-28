/**
 * The element editor's content-binding panel (inspector, Advanced tab; docs/developer/
 * web-builder-code-sections.md, "Binding to content"): binds the selected element of a code
 * section to a Pursae menu, a post loop, a post field or the form endpoint. The binding is stored
 * in the section's content.binds (bind.ts) as one history entry; the design's markup is not
 * touched. A preview lists what the site will show (the menu's items, the loop's posts).
 */
import { useEffect, useMemo, useState, type ComponentType } from 'react'
import { Ctl, Panel, inputCls } from '@/builder/StudioPanel'
import { useB } from '@/builder/store'
import type { Section } from '@/builder/types'
import type { CmsHost } from '../cms-host'
import { commit, latest } from './actions'
import { BIND_FIELDS, bindTarget, defaultFormField, FORM_FIELD_NAMES, kindsFor, newBind, parseBinds, stringifyBinds, withBind, type BindKind, type CodeBind } from './bind'
import { tb } from './bind-tr'
import { useElState } from './state'

const cmsHost = () => (typeof window !== 'undefined' ? (window.__WEB_BUILDER_HOST__ as CmsHost | undefined) : undefined)

const KIND_LABEL = (): Record<BindKind | '', string> => ({ '': tb('Not bound'), menu: tb('Menu'), submenu: tb('Submenu'), loop: tb('Loop'), field: tb('Field'), form: tb('Form') })
const FIELD_LABEL = (): Record<string, string> => ({
  title: tb('Title'),
  excerpt: tb('Excerpt'),
  content: tb('Content'),
  image: tb('Featured image'),
  url: tb('Permalink'),
  link: tb('Link with title'),
  date: tb('Date'),
  modified: tb('Modified date'),
  author: tb('Author'),
  terms: tb('Terms'),
  custom: tb('Custom field'),
  pagination: tb('Pagination'),
})

const selCls = inputCls + ' w-[168px]'

/** The binding panel of the selected element (nothing without a selection in this section). */
export function BindPanel({ s }: { s: Section }) {
  const b = useB()
  const st = useElState()
  const id = st.sel
  const html = s.content.html ?? ''
  const binds = useMemo(() => parseBinds(s.content.binds), [s.content.binds])
  const target = useMemo(() => (id && s.kind === 'code' ? bindTarget(html, id, binds) : null), [html, id, binds, s.kind])
  if (!id || !target) return null
  const cur = binds[id] ?? null
  const save = (next: CodeBind | null) => {
    const doc = latest.doc ?? b.doc
    const sec = doc.sections.find((x) => x.id === s.id)
    if (!sec) return
    const json = stringifyBinds(withBind(parseBinds(sec.content.binds), id, next))
    if ((sec.content.binds ?? '') === json) return
    commit(b, tb('Binding changed'), { html: {} }, { [s.id]: { binds: json } })
  }
  const kinds = kindsFor(target)
  return (
    <Panel title={tb('Content binding')}>
      <div data-wb-bind-panel={id}>
        <Ctl label={tb('Bind to')}>
          <select
            className={selCls}
            value={cur?.kind ?? ''}
            data-wb-bind-kind=""
            onChange={(e) => save(e.target.value ? newBind(e.target.value as BindKind, target) : null)}
          >
            <option value="">{KIND_LABEL()['']}</option>
            {kinds.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL()[k]}
              </option>
            ))}
          </select>
        </Ctl>
        {cur?.kind === 'menu' && <MenuFields b={cur} save={save} />}
        {cur?.kind === 'submenu' && <p className="text-[11px] text-ink-500">{tb('Fills its dropdown list from the item’s sub-items (one level).')}</p>}
        {cur?.kind === 'loop' && <LoopFields b={cur} save={save} />}
        {cur?.kind === 'field' && <FieldFields b={cur} save={save} />}
        {cur?.kind === 'form' && <FormFields b={cur} save={save} inputs={target.inputs} messages={target.messages} />}
      </div>
    </Panel>
  )
}

type FieldsProps = { b: CodeBind; save: (b: CodeBind | null) => void }

function Text({ label, value, onChange, placeholder, mono }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string; mono?: boolean }) {
  const [v, setV] = useState(value)
  useEffect(() => setV(value), [value])
  return (
    <Ctl label={label}>
      <input className={selCls + (mono ? ' font-mono' : '')} value={v} placeholder={placeholder} onChange={(e) => setV(e.target.value)} onBlur={() => v !== value && onChange(v.trim())} onKeyDown={(e) => e.key === 'Enter' && (e.target as HTMLInputElement).blur()} />
    </Ctl>
  )
}

function MenuFields({ b, save }: FieldsProps) {
  const host = cmsHost()
  const ctx = host?.dyn.get()
  const locs = ctx?.locations ?? []
  const items = b.location ? (ctx?.menus[b.location] ?? []) : []
  return (
    <div className="mt-3 space-y-3">
      <Ctl label={tb('Location')}>
        <select className={selCls} value={b.location ?? ''} onChange={(e) => save({ ...b, location: e.target.value || undefined, menu: e.target.value ? undefined : b.menu })}>
          <option value="">{tb('Menu id')}</option>
          {locs.map((l) => (
            <option key={l.slug} value={l.slug}>
              {l.name}
            </option>
          ))}
          {b.location && !locs.some((l) => l.slug === b.location) && <option value={b.location}>{b.location}</option>}
        </select>
      </Ctl>
      {!b.location && <Text label={tb('Menu id')} value={b.menu ? String(b.menu) : ''} onChange={(v) => save({ ...b, menu: Number(v) || undefined })} mono />}
      <p className="text-[11px] leading-snug text-ink-500">{tb('The items of the list are templates: each menu item uses the one it was imported from, new items the first plain one. Mark a dropdown list inside an item as Submenu.')}</p>
      {b.location && <Preview titles={items.map((i) => i.label)} />}
    </div>
  )
}

function LoopFields({ b, save }: FieldsProps) {
  const q = b.query ?? {}
  const set = (k: string, v: string) => {
    const next = { ...q }
    if (v) next[k] = v
    else delete next[k]
    save({ ...b, query: next })
  }
  const [titles, setTitles] = useState<string[] | null>(null)
  const key = JSON.stringify(q)
  useEffect(() => {
    const host = cmsHost()
    if (!host) return
    let live = true
    setTitles(null)
    const type = q.postType || 'post'
    const route = type === 'post' ? '/wp/v2/posts' : type === 'page' ? '/wp/v2/pages' : `/wp/v2/${type}`
    // The REST API answers 400 outside 1..100 (like WordPress); the preview only needs a sample.
    const n = parseInt(q.perPage || '', 10)
    const query: Record<string, string> = { per_page: String(n >= 1 ? Math.min(n, 100) : 6), orderby: q.orderBy === 'menu_order' ? 'menu_order' : q.orderBy === 'title' ? 'title' : q.orderBy === 'modified' ? 'modified' : 'date', order: q.order || 'desc', _fields: 'id,title' }
    if (q.offset) query.offset = q.offset
    if (q.parent) query.parent = q.parent
    host.rest
      .request<{ title?: { rendered?: string } }[]>('GET', route, { query })
      .then((r) => live && setTitles((r.data ?? []).map((p) => (p.title?.rendered ?? '').replace(/<[^>]*>/g, ''))))
      .catch(() => live && setTitles([]))
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
  return (
    <div className="mt-3 space-y-3">
      <Text label={tb('Post type')} value={q.postType ?? 'post'} onChange={(v) => set('postType', v)} mono />
      <Text label={tb('Parent page id')} value={q.parent ?? ''} onChange={(v) => set('parent', v)} mono />
      <Text label={tb('Terms')} value={q.terms ?? ''} onChange={(v) => set('terms', v)} placeholder="category:12" mono />
      <Text label={tb('Count')} value={q.perPage ?? ''} onChange={(v) => set('perPage', v)} mono />
      <Ctl label={tb('Order by')}>
        <select className={selCls} value={q.orderBy ?? 'date'} onChange={(e) => set('orderBy', e.target.value)}>
          {['date', 'modified', 'title', 'menu_order', 'comment_count', 'rand'].map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      </Ctl>
      <Ctl label={tb('Order')}>
        <select className={selCls} value={q.order ?? 'desc'} onChange={(e) => set('order', e.target.value)}>
          <option value="desc">desc</option>
          <option value="asc">asc</option>
        </select>
      </Ctl>
      <Text label={tb('Offset')} value={q.offset ?? ''} onChange={(v) => set('offset', v)} mono />
      <p className="text-[11px] leading-snug text-ink-500">{tb('The first child is the card template; bind its elements to fields. The other children are the design’s samples and are left out on the site.')}</p>
      <Preview titles={titles} />
    </div>
  )
}

function FieldFields({ b, save }: FieldsProps) {
  const labels = FIELD_LABEL()
  return (
    <div className="mt-3 space-y-3">
      <Ctl label={tb('Field')}>
        <select className={selCls} value={b.field ?? 'title'} onChange={(e) => save({ kind: 'field', field: e.target.value as CodeBind['field'] })}>
          {BIND_FIELDS.map((f) => (
            <option key={f} value={f}>
              {labels[f]}
            </option>
          ))}
        </select>
      </Ctl>
      {(b.field === 'custom' || b.field === 'image') && <Text label={tb('Field name')} value={b.name ?? ''} onChange={(v) => save({ ...b, name: v || undefined })} mono />}
      {b.field === 'custom' && (
        <Ctl label={tb('Fills')}>
          <select className={selCls} value={b.attr ?? ''} onChange={(e) => save({ ...b, attr: e.target.value || undefined })}>
            <option value="">{tb('Text')}</option>
            <option value="html">HTML</option>
            <option value="url">{tb('Link or image URL')}</option>
            <option value="attrs">{tb('Attributes (<name>_href, <name>_src, …)')}</option>
          </select>
        </Ctl>
      )}
      {(b.field === 'date' || b.field === 'modified') && <Text label={tb('Date format')} value={b.format ?? ''} onChange={(v) => save({ ...b, format: v || undefined })} placeholder="d.m.Y" mono />}
      {b.field === 'terms' && (
        <>
          <Text label={tb('Taxonomy')} value={b.tax ?? ''} onChange={(v) => save({ ...b, tax: v || undefined })} placeholder="category" mono />
          <Text label={tb('Separator')} value={b.sep ?? ''} onChange={(v) => save({ ...b, sep: v || undefined })} placeholder=", " />
        </>
      )}
      {b.field === 'excerpt' && <Text label={tb('Words')} value={b.words ? String(b.words) : ''} onChange={(v) => save({ ...b, words: Number(v) || undefined })} mono />}
      <p className="text-[11px] leading-snug text-ink-500">{tb('Filled from the post of the loop card, else from the post the template shows.')}</p>
    </div>
  )
}

function FormFields({ b, save, inputs, messages }: FieldsProps & { inputs: { name: string; type: string; label: string }[]; messages: { id: string; label: string }[] }) {
  const fields = b.fields ?? {}
  const req = new Set(b.required ?? [])
  const setField = (name: string, v: string) => {
    const next = { ...fields }
    if (v && v !== defaultFormField(name)) next[name] = v
    else delete next[name]
    save({ ...b, fields: Object.keys(next).length ? next : undefined })
  }
  const toggleReq = (name: string) => {
    const next = new Set(req)
    if (next.has(name)) next.delete(name)
    else next.add(name)
    save({ ...b, required: next.size ? [...next].sort() : undefined })
  }
  const msgSelect = (label: string, k: 'done' | 'error') => (
    <Ctl label={label}>
      <select className={selCls} value={b[k] ?? ''} onChange={(e) => save({ ...b, [k]: e.target.value || undefined })}>
        <option value="">{tb('Default')}</option>
        {messages.map((m) => (
          <option key={m.id} value={m.id}>
            {m.label}
          </option>
        ))}
      </select>
    </Ctl>
  )
  return (
    <div className="mt-3 space-y-3">
      {inputs.map((i) => {
        const def = defaultFormField(i.name)
        const v = fields[i.name] ?? def
        const options = [...new Set([...FORM_FIELD_NAMES, def, v].filter(Boolean))]
        return (
          <div key={i.name} className="space-y-1">
            <Ctl label={i.label}>
              <select className={selCls} value={v} onChange={(e) => setField(i.name, e.target.value)}>
                {options.map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
                <option value="-">{tb('Leave out')}</option>
              </select>
            </Ctl>
            <label className="flex items-center gap-2 pl-1 text-[11px] text-ink-400">
              <input type="checkbox" checked={req.has(i.name)} onChange={() => toggleReq(i.name)} />
              {tb('Required')} <code className="text-ink-500">{i.name}</code>
            </label>
          </div>
        )
      })}
      {msgSelect(tb('Success message'), 'done')}
      {msgSelect(tb('Error message'), 'error')}
      <Text label={tb('Recipients')} value={b.to ?? ''} onChange={(v) => save({ ...b, to: v || undefined })} placeholder="info@example.com" />
      <p className="text-[11px] leading-snug text-ink-500">{tb('The form posts to Pursae: submissions are stored and mailed (Settings › Web Builder). The design’s own validation still runs.')}</p>
    </div>
  )
}

function Preview({ titles }: { titles: string[] | null }) {
  if (titles === null) return <p className="text-[11px] text-ink-500">{tb('Loading…')}</p>
  if (!titles.length) return <p className="text-[11px] text-ink-500">{tb('On the site')}: {tb('No items.')}</p>
  return (
    <div className="rounded-[5px] border border-white/[0.07] p-2 text-[11px] text-ink-300" data-wb-bind-preview="">
      <div className="mb-1 text-ink-500">{tb('Shows {n} items:', { n: titles.length })}</div>
      <ol className="list-decimal space-y-0.5 pl-4">
        {titles.slice(0, 24).map((x, i) => (
          <li key={i}>{x}</li>
        ))}
      </ol>
    </div>
  )
}

/** The inspector tab with the binding panel above the Advanced tab's tools. */
export function withBindPanel<P extends { s: Section; tab: 'style' | 'advanced' }>(Tab: ComponentType<P>): ComponentType<P> {
  function TabWithBinding(props: P) {
    return (
      <>
        {props.tab === 'advanced' && <BindPanel s={props.s} />}
        <Tab {...props} />
      </>
    )
  }
  return TabWithBinding
}
