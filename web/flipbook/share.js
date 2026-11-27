/*
 * Pursae Flipbook share panel: the link with a copy button, a QR code of it (drawn by the site at
 * <book>/qr.svg and qr.png) and quick share targets. thecoolPDF's share panel (template/player/
 * share.js), made an instance that lives inside the book so it also shows in full screen.
 *
 * createShare(app, o): o = { url (the page the book is on), hashKey (the link opens at a page as
 * #<hashKey>=<page>, with &fs laid over that page full screen), qr (address of the QR images without the extension, or ''), path (that page's
 * path, which the QR code encodes), title, page() (the page showing), tr (Turkish labels) }.
 */
const STR = {
  tr: {
    share: 'Paylaş', close: 'Kapat', group: 'Link nereden açılsın', start: 'Baştan', page: 'Bu sayfadan', fs: 'Tam ekran açılsın',
    link: 'Sayfa linki', copy: 'Kopyala', copied: 'Kopyalandı', qrAlt: 'Flipbook linkinin QR kodu',
    qrHint: 'Telefon kamerasıyla okutunca bu sayfa açılır.', png: 'PNG indir', svg: 'SVG indir', native: 'Paylaş…', mail: 'E-posta'
  },
  en: {
    share: 'Share', close: 'Close', group: 'Where the link opens', start: 'From the start', page: 'From this page', fs: 'Open full screen',
    link: 'Page link', copy: 'Copy', copied: 'Copied', qrAlt: 'QR code of the flipbook link',
    qrHint: 'Scan it with a phone camera to open this page.', png: 'Download PNG', svg: 'Download SVG', native: 'Share…', mail: 'Email'
  }
};

const ICON = {
  close: '<path d="M6 6l12 12M18 6 6 18"/>',
  copy: '<rect x="8.5" y="8.5" width="11" height="11" rx="2.2"/><path d="M15.5 8.5V6a1.5 1.5 0 0 0-1.5-1.5H6A1.5 1.5 0 0 0 4.5 6v8A1.5 1.5 0 0 0 6 15.5h2.5"/>',
  check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
  download: '<path d="M12 4v11M7.5 10.5 12 15l4.5-4.5M4.5 19.5h15"/>',
  share: '<path d="M12 15V4M7.5 8.5 12 4l4.5 4.5"/><path d="M5 12v6.5A1.5 1.5 0 0 0 6.5 20h11a1.5 1.5 0 0 0 1.5-1.5V12"/>'
};
function icon(name) {
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + ICON[name] + '</svg>';
}

let serial = 0;

export function createShare(app, o) {
  const S = o.tr ? STR.tr : STR.en;
  const TARGETS = [
    { name: 'WhatsApp', url: (u, t) => 'https://wa.me/?text=' + encodeURIComponent(t + ' ' + u) },
    { name: 'X', url: (u, t) => 'https://twitter.com/intent/tweet?url=' + encodeURIComponent(u) + '&text=' + encodeURIComponent(t) },
    { name: 'Facebook', url: (u) => 'https://www.facebook.com/sharer/sharer.php?u=' + encodeURIComponent(u) },
    { name: 'LinkedIn', url: (u) => 'https://www.linkedin.com/sharing/share-offsite/?url=' + encodeURIComponent(u) },
    { name: 'Telegram', url: (u, t) => 'https://t.me/share/url?url=' + encodeURIComponent(u) + '&text=' + encodeURIComponent(t) },
    { name: S.mail, url: (u, t) => 'mailto:?subject=' + encodeURIComponent(t) + '&body=' + encodeURIComponent(u) }
  ];

  let root, linkInput, copyBtn, qrBox, qrImg, pngLink, svgLink, pageBtn, startBtn, lastFocus;
  let fromPage = false;
  let fullScreen = false;
  let copyTimer = 0;
  const ac = new AbortController();

  const page = () => Math.max(1, o.page() || 1);
  const startPage = () => (fromPage && page() > 1 ? page() : 1);
  const shareURL = () => o.url.split('#')[0] +
    (startPage() > 1 || fullScreen ? '#' + o.hashKey + '=' + startPage() + (fullScreen ? '&fs' : '') : '');

  function build() {
    const titleID = 'fc-share-title-' + (++serial);
    root = document.createElement('div');
    root.className = 'fc-share';
    root.hidden = true;
    root.innerHTML =
      '<div class="fc-share-backdrop" data-close></div>' +
      '<div class="fc-share-card" role="dialog" aria-modal="true" aria-labelledby="' + titleID + '">' +
        '<div class="fc-share-head">' +
          '<h2 id="' + titleID + '">' + S.share + '</h2>' +
          '<button type="button" class="fc-share-x" data-close aria-label="' + S.close + '">' + icon('close') + '</button>' +
        '</div>' +
        '<div class="fc-share-seg" role="group" aria-label="' + S.group + '">' +
          '<button type="button" data-start aria-pressed="true">' + S.start + '</button>' +
          '<button type="button" data-page aria-pressed="false">' + S.page + '</button>' +
        '</div>' +
        '<label class="fc-share-fs"><input type="checkbox" data-fs>' + S.fs + '</label>' +
        '<div class="fc-share-link">' +
          '<input type="text" readonly aria-label="' + S.link + '">' +
          '<button type="button" class="fc-share-copy">' + icon('copy') + '<span>' + S.copy + '</span></button>' +
        '</div>' +
        '<div class="fc-share-qr">' +
          '<div class="fc-share-qr-img"><img alt="' + S.qrAlt + '" width="168" height="168"></div>' +
          '<div class="fc-share-qr-side">' +
            '<p>' + S.qrHint + '</p>' +
            '<a class="fc-share-btn" data-png download>' + icon('download') + S.png + '</a>' +
            '<a class="fc-share-btn" data-svg download>' + icon('download') + S.svg + '</a>' +
          '</div>' +
        '</div>' +
        '<div class="fc-share-targets"></div>' +
      '</div>';
    app.appendChild(root);

    linkInput = root.querySelector('.fc-share-link input');
    copyBtn = root.querySelector('.fc-share-copy');
    qrBox = root.querySelector('.fc-share-qr');
    qrImg = qrBox.querySelector('img');
    // No QR to show (the site did not draw one): hide the block instead of a broken image.
    qrImg.addEventListener('error', () => { qrBox.hidden = true; });
    qrImg.addEventListener('load', () => { qrBox.hidden = false; });
    pngLink = root.querySelector('[data-png]');
    svgLink = root.querySelector('[data-svg]');
    startBtn = root.querySelector('[data-start]');
    pageBtn = root.querySelector('[data-page]');

    const targets = root.querySelector('.fc-share-targets');
    if (navigator.share) {
      const nativeBtn = document.createElement('button');
      nativeBtn.type = 'button';
      nativeBtn.className = 'fc-share-pill fc-share-native';
      nativeBtn.innerHTML = icon('share') + S.native;
      nativeBtn.addEventListener('click', () => {
        navigator.share({ title: o.title, url: shareURL() }).catch(() => { /* cancelled */ });
      });
      targets.appendChild(nativeBtn);
    }
    TARGETS.forEach((t) => {
      const a = document.createElement('a');
      a.className = 'fc-share-pill';
      a.textContent = t.name;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      const set = () => { a.href = t.url(shareURL(), o.title); };
      a.addEventListener('click', set);
      a.addEventListener('mouseenter', set);
      a.addEventListener('focus', set);
      targets.appendChild(a);
    });

    root.addEventListener('click', (e) => {
      if (e.target.closest('[data-close]')) hide();
    });
    startBtn.addEventListener('click', () => { fromPage = false; refresh(); });
    pageBtn.addEventListener('click', () => { fromPage = true; refresh(); });
    root.querySelector('[data-fs]').addEventListener('change', (e) => { fullScreen = e.target.checked; refresh(); });
    copyBtn.addEventListener('click', copy);
    linkInput.addEventListener('focus', () => linkInput.select());
    document.addEventListener('keydown', (e) => {
      if (!root.hidden && e.key === 'Escape') { e.stopPropagation(); hide(); }
    }, { capture: true, signal: ac.signal });
    // Keep the viewer from turning pages while typing/clicking in the panel.
    ['keydown', 'keyup', 'wheel', 'mousedown', 'touchstart', 'pointerdown', 'click'].forEach((ev) => {
      root.addEventListener(ev, (e) => e.stopPropagation(), { passive: true });
    });
  }

  function refresh() {
    const p = page();
    pageBtn.textContent = p > 1 ? S.page + ' (' + p + ')' : S.page;
    pageBtn.disabled = p <= 1;
    if (p <= 1) fromPage = false;
    startBtn.setAttribute('aria-pressed', String(!fromPage));
    pageBtn.setAttribute('aria-pressed', String(fromPage));

    linkInput.value = shareURL();
    if (o.qr) {
      const q = '?u=' + encodeURIComponent(o.path) + (startPage() > 1 ? '&p=' + startPage() : '') + (fullScreen ? '&fs=1' : '');
      qrImg.src = o.qr + '.svg' + q;
      svgLink.href = o.qr + '.svg' + q + '&dl=1';
      pngLink.href = o.qr + '.png' + q + '&dl=1';
    } else {
      qrBox.hidden = true;
    }
    resetCopy();
  }

  function resetCopy() {
    clearTimeout(copyTimer);
    copyBtn.classList.remove('done');
    copyBtn.innerHTML = icon('copy') + '<span>' + S.copy + '</span>';
  }

  function copy() {
    const text = shareURL();
    const done = () => {
      copyBtn.classList.add('done');
      copyBtn.innerHTML = icon('check') + '<span>' + S.copied + '</span>';
      clearTimeout(copyTimer);
      copyTimer = setTimeout(resetCopy, 1800);
    };
    const fallback = () => {
      linkInput.focus();
      linkInput.select();
      try { if (document.execCommand('copy')) done(); } catch (_) { /* the reader can copy by hand */ }
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fallback);
    else fallback();
  }

  function show() {
    if (!root) build();
    lastFocus = document.activeElement;
    refresh();
    root.hidden = false;
    requestAnimationFrame(() => root.classList.add('open'));
    copyBtn.focus();
  }

  function hide() {
    if (!root || root.hidden) return;
    root.classList.remove('open');
    root.hidden = true;
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }

  return {
    show,
    hide,
    toggle() { if (root && !root.hidden) hide(); else show(); },
    destroy() {
      ac.abort();
      clearTimeout(copyTimer);
      if (root) root.remove();
      root = null;
    }
  };
}
