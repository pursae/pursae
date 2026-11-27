/*
 * Pursae Flipbook viewer: a dependency-free page-flip book over pdf.js.
 *
 * The book itself (layout, the page fold with its light and shade, dragging, corner peel, zoom,
 * thumbnails, slider, autoplay, sound) is thecoolPDF's player, kept as it is there; see
 * https://github.com/boracomet/pdf-main (template/player/viewer.js). What differs here:
 *   - pages are drawn by pdf.js (Apache-2.0, ./vendor) into canvases instead of being images made
 *     on the server, with a bounded cache;
 *   - a book is an instance mounted on a [data-pfb] element, so a post can hold several, and all
 *     sizes come from that element, not the window;
 *   - a book in a post leaves the mouse wheel and vertical swipes to the page;
 *   - destroy() releases everything when the element leaves the document.
 *
 * Layout: a landscape box shows double-page spreads with the cover and (for an even page count)
 * the back page on their own, a portrait box one page at a time; the reader can override that
 * with the view button. Wide boxes have a bottom toolbar, arrows over the page edges and an
 * optional thumbnail strip; phones get top/bottom bars. Pages turn around the spine; they can be
 * clicked, dragged/swiped, flipped with the keyboard or the mouse wheel.
 */
import * as pdfjs from './vendor/pdf.min.mjs';
import { createShare } from './share.js';

const TR = /^tr\b/i.test(document.documentElement.lang || navigator.language || '');
const T = TR ? {
  home: 'Ana sayfa', zoomIn: 'Yakınlaştır', zoomOut: 'Uzaklaştır', thumbs: 'Sayfalar',
  play: 'Otomatik çevir', pause: 'Otomatik çevirmeyi durdur', soundOn: 'Sesi kapat', soundOff: 'Sesi aç',
  share: 'Paylaş', download: 'PDF indir', fullscreen: 'Tam ekran', exitFullscreen: 'Tam ekrandan çık',
  prev: 'Önceki sayfa', next: 'Sonraki sayfa', page: 'Sayfa', slider: 'Sayfa seç',
  scrollLeft: 'Sola kaydır', scrollRight: 'Sağa kaydır',
  viewSingle: 'Tek sayfa görünümü', viewDouble: 'Çift sayfa görünümü',
  error: 'PDF açılamadı.', open: 'PDF’i aç'
} : {
  home: 'Home', zoomIn: 'Zoom in', zoomOut: 'Zoom out', thumbs: 'Pages',
  play: 'Auto flip', pause: 'Stop auto flip', soundOn: 'Mute', soundOff: 'Unmute',
  share: 'Share', download: 'Download PDF', fullscreen: 'Fullscreen', exitFullscreen: 'Exit fullscreen',
  prev: 'Previous page', next: 'Next page', page: 'Page', slider: 'Choose page',
  scrollLeft: 'Scroll left', scrollRight: 'Scroll right',
  viewSingle: 'Single-page view', viewDouble: 'Two-page view',
  error: 'The PDF could not be opened.', open: 'Open the PDF'
};

// Toolbar icons as inline SVG (24x24, stroked with currentColor).
const ICON_ATTRS = 'fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"';
const ICONS = {
  home: '<path d="M3.5 10.5 12 3.8l8.5 6.7"/><path d="M5.5 9v10.2a.8.8 0 0 0 .8.8H10v-5.5h4V20h3.7a.8.8 0 0 0 .8-.8V9"/>',
  zoomIn: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m20 20-4.6-4.6M10.5 7.8v5.4M7.8 10.5h5.4"/>',
  zoomOut: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m20 20-4.6-4.6M7.8 10.5h5.4"/>',
  grid: '<rect x="3.5" y="3.5" width="7" height="7" rx="1.6"/><rect x="13.5" y="3.5" width="7" height="7" rx="1.6"/><rect x="3.5" y="13.5" width="7" height="7" rx="1.6"/><rect x="13.5" y="13.5" width="7" height="7" rx="1.6"/>',
  play: '<path d="M7 4.8v14.4a.8.8 0 0 0 1.2.7l11.3-7.2a.8.8 0 0 0 0-1.4L8.2 4.1A.8.8 0 0 0 7 4.8Z"/>',
  pause: '<rect x="6" y="4.5" width="4" height="15" rx="1.2"/><rect x="14" y="4.5" width="4" height="15" rx="1.2"/>',
  soundOn: '<path d="M4 9.5v5a1 1 0 0 0 1 1h3l4.4 3.7a.6.6 0 0 0 1-.5V5.3a.6.6 0 0 0-1-.5L8 8.5H5a1 1 0 0 0-1 1Z"/><path d="M16.5 9a4.2 4.2 0 0 1 0 6M19 6.5a7.8 7.8 0 0 1 0 11"/>',
  soundOff: '<path d="M4 9.5v5a1 1 0 0 0 1 1h3l4.4 3.7a.6.6 0 0 0 1-.5V5.3a.6.6 0 0 0-1-.5L8 8.5H5a1 1 0 0 0-1 1Z"/><path d="m16.5 9.5 5 5M21.5 9.5l-5 5"/>',
  share: '<circle cx="18" cy="5.5" r="2.5"/><circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="18.5" r="2.5"/><path d="m8.2 10.8 7.6-4.1M8.2 13.2l7.6 4.1"/>',
  download: '<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5M4.5 19.5h15"/>',
  // One page, and an open two-page spread, for the view toggle.
  pageSingle: '<rect x="6.5" y="3.5" width="11" height="17" rx="1.6"/>',
  pageDouble: '<path d="M12 5.2v14.6"/><path d="M12 5.2C10 4 7 3.6 3.8 4.3a.8.8 0 0 0-.6.8v13.4a.6.6 0 0 0 .7.6c3-.6 5.9-.2 8.1 1 2.2-1.2 5.1-1.6 8.1-1a.6.6 0 0 0 .7-.6V5.1a.8.8 0 0 0-.6-.8C17 3.6 14 4 12 5.2Z"/>',
  fullscreen: '<path d="M4 9V5a1 1 0 0 1 1-1h4M15 4h4a1 1 0 0 1 1 1v4M20 15v4a1 1 0 0 1-1 1h-4M9 20H5a1 1 0 0 1-1-1v-4"/>',
  exitFullscreen: '<path d="M9 4v4a1 1 0 0 1-1 1H4M20 9h-4a1 1 0 0 1-1-1V4M15 20v-4a1 1 0 0 1 1-1h4M4 15h4a1 1 0 0 1 1 1v4"/>',
  prev: '<path d="m14.5 5.5-6.5 6.5 6.5 6.5"/>',
  next: '<path d="m9.5 5.5 6.5 6.5-6.5 6.5"/>'
};
function icon(name) {
  if (!ICONS[name]) return '';
  return '<svg class="fc-svg" viewBox="0 0 24 24" ' + ICON_ATTRS + ' aria-hidden="true" focusable="false">' + ICONS[name] + '</svg>';
}

const reduceMotion = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;

function store(key, val) {
  try {
    if (val === undefined) return localStorage.getItem(key);
    localStorage.setItem(key, val);
  } catch (_) { /* private mode */ }
  return null;
}

/*
 * createViewer builds the book in host. cfg: { pdf (a pdf.js document), pages, width, height (of
 * the first page), title, full (the book has the whole window: the admin preview), homeURL,
 * download (the PDF's address, or ''), fileName, shareURL, sharePath, qr, hashKey }.
 */
function createViewer(host, cfg) {
  var N = cfg.pages;
  var AR = cfg.width > 0 && cfg.height > 0 ? cfg.width / cfg.height : 0.7071;
  var ac = new AbortController(); // listeners on the window and the document end with the book
  var share = null;

  // Desktop geometry (px).
  var BAR_H = 42, BAR_BOTTOM = 16, GAP = 10, THUMBS_H = 106;

  // ------------------------------------------------------------------ DOM

  function el(tag, cls, html) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html) e.innerHTML = html;
    return e;
  }

  function button(act, cls) {
    var b = el('button', 'fc-btn' + (cls ? ' ' + cls : ''));
    b.type = 'button';
    b.setAttribute('data-act', act);
    return b;
  }

  var app = el('div', 'fc-app');
  var stage = el('div', 'fc-stage');
  var zoomLayer = el('div', 'fc-zoom');
  var book = el('div', 'fc-book');
  var bookShadow = el('div', 'fc-bshadow');
  var edgeL = el('div', 'fc-edge fc-edge-l');
  var edgeR = el('div', 'fc-edge fc-edge-r');

  // A page box: the page image plus the light and shade printed over it
  // (binding shade, and a soft gloss on left-hand pages).
  function pageBox(cls) {
    var s = el('div', cls);
    s.holder = el('div', 'fc-holder');
    s.appendChild(s.holder);
    s.appendChild(el('div', 'fc-gutter'));
    return s;
  }
  var slotL = pageBox('fc-slot fc-slot-l fc-pl');
  var slotR = pageBox('fc-slot fc-slot-r fc-pr');

  // The turning page, drawn as a flat sheet folded along a straight line:
  // the part still lying flat (tFront), the shadow it throws on the page it
  // uncovers (uShade inside uClip), and the folded-over part (flap) showing
  // the back of the sheet, with light and shade along the fold (band).
  var turn = el('div', 'fc-turn');
  var uClip = el('div', 'fc-uclip');
  var uShade = el('div', 'fc-ushade');
  uClip.appendChild(uShade);
  var tFront = pageBox('fc-tfront');
  var flap = pageBox('fc-flap');
  flap.mirror = el('div', 'fc-mirror');
  flap.band = el('div', 'fc-band');
  flap.crease = el('div', 'fc-crease');
  flap.appendChild(flap.mirror);
  flap.appendChild(flap.band);
  flap.appendChild(flap.crease);
  turn.appendChild(uClip);
  turn.appendChild(tFront);
  turn.appendChild(flap);
  [bookShadow, edgeL, edgeR, slotL, slotR, turn].forEach(function (x) { book.appendChild(x); });
  zoomLayer.appendChild(book);
  stage.appendChild(zoomLayer);
  app.appendChild(stage);

  var prevArrow = button('prev', 'fc-arrow fc-arrow-prev');
  var nextArrow = button('next', 'fc-arrow fc-arrow-next');
  prevArrow.innerHTML = icon('prev');
  nextArrow.innerHTML = icon('next');
  app.appendChild(prevArrow);
  app.appendChild(nextArrow);

  // Thumbnails: a strip above the toolbar on desktop, a grid on phones.
  var thumbs = el('div', 'fc-thumbs');
  var thumbsView = el('div', 'fc-thumbs-view');
  var thumbsPrev = button('thumbsPrev', 'fc-thumbs-nav fc-thumbs-nav-prev');
  var thumbsNext = button('thumbsNext', 'fc-thumbs-nav fc-thumbs-nav-next');
  thumbsPrev.innerHTML = icon('prev');
  thumbsNext.innerHTML = icon('next');
  thumbs.appendChild(thumbsPrev);
  thumbs.appendChild(thumbsView);
  thumbs.appendChild(thumbsNext);
  app.appendChild(thumbs);

  // Desktop toolbar.
  var bar = el('div', 'fc-bar');
  var barL = el('div', 'fc-bar-l');
  var barR = el('div', 'fc-bar-r');
  var pageInput = el('input', 'fc-pagenum');
  pageInput.type = 'text';
  pageInput.inputMode = 'numeric';
  pageInput.autocomplete = 'off';
  pageInput.setAttribute('aria-label', T.page);
  var slider = el('div', 'fc-slider');
  slider.setAttribute('role', 'slider');
  slider.setAttribute('tabindex', '0');
  slider.setAttribute('aria-label', T.slider);
  slider.setAttribute('aria-valuemin', '1');
  slider.setAttribute('aria-valuemax', String(N));
  var track = el('div', 'fc-track');
  var knob = el('div', 'fc-knob');
  var tip = el('div', 'fc-tip', '<canvas></canvas><span></span>');
  slider.appendChild(track);
  slider.appendChild(knob);
  slider.appendChild(tip);
  ['home', 'zoom', 'thumbs', 'view'].forEach(function (a) { barL.appendChild(button(a)); });
  barL.appendChild(pageInput);
  ['play', 'sound', 'share', 'download', 'fullscreen'].forEach(function (a) { barR.appendChild(button(a)); });
  bar.appendChild(barL);
  bar.appendChild(slider);
  bar.appendChild(barR);
  app.appendChild(bar);

  // Phone bars.
  var ptop = el('div', 'fc-ptop');
  var ptopL = el('div', 'fc-ptop-l');
  var ptopR = el('div', 'fc-ptop-r');
  var pnum = el('span', 'fc-pnum');
  ptopL.appendChild(button('home', 'fc-pbtn'));
  ptopL.appendChild(pnum);
  ['share', 'download', 'fullscreen'].forEach(function (a) { ptopR.appendChild(button(a, 'fc-pbtn')); });
  ptop.appendChild(ptopL);
  ptop.appendChild(ptopR);
  var pbottom = el('div', 'fc-pbottom');
  ['thumbs', 'play', 'zoom', 'view', 'sound'].forEach(function (a) { pbottom.appendChild(button(a, 'fc-pbtn')); });
  app.appendChild(ptop);
  app.appendChild(pbottom);

  var loading = host.querySelector('.fc-loading') || el('div', 'fc-loading', '<div class="fc-spin"></div>');
  app.appendChild(loading);

  host.appendChild(app);

  // ----------------------------------------------------------- page cache

  // Pages are drawn by pdf.js into one canvas each, at the size they are shown. Memory is bounded:
  // at most CACHE_MAX page canvases exist (the spread, the turning sheet and a few neighbours), a
  // page left oversized by a zoom is drawn smaller again, and every canvas is emptied when dropped.
  var cache = {};
  var lru = [];
  var CACHE_MAX = 12;
  var MAX_SIDE = 2200; // px on the long side of a drawn page
  var queue = [], busy = false, sharpTimer = 0, dead = false;

  function lx(x) { return x - app.getBoundingClientRect().left; }
  function ly(y) { return y - app.getBoundingClientRect().top; }

  // The width, in device pixels, a page needs for the current layout and zoom.
  function wantWidth() {
    var dpr = Math.min(window.devicePixelRatio || 1, 2);
    var w = G.pw * dpr * Math.min(2, Math.max(1, zoom.s));
    return Math.max(64, Math.round(Math.min(w, MAX_SIDE * Math.min(1, AR))));
  }

  function draw(e) {
    if (!G.pw || dead) return;
    var want = wantWidth();
    if ((e.w >= want * 0.92 && e.w <= want * 1.3) || e.want === want) return;
    e.want = want;
    if (!e.drawing && queue.indexOf(e) === -1) queue.push(e);
    pump();
  }

  // One page at a time, those on screen first, so a turn in progress stays smooth.
  function pump() {
    if (busy || dead || !queue.length) return;
    var i = 0;
    for (var j = 0; j < queue.length; j++) {
      if (queue[j].isConnected) { i = j; break; }
    }
    var e = queue.splice(i, 1)[0];
    if (cache[e.n] !== e) { pump(); return; }
    busy = true;
    e.drawing = true;
    var want = e.want;
    var off = document.createElement('canvas');
    cfg.pdf.getPage(e.n).then(function (page) {
      var vp = page.getViewport({ scale: 1 });
      vp = page.getViewport({ scale: want / vp.width });
      off.width = Math.round(vp.width);
      off.height = Math.round(vp.height);
      return page.render({ canvasContext: off.getContext('2d', { alpha: false }), viewport: vp, background: '#ffffff' }).promise.then(function () {
        page.cleanup();
        if (dead || cache[e.n] !== e) return;
        e.canvas.width = off.width;
        e.canvas.height = off.height;
        e.canvas.getContext('2d').drawImage(off, 0, 0);
        e.canvas.classList.add('ok');
        e.w = want;
      });
    }).catch(function (err) {
      if (!dead) console.warn('Pursae Flipbook: page ' + e.n, err);
    }).then(function () {
      off.width = off.height = 0; // hand the pixels back now, not at the next collection
      busy = false;
      e.drawing = false;
      e.want = 0;
      e.resolve();
      if (cache[e.n] === e && e.w && e.isConnected) draw(e); // the book was resized meanwhile
      pump();
    });
  }

  // Draw the pages on screen again at the size they are shown now.
  function sharpen() {
    Object.keys(cache).forEach(function (n) {
      if (cache[n].isConnected) draw(cache[n]);
    });
  }

  function dropPage(k) {
    var e = cache[k];
    var q = queue.indexOf(e);
    if (q !== -1) queue.splice(q, 1);
    e.canvas.width = e.canvas.height = 0;
    e.resolve();
    delete cache[k];
  }

  function pageEl(n) {
    var e = cache[n];
    if (!e) {
      e = el('div', 'fc-page');
      e.n = n;
      e.w = 0;
      e.want = 0;
      e.canvas = el('canvas');
      e.canvas.setAttribute('role', 'img');
      e.canvas.setAttribute('aria-label', T.page + ' ' + n);
      e.appendChild(e.canvas);
      e.ready = new Promise(function (r) { e.resolve = r; });
      cache[n] = e;
    }
    draw(e);
    var i = lru.indexOf(n);
    if (i !== -1) lru.splice(i, 1);
    lru.push(n);
    for (var j = 0; lru.length > CACHE_MAX && j < lru.length; j++) {
      var k = lru[j];
      if (!cache[k].isConnected) {
        dropPage(k);
        lru.splice(j, 1);
        j--;
      }
    }
    return e;
  }

  // Thumbnails are drawn when they scroll into view and emptied when they scroll out, so a long
  // book holds only the ones on screen.
  var TH = 120;
  var thumbChain = Promise.resolve();

  function paintThumb(c, n) {
    c.n = n;
    thumbChain = thumbChain.then(function () {
      if (dead || c.n !== n || c.painted === n || !c.isConnected) return null;
      var off = document.createElement('canvas');
      return cfg.pdf.getPage(n).then(function (page) {
        var vp = page.getViewport({ scale: 1 });
        vp = page.getViewport({ scale: TH * Math.min(window.devicePixelRatio || 1, 1.5) / vp.height });
        off.width = Math.round(vp.width);
        off.height = Math.round(vp.height);
        return page.render({ canvasContext: off.getContext('2d', { alpha: false }), viewport: vp, background: '#ffffff' }).promise.then(function () {
          page.cleanup();
          if (dead || c.n !== n) return;
          c.width = off.width;
          c.height = off.height;
          c.getContext('2d').drawImage(off, 0, 0);
          c.painted = n;
        });
      }).then(function () { off.width = off.height = 0; }, function () { off.width = off.height = 0; });
    });
  }

  function clearThumb(c) {
    c.n = 0;
    c.painted = 0;
    c.width = c.height = 1;
  }

  var thumbIO = new IntersectionObserver(function (entries) {
    entries.forEach(function (en) {
      var c = en.target;
      if (en.isIntersecting) paintThumb(c, c.page);
      else if (c.painted || c.n) clearThumb(c);
    });
  }, { rootMargin: '300px' });

  function thumbEl(n) {
    var c = el('canvas');
    c.page = n;
    c.width = c.height = 1;
    c.style.aspectRatio = String(AR);
    thumbIO.observe(c);
    return c;
  }

  function put(holder, n) {
    var cur = holder.firstChild;
    if (n) {
      var e = pageEl(n);
      if (cur !== e) {
        holder.textContent = '';
        holder.appendChild(e);
      }
    } else if (cur) {
      holder.textContent = '';
    }
  }

  // --------------------------------------------------------------- spreads

  var double = true;
  var phone = false;
  // The reader's own choice of single or two-page view ('single', 'double'),
  // or null to follow the window. It only overrides the automatic choice
  // where the other mode makes sense (see canDouble in layout()).
  var forcedView = store('fc-view');
  if (forcedView !== 'single' && forcedView !== 'double') forcedView = null;
  var viewAuto = true, viewOffered = false;
  var cur = 0; // current spread index

  function count() { return double ? Math.floor(N / 2) + 1 : N; }
  function spread(k) {
    if (!double) return { l: 0, r: k + 1 };
    return { l: k === 0 ? 0 : 2 * k, r: 2 * k + 1 <= N ? 2 * k + 1 : 0 };
  }
  function spreadOf(p) { return double ? Math.floor(p / 2) : p - 1; }
  function firstPage(k) { var s = spread(k); return s.l || s.r; }
  function label(k) {
    var s = spread(k);
    return (s.l && s.r ? s.l + '-' + s.r : String(s.l || s.r)) + '/' + N;
  }

  // ---------------------------------------------------------------- layout

  var G = { W: 0, H: 0, pw: 0, ph: 0, bx: 0, by: 0, areaTop: 0, areaH: 0 };
  var thumbsOpen = false;

  function isPhoneUI(W, H) {
    var coarse = window.matchMedia && matchMedia('(pointer: coarse)').matches;
    return W < 520 || (coarse && Math.min(W, H) < 820);
  }

  function offset(k) {
    if (!double) return 0;
    var s = spread(k);
    if (!s.l && s.r) return -G.pw / 2;
    if (s.l && !s.r) return G.pw / 2;
    return 0;
  }

  function layout() {
    var W = app.clientWidth, H = app.clientHeight;
    if (!W || !H) return; // not on screen yet; the resize observer calls again
    var wasPhone = phone, wasDouble = double;
    var p = firstPage(cur);
    phone = isPhoneUI(W, H);
    app.classList.toggle('fc-is-phone', phone);
    app.classList.toggle('fc-narrow', W < 900);

    // A landscape window shows two-page spreads and a portrait one single
    // pages, whatever the shape of the pages themselves: a wide catalog on a
    // desktop screen should open like a magazine, not one sheet at a time.
    var autoDouble = N > 1 && W >= H;
    // Two pages side by side on a phone held upright would each be a
    // postage stamp, so there the toggle is not offered at all.
    viewOffered = N > 1 && (!phone || autoDouble);
    viewAuto = !viewOffered || !forcedView || (forcedView === 'double') === autoDouble;
    double = viewAuto ? autoDouble : forcedView === 'double';
    viewUI();

    // The book uses nearly the whole width; the round arrows float over the
    // outer page edges rather than reserving a column of their own.
    var top, bottom, side;
    if (phone) {
      top = ptop.offsetHeight || 40;
      bottom = pbottom.offsetHeight || 60;
      side = double ? 11 : 0;
    } else {
      top = GAP;
      bottom = BAR_BOTTOM + BAR_H + GAP + (thumbsOpen ? THUMBS_H : 0);
      side = double ? 21 : GAP;
    }
    var availW = Math.max(50, W - 2 * side);
    var availH = Math.max(50, H - top - bottom);

    var pw = double ? Math.min(availW / 2, availH * AR) : Math.min(availW, availH * AR);
    pw = Math.floor(pw);
    var ph = Math.round(pw / AR);
    var bw = double ? 2 * pw : pw;
    var bx = Math.round((W - bw) / 2);
    var by = phone
      ? Math.round(Math.min(Math.max((H - ph) / 2, top), H - bottom - ph))
      : Math.round(top + (availH - ph) / 2);

    G = { W: W, H: H, pw: pw, ph: ph, bx: bx, by: by, bw: bw, areaTop: top, areaH: availH };

    book.style.left = bx + 'px';
    book.style.top = by + 'px';
    book.style.width = bw + 'px';
    book.style.height = ph + 'px';
    app.style.setProperty('--fc-pw', pw + 'px');
    app.classList.toggle('fc-double', double);

    slotL.style.cssText = 'left:0;width:' + pw + 'px';
    slotR.style.cssText = 'left:' + (double ? pw : 0) + 'px;width:' + pw + 'px';
    flap.style.width = pw + 'px';
    slider.style.visibility = slider.clientWidth < 60 ? 'hidden' : '';

    // Arrows stay put when the thumbnail strip opens.
    var arrowTop = Math.round(top + (phone ? availH : availH + (thumbsOpen ? THUMBS_H : 0)) / 2 - 26);
    prevArrow.style.top = nextArrow.style.top = arrowTop + 'px';

    if (anim) finishNow();
    if (wasDouble !== double || wasPhone !== phone) {
      cur = Math.min(spreadOf(p), count() - 1);
      buildThumbs();
    }
    resetZoom(false);
    render();
  }

  // ---------------------------------------------------------- static view

  var bookOff = 0;
  function setOffset(x) {
    bookOff = x;
    book.style.transform = x ? 'translateX(' + x.toFixed(2) + 'px)' : '';
  }

  function render() {
    var s = spread(cur);
    put(slotL.holder, double ? s.l : 0);
    put(slotR.holder, s.r);
    slotL.classList.toggle('fc-empty', !double || !s.l);
    slotR.classList.toggle('fc-empty', !s.r);
    // A cover or back page on its own is a closed book: no binding shade.
    slotL.classList.toggle('fc-solo', double && !s.r);
    slotR.classList.toggle('fc-solo', double && !s.l);
    shadowFor(double ? s.l : 0, s.r);
    hideTurn();
    setOffset(offset(cur));
    edges(s);
    updateUI();
  }

  // Drop shadow under the pages lying flat.
  function shadowFor(l, r) {
    var x0 = double ? (l ? 0 : G.pw) : 0;
    var x1 = double ? (r ? 2 * G.pw : G.pw) : (r ? G.pw : 0);
    bookShadow.style.left = x0 + 'px';
    bookShadow.style.width = Math.max(0, x1 - x0) + 'px';
  }

  // Page stack thickness beside the open pages: fine sheet lines next to the
  // page, then the flat of the block, darkening at its outer side.
  function stack(pages, cover) {
    if (pages < 3) return [];
    if (cover) return [212, 223, 213, 207];
    var w = Math.min(11, Math.max(1, Math.round(16.875 * pages / N - 4.75)));
    var lines = w >= 10 ? 5 : w >= 6 ? 3 : 1;
    var tail = w >= 10 ? 3 : w >= 3 ? 1 : 0;
    var out = [209, 231, 209, 230, 208].slice(0, lines);
    while (out.length < w - tail) out.push(212);
    return out.concat([201, 205, 204].slice(3 - tail));
  }
  function stackCSS(shades, x) {
    var stops = shades.map(function (v, i) {
      return 'rgb(' + v + ',' + v + ',' + v + ') ' + i + 'px ' + (i + 1) + 'px';
    });
    return 'display:block;left:' + x + 'px;width:' + shades.length + 'px;background:linear-gradient(to right,' + stops.join(',') + ')';
  }
  function edges(s) {
    var left = double && s.l ? s.l - 1 : 0;
    var right = s.r ? N - s.r : 0;
    var closed = double && !s.l;
    var r = stack(right, closed);
    // The open right page catches a thin highlight along its edge.
    if (r.length && !closed) r.unshift(243);
    var l = stack(left, false).reverse();
    var lx = double ? (s.l ? 0 : G.pw) : 0;
    var rx = double ? (s.r ? 2 * G.pw : G.pw) : G.pw;
    edgeL.style.cssText = l.length ? stackCSS(l, lx - l.length) : 'display:none';
    edgeR.style.cssText = r.length ? stackCSS(r, rx) : 'display:none';
  }

  // ------------------------------------------------------------- page fold
  //
  // Geometry lives in "turn space": origin at the top of the spine, the
  // turning page on x in [0, pw], y in [0, ph]. A page turning backwards in
  // a two-page spread is the mirror image (s = -1). C is the corner being
  // lifted and P where that corner is now; the sheet folds along the
  // perpendicular bisector of C and P, so the folded part is the mirror image
  // of what lies beyond that line. A single page turning back is mirrored
  // too: it is held along its right edge and its left corner lifts.

  var DUR = 300;          // a full turn by click, key or wheel
  var LIFT = 0.0375;      // how far the corner rises mid-turn, of page height
  var FOLLOW = 67;        // ms: how quickly a dragged or peeled corner catches up
  var anim = null;
  var pending = null;

  function easeInOut(t) { return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; }
  function easeOut(t) { return Math.sin(t * Math.PI / 2); }
  function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }

  // Clip a polygon to one side of the line through M with normal n.
  function halfPlane(poly, M, n, side) {
    var out = [];
    for (var i = 0; i < poly.length; i++) {
      var a = poly[i], b = poly[(i + 1) % poly.length];
      var da = side * ((a[0] - M[0]) * n[0] + (a[1] - M[1]) * n[1]);
      var db = side * ((b[0] - M[0]) * n[0] + (b[1] - M[1]) * n[1]);
      if (da >= 0) out.push(a);
      if ((da >= 0) !== (db >= 0)) {
        var k = da / (da - db);
        out.push([a[0] + (b[0] - a[0]) * k, a[1] + (b[1] - a[1]) * k]);
      }
    }
    return out;
  }

  function polygon(pts) {
    return 'polygon(' + pts.map(function (p) { return p[0].toFixed(2) + 'px ' + p[1].toFixed(2) + 'px'; }).join(',') + ')';
  }

  function matrix(o, u, v) {
    return 'matrix(' + [u[0], u[1], v[0], v[1], o[0], o[1]].map(function (x) { return x.toFixed(4); }).join(',') + ')';
  }

  // Keep the corner attached to the spine: it can't move further than a page
  // width from the spine end on its own edge, nor further than the page
  // diagonal from the other end.
  function constrain(P, top) {
    var pw = G.pw, ph = G.ph;
    var near = [0, top ? 0 : ph], far = [0, top ? ph : 0];
    var dx = P[0] - near[0], dy = P[1] - near[1], d = Math.hypot(dx, dy);
    if (d > pw) P = [near[0] + dx * pw / d, near[1] + dy * pw / d];
    var diag = Math.hypot(pw, ph);
    dx = P[0] - far[0]; dy = P[1] - far[1]; d = Math.hypot(dx, dy);
    if (d > diag) P = [far[0] + dx * diag / d, far[1] + dy * diag / d];
    return P;
  }

  // Where the corner is along a turn by click: it sweeps across in a sine
  // ease while rising a little and settling back down.
  // Where the fixed edge of a turn with mirror s is, in book coordinates.
  function spineAt(s) { return double || s < 0 ? G.pw : 0; }

  function cornerAt(f, u) {
    var lift = LIFT * G.ph * Math.sin(Math.PI * u);
    return [G.pw * Math.cos(Math.PI * u), f.top ? lift : G.ph - lift];
  }

  function hideTurn() {
    turn.style.display = 'none';
    put(tFront.holder, 0);
    put(flap.holder, 0);
    flap.mirror.style.backgroundImage = '';
  }

  // Set up the pages for a turn from spread cur to spread `to`.
  function prepare(to, top) {
    var dir = to > cur ? 1 : -1;
    var A = spread(cur), B = spread(to);
    var f = {
      dir: dir, to: to, top: !!top, raf: 0,
      s: dir < 0 ? -1 : 1,
      offFrom: offset(cur), offTo: offset(to)
    };
    var pw = G.pw, ph = G.ph;
    var C = [pw, f.top ? 0 : ph];
    var gone = [-pw, C[1]];
    f.Pdone = gone;
    f.Pundo = C;
    f.P = f.Pundo.slice();

    var front, under, backPage = 0, mirrorPage = 0;
    slotL.classList.remove('fc-solo');
    slotR.classList.remove('fc-solo');
    if (double) {
      if (dir > 0) {
        put(slotL.holder, A.l); put(slotR.holder, B.r);
        front = A.r; under = B.r; backPage = B.l;
        slotL.classList.toggle('fc-empty', !A.l); slotR.classList.toggle('fc-empty', !B.r);
        shadowFor(A.l, B.r);
      } else {
        put(slotL.holder, B.l); put(slotR.holder, A.r);
        front = A.l; under = B.l; backPage = B.r;
        slotL.classList.toggle('fc-empty', !B.l); slotR.classList.toggle('fc-empty', !A.r);
        shadowFor(B.l, A.r);
      }
    } else {
      // Single page: the current page lifts off the one it uncovers. The
      // back of the sheet shows the page being turned to, as it does in a
      // two-page spread, rather than the sheet's own print seen through.
      front = A.r;
      under = B.r;
      backPage = under;
      put(slotR.holder, under);
      slotR.classList.toggle('fc-empty', !under);
      shadowFor(0, under);
    }
    f.under = under;
    put(tFront.holder, front);
    put(flap.holder, backPage);
    flap.mirror.style.backgroundImage = '';
    tFront.className = 'fc-tfront ' + (f.s > 0 || !double ? 'fc-pr' : 'fc-pl');
    flap.className = 'fc-flap ' + (mirrorPage ? 'fc-back-mirror' : !double ? '' : f.s > 0 ? 'fc-pl' : 'fc-pr');
    var x0 = spineAt(f.s) - (f.s > 0 ? 0 : pw);
    tFront.style.left = uClip.style.left = x0 + 'px';
    tFront.style.width = uClip.style.width = pw + 'px';
    uShade.style.display = under ? '' : 'none';
    turn.style.display = 'block';
    drawFold(f);
    return f;
  }

  // Lay out the sheet for the corner at f.P.
  function drawFold(f) {
    var pw = G.pw, ph = G.ph, s = f.s;
    var spineX = spineAt(s);
    var C = [pw, f.top ? 0 : ph];
    var P = f.P;

    // The book slides between a closed cover and an open spread as it turns.
    var prog = clamp((pw - P[0]) / (2 * pw), 0, 1);
    var u = Math.acos(1 - 2 * prog) / Math.PI;
    setOffset(f.offFrom + (f.offTo - f.offFrom) * easeInOut(u));

    var dx = C[0] - P[0], dy = C[1] - P[1];
    var len = Math.hypot(dx, dy);
    if (len < 0.5) {
      tFront.style.clipPath = '';
      flap.style.visibility = 'hidden';
      uShade.style.opacity = '0';
      return;
    }
    var n = [dx / len, dy / len];                      // points at the corner
    var t = [-n[1], n[0]];                             // along the fold
    var M = [(C[0] + P[0]) / 2, (C[1] + P[1]) / 2];    // a point on the fold
    var rect = [[0, 0], [pw, 0], [pw, ph], [0, ph]];
    var flat = halfPlane(rect, M, n, -1);
    var folded = halfPlane(rect, M, n, 1);

    // Turn space to the turning page's own box, and to book coordinates.
    var local = function (X) { return [s > 0 ? X[0] : pw - X[0], X[1]]; };
    var toBook = function (X) { return [spineX + s * X[0], X[1]]; };
    var vec = function (v) { return [s * v[0], v[1]]; };
    // A point of the back of the sheet, in its own box, to the point of the
    // front it backs (the sheet is mirrored through the spine).
    var backToFront = function (X) { return [s > 0 ? pw - X[0] : X[0], X[1]]; };
    var backVec = function (v) { return [s > 0 ? -v[0] : v[0], v[1]]; };
    var reflect = function (X) {
      var d = 2 * ((X[0] - M[0]) * n[0] + (X[1] - M[1]) * n[1]);
      return [X[0] - d * n[0], X[1] - d * n[1]];
    };

    tFront.style.clipPath = flat.length > 2 ? polygon(flat.map(local)) : 'polygon(0 0,0 0,0 0)';

    if (folded.length < 3) {
      flap.style.visibility = 'hidden';
      uShade.style.opacity = '0';
      return;
    }
    flap.style.visibility = '';
    var o = toBook(reflect(backToFront([0, 0])));
    var ex = toBook(reflect(backToFront([1, 0])));
    var ey = toBook(reflect(backToFront([0, 1])));
    flap.style.transform = matrix(o, [ex[0] - o[0], ex[1] - o[1]], [ey[0] - o[0], ey[1] - o[1]]);
    flap.style.clipPath = polygon(folded.map(backToFront));

    // Light and shade grow with how much of the sheet is folded over, and
    // fade as it comes to rest flat against the other side.
    var depth = len / 2;
    var L = 2 * (pw + ph);
    var Q0 = [M[0] - t[0] * L, M[1] - t[1] * L];
    var rest = clamp(M[0] / (0.081 * pw), 0, 1);
    var bo = backToFront(Q0);
    var bm = matrix(bo, backVec(n), backVec(t));
    var sheen = Math.min(1, depth / (0.42 * pw));
    flap.band.style.width = (0.325 * pw * sheen).toFixed(2) + 'px';
    flap.band.style.height = (2 * L) + 'px';
    flap.band.style.transform = bm;
    flap.band.style.opacity = (sheen * Math.sqrt(rest)).toFixed(3);
    flap.crease.style.width = (0.0227 * pw).toFixed(2) + 'px';
    flap.crease.style.height = (2 * L) + 'px';
    flap.crease.style.transform = bm;
    flap.crease.style.opacity = (Math.min(1, depth / (0.13 * pw)) * Math.sqrt(rest)).toFixed(3);

    if (f.under) {
      // The shadow narrows as the fold leans over: it is cast across the
      // page, not along the fold.
      var across = depth * n[0] * n[0];
      var lo = local(Q0);
      uShade.style.width = (Math.max(0, 0.322 * across - 0.0127 * pw) * Math.sqrt(rest)).toFixed(2) + 'px';
      uShade.style.height = (2 * L) + 'px';
      uShade.style.transform = matrix(lo, vec(n), vec(t));
      uShade.style.opacity = (Math.min(1, Math.pow(depth / (0.146 * pw), 1.5)) * rest).toFixed(3);
    }
  }

  // Drive f.P every animation frame: along a timed path (tween), or chasing
  // a target with a little lag (follow).
  function run(f) {
    if (f.raf) return;
    var last = 0;
    function step(now) {
      f.raf = 0;
      if (anim !== f) return;
      var dt = last ? Math.min(64, now - last) : 16;
      last = now;
      if (f.tween) {
        var tw = f.tween;
        if (!tw.t0) tw.t0 = now;
        var k = tw.dur > 0 ? Math.min(1, (now - tw.t0) / tw.dur) : 1;
        f.P = tw.at(k);
        drawFold(f);
        if (k >= 1) { endTween(f); return; }
      } else if (f.target) {
        var a = 1 - Math.exp(-dt / FOLLOW);
        f.P = [f.P[0] + (f.target[0] - f.P[0]) * a, f.P[1] + (f.target[1] - f.P[1]) * a];
        var gap = Math.hypot(f.target[0] - f.P[0], f.target[1] - f.P[1]);
        if (gap < 0.5) f.P = f.target.slice();
        drawFold(f);
        if (gap < 0.5 && f.settleOnArrive) { settle(f, f.settleOnArrive === 'done'); return; }
        if (gap < 0.5 && !f.drag) return; // at rest: wait for the next move
      }
      f.raf = requestAnimationFrame(step);
    }
    f.raf = requestAnimationFrame(step);
  }

  function tween(f, dur, at, completed) {
    f.target = null;
    f.tween = { dur: dur, at: at, t0: 0, completed: completed };
    cancelAnimationFrame(f.raf);
    f.raf = 0;
    clearTimeout(f.safety);
    // Background tabs may not run animation frames at all: finish anyway.
    f.safety = setTimeout(function () { endTween(f); }, dur + 400);
    run(f);
  }

  function endTween(f) {
    if (anim !== f || !f.tween) return;
    var tw = f.tween;
    f.P = tw.at(1);
    settle(f, tw.completed);
  }

  // Let the corner chase a point (a dragged or hovering pointer).
  function follow(f, target, then) {
    f.tween = null;
    f.target = constrain(target, f.top);
    f.settleOnArrive = then || null;
    clearTimeout(f.safety);
    if (then) f.safety = setTimeout(function () { if (anim === f) settle(f, then === 'done'); }, 1200);
    run(f);
  }

  // Finish a turn from wherever the corner is now.
  function complete(f) {
    var P0 = f.P.slice(), P1 = f.Pdone;
    var dur = Math.max(60, DUR * Math.abs(P1[0] - P0[0]) / (2 * G.pw));
    f.released = true;
    f.landing = f.to;
    tween(f, dur, function (k) {
      var e = easeOut(k);
      return [P0[0] + (P1[0] - P0[0]) * e, P0[1] + (P1[1] - P0[1]) * e];
    }, true);
  }

  // Put the sheet back down where it came from.
  function undo(f) {
    f.released = true;
    f.landing = cur;
    f.drag = false;
    f.peel = false;
    follow(f, f.Pundo, 'undo');
  }

  function settle(f, completed) {
    if (anim !== f) return;
    cancelAnimationFrame(f.raf);
    clearTimeout(f.safety);
    anim = null;
    var changed = completed && f.to !== cur;
    if (completed) cur = f.to;
    render();
    if (changed) afterChange();
    if (pending !== null) {
      var k = pending;
      pending = null;
      flipTo(k, true);
    }
  }

  function finishNow() {
    if (!anim) return;
    var f = anim;
    pending = null;
    if (f.peel) { settle(f, false); return; }
    settle(f, f.drag ? !!f.released && f.landing === f.to : true);
  }

  function flipTo(k, quick) {
    k = Math.max(0, Math.min(count() - 1, k));
    if (anim && anim.peel && !anim.drag) {
      // A corner lifted under the mouse: turn it from where it is.
      if (k === anim.to) {
        anim.peel = false;
        flipSound();
        complete(anim);
        return;
      }
      settle(anim, false);
    }
    if (anim) {
      // Queue behind a running turn; a page still under the finger wins.
      if (!anim.drag || anim.released) pending = k === landing() ? null : k;
      return;
    }
    if (k === cur) return;
    if (zoom.s > 1) resetZoom(true);
    if (reduceMotion) {
      cur = k;
      render();
      afterChange();
      return;
    }
    var f = prepare(k, false);
    anim = f;
    flipSound();
    tween(f, quick ? DUR * 0.6 : DUR, function (u) {
      return cornerAt(f, u);
    }, true);
    f.landing = k;
  }

  // Spread the running turn will end on.
  function landing() {
    if (!anim) return cur;
    if (anim.peel) return cur;
    if (anim.drag) return anim.released ? anim.landing : cur;
    return anim.landing !== undefined ? anim.landing : anim.to;
  }
  function target() { return pending !== null ? pending : landing(); }
  function go(d) { flipTo(target() + d); }
  function goPage(p) { flipTo(spreadOf(Math.max(1, Math.min(N, p)))); }

  // ------------------------------------------------------------ after flip

  var lastHash = '';
  function afterChange() {
    var p = firstPage(cur);
    var h = '#p=' + p;
    if (cfg.full && h !== lastHash) {
      lastHash = h;
      try { history.replaceState(null, '', h); } catch (_) { location.hash = h; }
    }
    preload();
    if (autoplay && cur >= count() - 1) setAutoplay(false);
  }

  function preload() {
    var s = spread(cur);
    var a = s.l || s.r, b = s.r || s.l;
    for (var i = 1; i <= 4; i++) {
      if (b + i <= N) pageEl(b + i);
      if (a - i >= 1) pageEl(a - i);
    }
    // keep the visible ones most recently used
    if (s.l) pageEl(s.l);
    if (s.r) pageEl(s.r);
  }

  function updateUI() {
    var c = count();
    var txt = label(cur);
    if (document.activeElement !== pageInput) pageInput.value = txt;
    pnum.textContent = txt;
    var frac = c > 1 ? cur / (c - 1) : 0;
    knob.style.left = (6 + frac * Math.max(0, slider.clientWidth - 12)) + 'px';
    slider.setAttribute('aria-valuenow', String(firstPage(cur)));
    slider.setAttribute('aria-valuetext', txt);
    prevArrow.classList.toggle('fc-off', cur <= 0);
    nextArrow.classList.toggle('fc-off', cur >= c - 1);
    var items = thumbsView.children;
    for (var i = 0; i < items.length; i++) {
      var on = +items[i].getAttribute('data-k') === cur;
      items[i].classList.toggle('fc-cur', on);
      if (on) items[i].setAttribute('aria-current', 'page'); else items[i].removeAttribute('aria-current');
    }
    if (thumbsOpen) scrollThumb(false);
  }

  // ------------------------------------------------------------ thumbnails

  function buildThumbs() {
    thumbIO.disconnect();
    thumbsView.textContent = '';
    var c = count();
    for (var k = 0; k < c; k++) {
      var s = spread(k);
      var b = el('button', 'fc-thumb');
      b.type = 'button';
      b.setAttribute('data-k', String(k));
      b.setAttribute('aria-label', T.page + ' ' + label(k).split('/')[0]);
      var imgs = el('span', 'fc-thumb-imgs');
      [s.l, s.r].forEach(function (n) {
        if (!n) return;
        imgs.appendChild(thumbEl(n));
      });
      b.appendChild(imgs);
      b.appendChild(el('span', 'fc-thumb-label', label(k).split('/')[0]));
      thumbsView.appendChild(b);
    }
  }

  function scrollThumb(smooth) {
    var item = thumbsView.querySelector('.fc-cur');
    if (!item) return;
    if (phone) {
      var y = item.offsetTop - (thumbsView.clientHeight - item.offsetHeight) / 2;
      thumbsView.scrollTo({ top: y, behavior: smooth ? 'smooth' : 'auto' });
    } else {
      var x = item.offsetLeft - (thumbsView.clientWidth - item.offsetWidth) / 2;
      thumbsView.scrollTo({ left: x, behavior: smooth ? 'smooth' : 'auto' });
    }
  }

  function setThumbs(open) {
    thumbsOpen = open;
    thumbs.classList.toggle('fc-open', open);
    setPressed('thumbs', open);
    layout();
    if (open) scrollThumb(false);
  }

  thumbsView.addEventListener('click', function (e) {
    var b = e.target.closest('.fc-thumb');
    if (!b) return;
    flipTo(+b.getAttribute('data-k'));
    if (phone) setThumbs(false);
  });
  thumbsView.addEventListener('wheel', function (e) {
    if (phone || Math.abs(e.deltaY) <= Math.abs(e.deltaX)) return;
    thumbsView.scrollLeft += e.deltaY;
    e.preventDefault();
  }, { passive: false });

  // ------------------------------------------------------------------ zoom

  var zoom = { s: 1, x: 0, y: 0 };

  function visibleRect() {
    var s = spread(cur);
    var x0 = G.bx + offset(cur), w = G.pw;
    if (double) {
      if (s.l && s.r) w = 2 * G.pw;
      else if (s.r) x0 += G.pw;
    }
    return { x: x0, y: G.by, w: w, h: G.ph };
  }

  function clampZoom() {
    if (zoom.s <= 1) { zoom.s = 1; zoom.x = 0; zoom.y = 0; return; }
    var b = visibleRect(), s = zoom.s, m = 20;
    var lo = Math.min(-b.x * s + m, G.W - (b.x + b.w) * s - m);
    var hi = Math.max(-b.x * s + m, G.W - (b.x + b.w) * s - m);
    if (b.w * s <= G.W) { lo = -b.x * s; hi = G.W - (b.x + b.w) * s; }
    zoom.x = Math.max(lo, Math.min(hi, zoom.x));
    lo = Math.min(-b.y * s + m, G.H - (b.y + b.h) * s - m);
    hi = Math.max(-b.y * s + m, G.H - (b.y + b.h) * s - m);
    if (b.h * s <= G.H) { lo = -b.y * s; hi = G.H - (b.y + b.h) * s; }
    zoom.y = Math.max(lo, Math.min(hi, zoom.y));
  }

  function applyZoom(animated) {
    clampZoom();
    zoomLayer.classList.toggle('fc-anim', !!animated);
    zoomLayer.style.transform = zoom.s > 1
      ? 'translate(' + zoom.x.toFixed(1) + 'px,' + zoom.y.toFixed(1) + 'px) scale(' + zoom.s.toFixed(4) + ')'
      : '';
    app.classList.toggle('fc-zoomed', zoom.s > 1);
    setPressed('zoom', zoom.s > 1);
    clearTimeout(sharpTimer);
    sharpTimer = setTimeout(sharpen, 180);
    app.querySelectorAll('[data-act="zoom"]').forEach(function (b) {
      b.innerHTML = icon(zoom.s > 1 ? 'zoomOut' : 'zoomIn');
      b.setAttribute('aria-label', zoom.s > 1 ? T.zoomOut : T.zoomIn);
      b.title = b.getAttribute('aria-label');
    });
  }

  function zoomAt(px, py, s2, animated) {
    s2 = Math.max(1, Math.min(4, s2));
    zoom.x = px - (px - zoom.x) * (s2 / zoom.s);
    zoom.y = py - (py - zoom.y) * (s2 / zoom.s);
    zoom.s = s2;
    applyZoom(animated);
  }

  function resetZoom(animated) {
    zoom.s = 1; zoom.x = 0; zoom.y = 0;
    applyZoom(animated);
  }

  function toggleZoom(px, py) {
    if (zoom.s > 1) { resetZoom(true); return; }
    if (px === undefined) {
      var b = visibleRect();
      px = b.x + b.w / 2;
      py = b.y + b.h / 2;
    }
    zoomAt(px, py, 2, true);
  }

  // ---------------------------------------------------------- pointer input

  var pointers = {};
  var gesture = null;
  var lastTap = null;
  var tapTimer = 0;
  var CORNER = 0.2;      // size of the hot corner that lifts under the mouse, of page width

  function pcount() { return Object.keys(pointers).length; }

  // A point on screen in the turn space of a turn with mirror s.
  function toTurn(x, y, s) {
    var r = book.getBoundingClientRect();
    return [s * (x - r.left - spineAt(s)), y - r.top];
  }

  function inCorner(q) {
    var Z = Math.max(24, CORNER * G.pw);
    if (q[0] < G.pw - Z || q[0] > G.pw + 6) return 0;
    if (q[1] >= -6 && q[1] <= Z) return 'top';
    if (q[1] <= G.ph + 6 && q[1] >= G.ph - Z) return 'bottom';
    return 0;
  }

  // The corner under the mouse, if it belongs to a page that can turn.
  function hotCorner(x, y) {
    if (zoom.s > 1) return null;
    var dirs = [];
    if (cur < count() - 1) dirs.push(1);
    if (cur > 0) dirs.push(-1);
    for (var i = 0; i < dirs.length; i++) {
      var s = dirs[i] < 0 ? -1 : 1;
      var q = toTurn(x, y, s);
      var c = inCorner(q);
      if (c) return { dir: dirs[i], top: c === 'top', P: q };
    }
    return null;
  }

  // Lift the corner under the mouse, and lay it back down when it leaves.
  function peel(x, y) {
    var f = anim;
    if (f && !f.peel) return;
    var hit = hotCorner(x, y);
    if (f && hit && (hit.dir !== f.dir || hit.top !== f.top)) {
      settle(f, false);
      f = null;
    }
    if (hit) {
      if (!f) {
        f = prepare(cur + hit.dir, hit.top);
        f.peel = true;
        anim = f;
      }
      follow(f, toTurn(x, y, f.s));
    } else if (f) {
      follow(f, f.Pundo, 'undo');
    }
  }

  function startPinch() {
    var ids = Object.keys(pointers);
    var a = pointers[ids[0]], b = pointers[ids[1]];
    if (gesture && gesture.flip && anim && anim.drag) undo(anim);
    gesture = {
      pinch: true,
      d0: Math.hypot(a.x - b.x, a.y - b.y) || 1,
      mx: lx((a.x + b.x) / 2), my: ly((a.y + b.y) / 2),
      s0: zoom.s, x0: zoom.x, y0: zoom.y
    };
  }

  // Pick up the page under the pointer once a drag heads for the spine.
  // A single page is pulled whichever way the drag goes: leftwards by its
  // right edge, rightwards by its left edge.
  function startDrag(g, dx) {
    var dir;
    if (double) {
      var r = book.getBoundingClientRect();
      dir = g.x0 >= r.left + G.pw ? 1 : -1;
      if ((dir > 0) !== (dx < 0)) return;
    } else {
      dir = dx < 0 ? 1 : -1;
    }
    var to = cur + dir;
    if (to < 0 || to >= count()) return;
    var s = dir < 0 ? -1 : 1;
    var q = toTurn(g.x0, g.y0, s);
    var c = inCorner(q);
    g.gy = clamp(q[1], 0, G.ph);
    g.corner = !!c;
    var top = c ? c === 'top' : g.gy < 0.25 * G.ph;
    var f = prepare(to, top);
    f.drag = true;
    anim = f;
    g.flip = true;
    flipSound();
  }

  // Move the sheet with the pointer. A corner picked up follows the pointer
  // itself; a page picked up elsewhere follows with the point of its outer
  // edge level with where it was grabbed.
  function dragTo(g, x, y) {
    var f = anim, pw = G.pw, ph = G.ph;
    var target;
    var M = toTurn(x, y, f.s);
    if (g.corner) {
      target = M;
    } else {
      // Fold along the line that takes the grabbed edge point onto the
      // pointer, leaning a touch so the bottom corner leads.
      var E = [pw, g.gy];
      var Mb = [M[0], M[1] - 0.0067 * ph];
      var ex = E[0] - Mb[0], ey = E[1] - Mb[1], d = Math.hypot(ex, ey);
      if (d < 1) {
        target = f.Pundo.slice();
      } else {
        var n = [ex / d, ey / d], mid = [(E[0] + Mb[0]) / 2, (E[1] + Mb[1]) / 2];
        var C = [pw, f.top ? 0 : ph];
        var k = 2 * ((C[0] - mid[0]) * n[0] + (C[1] - mid[1]) * n[1]);
        target = [C[0] - k * n[0], C[1] - k * n[1]];
      }
    }
    follow(f, target);
  }

  stage.addEventListener('pointerdown', function (e) {
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    pointers[e.pointerId] = { x: e.clientX, y: e.clientY };
    try { stage.setPointerCapture(e.pointerId); } catch (_) { /* ignore */ }
    if (pcount() === 2) { startPinch(); return; }
    if (pcount() > 2) return;
    gesture = {
      x0: e.clientX, y0: e.clientY, t0: performance.now(), lx: e.clientX, vx: 0,
      type: e.pointerType, pan: zoom.s > 1, zx: zoom.x, zy: zoom.y, moved: false, flip: false
    };
    if (gesture.pan) {
      app.classList.add('fc-grabbing');
    } else if (anim && anim.peel) {
      // A corner already lifted under the mouse is picked up as it is.
      anim.peel = false;
      anim.drag = true;
      gesture.flip = true;
      gesture.corner = true;
    }
  });

  stage.addEventListener('pointermove', function (e) {
    if (!pointers[e.pointerId]) {
      if (e.pointerType === 'mouse') {
        hoverCursor(e);
        peel(e.clientX, e.clientY);
      }
      return;
    }
    pointers[e.pointerId] = { x: e.clientX, y: e.clientY };
    var g = gesture;
    if (!g) return;
    if (g.pinch) {
      if (pcount() < 2) return;
      var ids = Object.keys(pointers);
      var a = pointers[ids[0]], b = pointers[ids[1]];
      var d = Math.hypot(a.x - b.x, a.y - b.y);
      var mx = lx((a.x + b.x) / 2), my = ly((a.y + b.y) / 2);
      var s2 = Math.max(1, Math.min(4, g.s0 * d / g.d0));
      zoom.s = s2;
      zoom.x = mx - (g.mx - g.x0) * (s2 / g.s0);
      zoom.y = my - (g.my - g.y0) * (s2 / g.s0);
      applyZoom(false);
      return;
    }
    var dx = e.clientX - g.x0, dy = e.clientY - g.y0;
    if (!g.moved && Math.hypot(dx, dy) > 8) g.moved = true;
    if (g.pan) {
      zoom.x = g.zx + dx;
      zoom.y = g.zy + dy;
      applyZoom(false);
      return;
    }
    // Which way the pointer was last heading decides how a drag ends.
    if (e.clientX !== g.lx) g.vx = 0.5 * g.vx + 0.5 * (e.clientX - g.lx);
    g.lx = e.clientX;
    if (!g.flip && !anim && Math.abs(dx) > (g.type === 'mouse' ? 24 : 10) && Math.abs(dx) > Math.abs(dy)) {
      startDrag(g, dx);
    }
    if (g.flip && anim && anim.drag) dragTo(g, e.clientX, e.clientY);
  });

  stage.addEventListener('pointerleave', function (e) {
    if (e.pointerType === 'mouse' && anim && anim.peel) follow(anim, anim.Pundo, 'undo');
  });

  function endPointer(e) {
    if (!pointers[e.pointerId]) return;
    delete pointers[e.pointerId];
    var g = gesture;
    app.classList.remove('fc-grabbing');
    if (!g) return;
    if (g.pinch) {
      if (pcount() === 0) {
        gesture = null;
        if (zoom.s < 1.05) resetZoom(true);
      }
      return;
    }
    gesture = null;
    if (g.flip && anim && anim.drag) {
      var f = anim;
      f.drag = false;
      if (e.type === 'pointercancel') { undo(f); return; }
      // A lifted corner that is clicked turns over.
      if (!g.moved) { complete(f); return; }
      var heading = Math.abs(g.vx) >= 0.5 ? (g.vx * f.s < 0) : null;
      var past = f.P[0] < 0;
      if (heading === true || (heading === null && past)) { complete(f); return; }
      // Let go near the corner it came from: keep it lifted under the mouse.
      if (g.type === 'mouse') {
        var hit = hotCorner(e.clientX, e.clientY);
        if (hit && hit.dir === f.dir && hit.top === f.top) {
          f.peel = true;
          follow(f, toTurn(e.clientX, e.clientY, f.s));
          return;
        }
      }
      undo(f);
      return;
    }
    if (e.type === 'pointercancel') return;
    if (!g.moved && performance.now() - g.t0 < 500 && !g.pan) tap(e.clientX, e.clientY, e.pointerType);
  }
  stage.addEventListener('pointerup', endPointer);
  stage.addEventListener('pointercancel', endPointer);

  function sideOf(x, y) {
    x = lx(x);
    y = ly(y);
    var b = visibleRect();
    if (x < b.x || x > b.x + b.w || y < b.y || y > b.y + b.h) return 0;
    var s = spread(cur);
    if (double && s.l && s.r) return x < b.x + b.w / 2 ? -1 : 1;
    if (double) return s.r ? 1 : -1;
    return x < b.x + b.w / 2 ? -1 : 1;
  }

  function hoverCursor(e) {
    if (zoom.s > 1) return;
    var side = sideOf(e.clientX, e.clientY);
    var ok = (side > 0 && cur < count() - 1) || (side < 0 && cur > 0);
    stage.style.cursor = ok ? 'pointer' : '';
  }

  function tap(x, y, type) {
    if (type === 'mouse') {
      if (zoom.s > 1) return;
      var side = sideOf(x, y);
      if (side) go(side);
      return;
    }
    // Touch: wait a moment to tell a single tap from a double tap (zoom).
    var now = performance.now();
    if (lastTap && now - lastTap.t < 300 && Math.hypot(x - lastTap.x, y - lastTap.y) < 40) {
      clearTimeout(tapTimer);
      lastTap = null;
      toggleZoom(lx(x), ly(y));
      return;
    }
    lastTap = { t: now, x: x, y: y };
    clearTimeout(tapTimer);
    tapTimer = setTimeout(function () {
      lastTap = null;
      if (zoom.s > 1) return;
      var side = sideOf(x, y);
      if (side) go(side);
    }, 300);
  }

  stage.addEventListener('dblclick', function (e) {
    if (e.pointerType && e.pointerType !== 'mouse') return;
    e.preventDefault();
  });

  // Mouse wheel: flip once per wheel gesture; pan or zoom when zoomed.
  var wheelLock = false, wheelTimer = 0;
  stage.addEventListener('wheel', function (e) {
    // A book placed in a post leaves the wheel to the page, unless it is zoomed or full screen.
    if (!cfg.full && !fsElement() && !e.ctrlKey && zoom.s <= 1) return;
    e.preventDefault();
    if (e.ctrlKey) {
      zoomAt(lx(e.clientX), ly(e.clientY), zoom.s * Math.exp(-e.deltaY * 0.01), false);
      return;
    }
    if (zoom.s > 1) {
      zoom.x -= e.deltaX;
      zoom.y -= e.deltaY;
      applyZoom(false);
      return;
    }
    clearTimeout(wheelTimer);
    wheelTimer = setTimeout(function () { wheelLock = false; }, 220);
    if (wheelLock) return;
    var d = Math.abs(e.deltaY) >= Math.abs(e.deltaX) ? e.deltaY : e.deltaX;
    if (Math.abs(d) < 4) return;
    wheelLock = true;
    go(d > 0 ? 1 : -1);
  }, { passive: false });

  // Safari pinch on trackpads.
  ['gesturestart', 'gesturechange'].forEach(function (t) {
    app.addEventListener(t, function (e) { e.preventDefault(); }, { passive: false });
  });

  // ---------------------------------------------------------------- slider

  var sliding = null;
  function sliderSpread(clientX) {
    var r = slider.getBoundingClientRect();
    var frac = Math.max(0, Math.min(1, (clientX - r.left - 6) / Math.max(1, r.width - 12)));
    return Math.round(frac * (count() - 1));
  }
  function showTip(clientX, k) {
    var r = slider.getBoundingClientRect();
    var s = spread(k);
    var n = s.r || s.l;
    var tc = tip.querySelector('canvas');
    tc.style.aspectRatio = String(AR);
    if (tc.n !== (s.l || s.r)) paintThumb(tc, s.l || s.r);
    tip.querySelector('span').textContent = label(k);
    tip.style.left = Math.max(0, Math.min(r.width, clientX - r.left)) + 'px';
    tip.classList.add('fc-show');
    return n;
  }
  slider.addEventListener('pointerdown', function (e) {
    if (e.button !== 0) return;
    sliding = { id: e.pointerId };
    slider.setPointerCapture(e.pointerId);
    var k = sliderSpread(e.clientX);
    sliding.k = k;
    showTip(e.clientX, k);
    var c = count();
    knob.style.left = (6 + (c > 1 ? k / (c - 1) : 0) * (slider.clientWidth - 12)) + 'px';
  });
  slider.addEventListener('pointermove', function (e) {
    var k = sliderSpread(e.clientX);
    if (sliding && sliding.id === e.pointerId) {
      sliding.k = k;
      var c = count();
      knob.style.left = (6 + (c > 1 ? k / (c - 1) : 0) * (slider.clientWidth - 12)) + 'px';
    }
    showTip(e.clientX, k);
  });
  slider.addEventListener('pointerup', function (e) {
    if (!sliding || sliding.id !== e.pointerId) return;
    var k = sliding.k;
    sliding = null;
    tip.classList.remove('fc-show');
    if (k !== cur) flipTo(k); else updateUI();
  });
  slider.addEventListener('pointerleave', function () { if (!sliding) tip.classList.remove('fc-show'); });
  slider.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowRight' || e.key === 'ArrowUp') { go(1); e.preventDefault(); e.stopPropagation(); }
    if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') { go(-1); e.preventDefault(); e.stopPropagation(); }
  });

  // ----------------------------------------------------------- page input

  pageInput.addEventListener('focus', function () {
    pageInput.value = String(firstPage(cur));
    setTimeout(function () { pageInput.select(); }, 0);
  });
  pageInput.addEventListener('keydown', function (e) {
    e.stopPropagation();
    if (e.key === 'Enter') {
      var m = /\d+/.exec(pageInput.value);
      if (m) goPage(parseInt(m[0], 10));
      pageInput.blur();
    } else if (e.key === 'Escape') {
      pageInput.blur();
    }
  });
  pageInput.addEventListener('blur', function () { pageInput.value = label(target()); });

  // -------------------------------------------------------------- buttons

  function setPressed(act, on) {
    app.querySelectorAll('[data-act="' + act + '"]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(!!on));
    });
  }

  function setButton(act, name, text) {
    app.querySelectorAll('[data-act="' + act + '"]').forEach(function (b) {
      b.innerHTML = icon(name);
      b.setAttribute('aria-label', text);
      b.title = text;
    });
  }

  // The view button offers the mode that is not showing; it reads as
  // pressed while the reader's choice overrides the automatic one.
  function viewUI() {
    setButton('view', double ? 'pageSingle' : 'pageDouble', double ? T.viewSingle : T.viewDouble);
    setPressed('view', !viewAuto);
    app.querySelectorAll('[data-act="view"]').forEach(function (b) { b.hidden = !viewOffered; });
    barCounts();
  }

  // The slider takes the room the buttons on either side leave.
  function barCounts() {
    app.style.setProperty('--fc-nl', String(barL.querySelectorAll('.fc-btn:not([hidden])').length));
    app.style.setProperty('--fc-nr', String(barR.querySelectorAll('.fc-btn:not([hidden])').length));
  }

  function download() {
    var a = el('a');
    a.href = cfg.download;
    a.download = cfg.fileName || '';
    a.rel = 'noopener';
    app.appendChild(a);
    a.click();
    a.remove();
  }

  function toggleView() {
    // Back to automatic if a choice is in force, else the other mode.
    forcedView = viewAuto ? (double ? 'single' : 'double') : null;
    store('fc-view', forcedView || 'auto');
    layout();
  }

  var homeURL = cfg.homeURL;

  function initButtons() {
    setButton('home', 'home', T.home);
    setButton('zoom', 'zoomIn', T.zoomIn);
    setButton('thumbs', 'grid', T.thumbs);
    setButton('play', 'play', T.play);
    setButton('share', 'share', T.share);
    setButton('download', 'download', T.download);
    setButton('fullscreen', 'fullscreen', T.fullscreen);
    prevArrow.setAttribute('aria-label', T.prev);
    nextArrow.setAttribute('aria-label', T.next);
    prevArrow.title = T.prev;
    nextArrow.title = T.next;
    thumbsPrev.setAttribute('aria-label', T.scrollLeft);
    thumbsNext.setAttribute('aria-label', T.scrollRight);
    soundUI();
    if (!homeURL) app.querySelectorAll('[data-act="home"]').forEach(function (b) { b.hidden = true; });
    if (!fsEnabled()) app.querySelectorAll('[data-act="fullscreen"]').forEach(function (b) { b.hidden = true; });
    if (!share) app.querySelectorAll('[data-act="share"]').forEach(function (b) { b.hidden = true; });
    if (!cfg.download) app.querySelectorAll('[data-act="download"]').forEach(function (b) { b.hidden = true; });
    setPressed('thumbs', false);
    setPressed('play', false);
    barCounts();
  }

  app.addEventListener('click', function (e) {
    var b = e.target.closest('[data-act]');
    if (!b) return;
    switch (b.getAttribute('data-act')) {
      case 'prev': go(-1); break;
      case 'next': go(1); break;
      case 'home': if (homeURL) location.href = homeURL; break;
      case 'zoom': toggleZoom(); break;
      case 'thumbs': setThumbs(!thumbsOpen); break;
      case 'view': toggleView(); break;
      case 'play': setAutoplay(!autoplay); break;
      case 'sound': setSound(!soundOn); break;
      case 'share': if (share) share.toggle(); break;
      case 'download': download(); break;
      case 'fullscreen': toggleFullscreen(); break;
      case 'thumbsPrev': thumbsView.scrollBy({ left: -thumbsView.clientWidth * 0.8, behavior: 'smooth' }); break;
      case 'thumbsNext': thumbsView.scrollBy({ left: thumbsView.clientWidth * 0.8, behavior: 'smooth' }); break;
    }
  });

  // ------------------------------------------------------------- autoplay

  var autoplay = false, autoTimer = 0;
  function setAutoplay(on) {
    autoplay = on;
    clearInterval(autoTimer);
    if (on) {
      if (cur >= count() - 1) flipTo(0);
      autoTimer = setInterval(function () {
        if (target() >= count() - 1) setAutoplay(false); else go(1);
      }, 3000);
    }
    setButton('play', on ? 'pause' : 'play', on ? T.pause : T.play);
    setPressed('play', on);
  }

  // ---------------------------------------------------------------- sound

  var soundOn = store('fc-sound') !== 'off';
  var audio = null;
  function setSound(on) {
    soundOn = on;
    store('fc-sound', on ? 'on' : 'off');
    soundUI();
  }
  function soundUI() {
    setButton('sound', soundOn ? 'soundOn' : 'soundOff', soundOn ? T.soundOn : T.soundOff);
  }
  // A short paper rustle: filtered noise with a quick swell and fall-off.
  function flipSound() {
    if (!soundOn) return;
    try {
      var AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return;
      if (!audio) audio = new AC();
      if (audio.state === 'suspended') audio.resume();
      var sr = audio.sampleRate, len = Math.floor(sr * 0.42);
      var buf = audio.createBuffer(1, len, sr);
      var d = buf.getChannelData(0);
      var grain = 0;
      for (var i = 0; i < len; i++) {
        var t = i / len;
        var env = Math.pow(Math.sin(Math.PI * Math.min(1, t * 1.6)), 2) * (1 - t);
        if (i % 90 === 0) grain = 0.55 + Math.random() * 0.45;
        d[i] = (Math.random() * 2 - 1) * env * grain;
      }
      var src = audio.createBufferSource();
      src.buffer = buf;
      var bp = audio.createBiquadFilter();
      bp.type = 'bandpass';
      bp.frequency.value = 2600;
      bp.Q.value = 0.6;
      var hp = audio.createBiquadFilter();
      hp.type = 'highpass';
      hp.frequency.value = 700;
      var g = audio.createGain();
      g.gain.value = 0.35;
      src.connect(bp); bp.connect(hp); hp.connect(g); g.connect(audio.destination);
      src.start();
    } catch (_) { /* audio is optional */ }
  }

  // ----------------------------------------------------------- fullscreen

  // A book in a post goes full screen on its own. Where the browser has no element full screen
  // (iPhone Safari) it is laid over the page instead, so the reader never leaves the page.
  var pseudoFS = false;
  function fsNative() {
    return !!(document.fullscreenEnabled || document.webkitFullscreenEnabled);
  }
  function fsEnabled() { return fsNative() || !cfg.full; }
  function fsElement() {
    var f = document.fullscreenElement || document.webkitFullscreenElement;
    if (cfg.full) return f;
    return f === host || pseudoFS ? host : null;
  }
  function setPseudo(on) {
    pseudoFS = on;
    document.documentElement.style.overflow = on ? 'hidden' : '';
    fsUI();
  }
  function toggleFullscreen() {
    var d = cfg.full ? document.documentElement : host;
    var req = d.requestFullscreen || d.webkitRequestFullscreen;
    if (pseudoFS) {
      setPseudo(false);
    } else if (document.fullscreenElement || document.webkitFullscreenElement) {
      (document.exitFullscreen || document.webkitExitFullscreen).call(document);
    } else if (req && fsNative()) {
      var p = req.call(d);
      if (p && p.catch) p.catch(function () { if (!cfg.full) setPseudo(true); });
    } else if (!cfg.full) {
      setPseudo(true);
    }
  }
  function fsUI() {
    var on = !!fsElement();
    host.classList.toggle('is-fullscreen', on && !cfg.full);
    setButton('fullscreen', on ? 'exitFullscreen' : 'fullscreen', on ? T.exitFullscreen : T.fullscreen);
  }
  document.addEventListener('fullscreenchange', fsUI, { signal: ac.signal });
  document.addEventListener('webkitfullscreenchange', fsUI, { signal: ac.signal });

  // ------------------------------------------------------------- keyboard

  (cfg.full ? document : host).addEventListener('keydown', function (e) {
    var t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.altKey || e.metaKey || e.ctrlKey) return;
    var k = e.key;
    if (k === 'ArrowRight' || k === 'PageDown') go(1);
    else if (k === 'ArrowLeft' || k === 'PageUp') go(-1);
    else if (k === 'Home') flipTo(0);
    else if (k === 'End') flipTo(count() - 1);
    else if (k === '+' || k === '=') zoomAt(G.W / 2, G.H / 2, zoom.s * 1.5, true);
    else if (k === '-') zoomAt(G.W / 2, G.H / 2, zoom.s / 1.5, true);
    else if (k === 'Escape') {
      if (zoom.s > 1) resetZoom(true);
      else if (thumbsOpen) setThumbs(false);
      else if (pseudoFS) setPseudo(false);
      else return;
    } else return;
    e.preventDefault();
  }, { signal: ac.signal });

  // ---------------------------------------------------------------- start

  // The full-screen page keeps the page in the address (#p=6); a book in a post opens at the page
  // a shared link names (#pfb<id>=6).
  function pageFromHash() {
    var m = new RegExp('[#&]' + cfg.hashKey + '=(\\d+)').exec(location.hash);
    return m ? Math.max(1, Math.min(N, parseInt(m[1], 10))) : 1;
  }

  if (cfg.full) {
    window.addEventListener('hashchange', function () {
      if (location.hash === lastHash) return;
      goPage(pageFromHash());
    }, { signal: ac.signal });
  }

  var resizeRaf = 0, lastW = 0, lastH = 0;
  function relayout() {
    if (!host.isConnected) { destroy(); return; }
    if (app.clientWidth === lastW && app.clientHeight === lastH) return;
    cancelAnimationFrame(resizeRaf);
    resizeRaf = requestAnimationFrame(function () {
      lastW = app.clientWidth;
      lastH = app.clientHeight;
      layout();
    });
  }
  var sizeRO = new ResizeObserver(relayout);
  sizeRO.observe(app);
  window.addEventListener('resize', relayout, { signal: ac.signal });

  // Release everything the book holds: the page canvases, pdf.js's document, timers, listeners.
  function destroy() {
    if (dead) return;
    dead = true;
    ac.abort();
    sizeRO.disconnect();
    thumbIO.disconnect();
    cancelAnimationFrame(resizeRaf);
    if (anim) { cancelAnimationFrame(anim.raf); clearTimeout(anim.safety); anim = null; }
    pending = null;
    [autoTimer, wheelTimer, tapTimer, sharpTimer].forEach(function (t) { clearTimeout(t); });
    clearInterval(autoTimer);
    queue.length = 0;
    Object.keys(cache).forEach(dropPage);
    lru.length = 0;
    app.querySelectorAll('canvas').forEach(function (c) { c.width = c.height = 0; });
    if (share) share.destroy();
    if (pseudoFS) document.documentElement.style.overflow = '';
    if (audio && audio.close) { try { audio.close(); } catch (_) { /* already closed */ } }
    audio = null;
    app.remove();
    delete host.pfbViewer;
    try { cfg.pdf.destroy(); } catch (_) { /* already gone */ }
  }

  if (cfg.shareURL) {
    share = createShare(app, {
      url: cfg.shareURL, hashKey: cfg.hashKey, qr: cfg.qr, path: cfg.sharePath, title: cfg.title, tr: TR,
      page: function () { return firstPage(cur); }
    });
  }

  initButtons();
  buildThumbs();
  lastW = app.clientWidth;
  lastH = app.clientHeight;
  layout();
  cur = Math.min(spreadOf(pageFromHash()), count() - 1);
  render();
  afterChange();
  if (!cfg.full && cur > 0) host.scrollIntoView({ block: 'center' }); // opened by a shared link
  // A link shared as "full screen" (#pfb<id>=<page>&fs) lays the book over the page at once; the
  // browser's own full screen cannot start without a click.
  if (!cfg.full && new RegExp('[#&]' + cfg.hashKey + '=\\d+&fs\\b').test(location.hash)) setPseudo(true);

  // Hide the spinner once the first pages are on screen (or after 8s).
  (function () {
    var s = spread(cur);
    var hide = function () {
      loading.classList.add('fc-hide');
      setTimeout(function () { loading.remove(); }, 400);
    };
    var timer = setTimeout(hide, 8000);
    Promise.all([s.l, s.r].filter(Boolean).map(function (n) { return pageEl(n).ready; })).then(function () {
      clearTimeout(timer);
      hide();
    });
  })();

  return {
    title: cfg.title,
    pageCount: N,
    page: function () { return firstPage(cur); },
    goTo: goPage,
    next: function () { go(1); },
    prev: function () { go(-1); },
    destroy: destroy
  };
}

// ------------------------------------------------------------------ mount

let worker = null; // one pdf.js worker for every book on the page

function fail(host, err) {
  console.error('Pursae Flipbook:', err);
  const l = host.querySelector('.fc-loading');
  if (!l) return;
  l.textContent = '';
  const p = document.createElement('p');
  p.textContent = T.error;
  const a = document.createElement('a');
  a.className = 'fc-pdf-link';
  a.href = host.dataset.pdf;
  a.target = '_blank';
  a.rel = 'noopener';
  a.textContent = T.open;
  l.append(p, a);
}

async function mount(host) {
  const d = host.dataset;
  const full = host.classList.contains('pfb--full');
  let pdf = null;
  try {
    if (!worker) {
      pdfjs.GlobalWorkerOptions.workerSrc = d.worker;
      worker = new pdfjs.PDFWorker();
    }
    // Ranges are fetched as pages are opened, so a large PDF is never held whole in memory.
    pdf = await pdfjs.getDocument({ url: d.pdf, worker, isEvalSupported: false, disableAutoFetch: true, disableStream: true }).promise;
    const first = await pdf.getPage(1);
    const vp = first.getViewport({ scale: 1 });
    if (!host.isConnected) { pdf.destroy(); return; }
    if (!full) host.tabIndex = 0;
    host.setAttribute('role', 'region');
    host.setAttribute('aria-label', d.title || 'Flipbook');
    host.pfbViewer = createViewer(host, {
      pdf, pages: pdf.numPages, width: vp.width, height: vp.height, title: d.title || '', full,
      homeURL: full ? d.home || '' : '',
      download: d.download ? d.pdf : '',
      fileName: d.file || '',
      // A shared link is the address of the page the book is on, never an address of its own.
      shareURL: d.share && !full ? location.origin + location.pathname + location.search : '',
      sharePath: location.pathname + location.search,
      qr: d.share && d.qr ? d.qr : '',
      hashKey: full ? 'p' : 'pfb' + (d.id || '')
    });
  } catch (err) {
    if (pdf) { try { pdf.destroy(); } catch (_) { /* already gone */ } }
    fail(host, err);
  }
}

// A book is loaded when it comes near the screen, not with the page: several books in a post, or
// one far below the fold, cost nothing until they are reached.
const near = new IntersectionObserver((entries) => {
  for (const en of entries) {
    if (!en.isIntersecting) continue;
    near.unobserve(en.target);
    mount(en.target);
  }
}, { rootMargin: '600px' });

function boot() {
  document.querySelectorAll('[data-pfb]:not([data-pfb-on])').forEach((host) => {
    host.dataset.pfbOn = '1';
    const linked = host.dataset.id && new RegExp('[#&]pfb' + host.dataset.id + '=\\d').test(location.hash);
    if (host.classList.contains('pfb--full') || linked) mount(host);
    else near.observe(host);
  });
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();
