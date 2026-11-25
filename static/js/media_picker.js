// メディアピッカー: 記事編集画面からメディアライブラリの画像を選び、本文のカーソル位置に挿入するか、
// アイキャッチ画像に設定する。data-media-picker-open を付けたボタンで開く。
(function () {
  'use strict';

  var dialog = document.getElementById('media-picker');
  if (!dialog) return;
  var grid = document.getElementById('media-picker-grid');
  var statusEl = document.getElementById('media-picker-status');
  var countEl = document.getElementById('media-picker-count');
  var moreButton = document.getElementById('media-picker-more');
  var searchForm = document.getElementById('media-picker-search');
  var queryInput = document.getElementById('media-picker-q');
  var selectedImg = document.getElementById('media-picker-selected');
  var altInput = document.getElementById('media-picker-alt');
  var captionInput = document.getElementById('media-picker-caption');
  var insertButton = document.getElementById('media-picker-insert');
  var featuredButton = document.getElementById('media-picker-featured');
  var textarea = document.getElementById('content_markdown');
  var featuredInput = document.getElementById('featured_image_url');
  var usedSection = document.getElementById('media-picker-used');
  var usedTitle = document.getElementById('media-picker-used-title');
  var usedGrid = document.getElementById('media-picker-used-grid');
  var libraryTitle = document.getElementById('media-picker-library-title');
  var useFirstButton = document.getElementById('featured-image-use-first');

  // mode は開いた目的（featured: アイキャッチを選ぶ、insert: 本文に挿入する）。
  var state = { q: '', page: 0, hasNext: false, loading: false, selected: null, request: 0, mode: 'insert' };

  // collectBodyImages は本文（Markdown）で使っている画像を出現順・URL の重複なしで返す（[{url, alt, caption}]）。
  // Markdown の ![alt](url "caption") と HTML の <img src>（:::gallery の中も同じ記法）を拾い、コードブロック・
  // インラインコードの中の記法は除く。アイキャッチに使えない URL（data:・相対パス・// で始まる URL など。サーバーの
  // validFeaturedImageURL と同じ規則）も除く。
  function collectBodyImages(text) {
    var lines = String(text || '').replace(/\r\n?/g, '\n').split('\n');
    var fence = null;
    for (var i = 0; i < lines.length; i++) {
      var m = /^ {0,3}(`{3,}|~{3,})/.exec(lines[i]);
      if (fence) {
        if (m && m[1].charAt(0) === fence.charAt(0) && m[1].length >= fence.length && /^ {0,3}[`~]+\s*$/.test(lines[i])) fence = null;
        lines[i] = '';
      } else if (m) {
        fence = m[1];
        lines[i] = '';
      }
    }
    var src = lines.join('\n').replace(/`[^`\n]*`/g, ' ').replace(/<!--[\s\S]*?-->/g, ' ');
    var found = [];
    var md = /!\[([^\]\n]*)\]\(\s*(<[^>\n]+>|[^)\s]+)(?:\s+(?:"([^"\n]*)"|'([^'\n]*)'))?\s*\)/g;
    var hit;
    while ((hit = md.exec(src))) {
      found.push({ index: hit.index, url: hit[2].replace(/^<|>$/g, ''), alt: hit[1], caption: hit[3] || hit[4] || '' });
    }
    var html = /<img\b[^>]*>/gi;
    while ((hit = html.exec(src))) {
      var tag = hit[0];
      var s = /\ssrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/i.exec(tag);
      if (!s) continue;
      var a = /\salt\s*=\s*(?:"([^"]*)"|'([^']*)')/i.exec(tag);
      found.push({ index: hit.index, url: s[1] || s[2] || s[3] || '', alt: a ? (a[1] || a[2] || '') : '', caption: '' });
    }
    found.sort(function (x, y) { return x.index - y.index; });
    var seen = {};
    var out = [];
    found.forEach(function (f) {
      var url = f.url.trim();
      if (!usableImageURL(url) || seen[url]) return;
      seen[url] = true;
      out.push({ url: url, alt: f.alt.trim(), caption: f.caption.trim() });
    });
    return out;
  }

  // usableImageURL はアイキャッチに設定できる URL か（/ で始まるパス、または http(s) の絶対 URL）。
  function usableImageURL(url) {
    if (!url || /[\s"'<>`]/.test(url)) return false;
    if (url.charAt(0) === '/') return !/^\/[\/\\]/.test(url);
    return /^https?:\/\/[^\/\s]+/i.test(url);
  }
  // ブラウザでの動作確認用（Node.js を使わない方針のため、実ブラウザから呼んで確かめる）。
  window.cmsCollectBodyImages = collectBodyImages;

  function msg(name, n) {
    var text = dialog.getAttribute('data-msg-' + name) || '';
    return n === undefined ? text : text.replace('{n}', String(n));
  }

  function select(item, button) {
    state.selected = item;
    grid.querySelectorAll('[aria-pressed="true"]').forEach(function (b) { b.setAttribute('aria-pressed', 'false'); });
    button.setAttribute('aria-pressed', 'true');
    selectedImg.src = item.thumbnail_url;
    selectedImg.alt = item.alt;
    selectedImg.hidden = false;
    altInput.disabled = false;
    captionInput.disabled = false;
    altInput.value = item.alt || stem(item.filename);
    captionInput.value = item.caption || '';
    insertButton.disabled = !textarea || textarea.readOnly;
    featuredButton.disabled = !featuredInput;
    usedGrid.querySelectorAll('[aria-pressed="true"]').forEach(function (b) { b.setAttribute('aria-pressed', 'false'); });
  }

  function clearSelection() {
    state.selected = null;
    selectedImg.hidden = true;
    selectedImg.removeAttribute('src');
    altInput.value = '';
    captionInput.value = '';
    altInput.disabled = true;
    captionInput.disabled = true;
    insertButton.disabled = true;
    featuredButton.disabled = true;
  }

  function stem(name) {
    return String(name || '').replace(/\.[^.]*$/, '');
  }

  function renderItem(item) {
    var li = document.createElement('li');
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'media-picker-item block w-full overflow-hidden rounded-lg border border-neutral-200 bg-white text-left hover:border-zinc-400';
    b.setAttribute('aria-pressed', 'false');
    b.setAttribute('data-media-id', item.id);
    b.title = item.filename;
    var frame = document.createElement('span');
    frame.className = 'block aspect-square bg-neutral-100';
    var img = document.createElement('img');
    img.src = item.thumbnail_url;
    img.alt = item.alt || '';
    img.loading = 'lazy';
    img.className = 'h-full w-full object-cover';
    frame.appendChild(img);
    var label = document.createElement('span');
    label.className = 'block truncate px-1.5 py-1 text-[11px] text-neutral-600';
    label.textContent = item.alt || item.filename;
    b.appendChild(frame);
    b.appendChild(label);
    b.addEventListener('click', function () { select(item, b); });
    // ダブルクリックは開いた目的の操作（アイキャッチに設定・本文に挿入）をそのまま行う。
    b.addEventListener('dblclick', function () {
      select(item, b);
      var primary = state.mode === 'featured' ? featuredButton : insertButton;
      if (!primary.disabled) primary.click();
    });
    li.appendChild(b);
    return li;
  }

  // renderUsedItem は「この記事で使っている画像」の1枚。押すとすぐアイキャッチに設定してモーダルを閉じる。
  function renderUsedItem(img) {
    var li = document.createElement('li');
    li.className = 'w-24 flex-shrink-0';
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'media-picker-item block w-full overflow-hidden rounded-lg border border-neutral-200 bg-white text-left hover:border-zinc-400';
    b.setAttribute('aria-pressed', featuredInput && featuredInput.value === img.url ? 'true' : 'false');
    b.setAttribute('data-used-url', img.url);
    b.title = img.alt || img.url;
    var frame = document.createElement('span');
    frame.className = 'block aspect-square bg-neutral-100';
    var el = document.createElement('img');
    el.src = img.url;
    el.alt = img.alt || '';
    el.loading = 'lazy';
    el.className = 'h-full w-full object-cover';
    // 読み込めない画像（外部 URL の削除など）は候補から外す。
    el.addEventListener('error', function () {
      li.remove();
      updateUsedTitle();
    });
    frame.appendChild(el);
    var label = document.createElement('span');
    label.className = 'block truncate px-1.5 py-1 text-[11px] text-neutral-600';
    label.textContent = img.alt || img.url.replace(/^.*\//, '');
    b.appendChild(frame);
    b.appendChild(label);
    b.addEventListener('click', function () {
      setFeatured(img.url);
      dialog.close();
      window.cmsToast(msg('featured'), 'success');
    });
    li.appendChild(b);
    return li;
  }

  function updateUsedTitle() {
    var n = usedGrid.children.length;
    usedTitle.textContent = msg('used', n);
    usedSection.hidden = n === 0;
    libraryTitle.hidden = n === 0;
  }

  // showUsedImages はアイキャッチを選ぶときだけ、本文の画像を先頭に並べる（0枚・本文への挿入では出さない）。
  function showUsedImages() {
    usedGrid.textContent = '';
    if (state.mode === 'featured' && textarea) {
      collectBodyImages(textarea.value).forEach(function (img) { usedGrid.appendChild(renderUsedItem(img)); });
    }
    updateUsedTitle();
  }

  // load は検索語 state.q の次のページを読み、一覧の末尾に足す。reset なら一覧を作り直す。
  function load(reset) {
    if (reset) {
      state.page = 0;
      state.hasNext = false;
      grid.textContent = '';
      clearSelection();
    }
    var req = ++state.request;
    state.loading = true;
    moreButton.hidden = true;
    statusEl.textContent = msg('loading');
    var params = new URLSearchParams({ page: String(state.page + 1) });
    if (state.q) params.set('q', state.q);
    fetch(dialog.getAttribute('data-api') + '?' + params.toString(), {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) throw new Error(msg('error'));
      return res.json();
    }).then(function (json) {
      if (req !== state.request) return;
      state.page = json.page;
      state.hasNext = json.has_next;
      (json.items || []).forEach(function (item) { grid.appendChild(renderItem(item)); });
      countEl.textContent = msg('count', json.total);
      statusEl.textContent = grid.children.length === 0 ? msg('empty') : '';
      moreButton.hidden = !state.hasNext;
    }).catch(function (err) {
      if (req !== state.request) return;
      statusEl.textContent = err.message || msg('error');
    }).finally(function () {
      if (req === state.request) state.loading = false;
    });
  }

  function markdownFor(item) {
    var alt = altInput.value.replace(/[\[\]\\]/g, '').replace(/[\r\n]+/g, ' ').trim() || stem(item.filename);
    var caption = captionInput.value.replace(/["\\]/g, '').replace(/[\r\n]+/g, ' ').trim();
    return '![' + alt + '](' + item.url + (caption ? ' "' + caption + '"' : '') + ')';
  }

  // insertAtCursor は本文のカーソル位置に text を独立した行として挿入し、プレビューを更新させる。
  function insertAtCursor(text) {
    var start = textarea.selectionStart;
    var end = textarea.selectionEnd;
    var before = textarea.value.slice(0, start);
    var prefix = before && !before.endsWith('\n') ? '\n' : '';
    textarea.setRangeText(prefix + text + '\n', start, end, 'end');
    textarea.focus();
    textarea.dispatchEvent(new Event('input'));
  }

  function setFeatured(url) {
    if (!featuredInput) return;
    featuredInput.value = url;
    featuredInput.dispatchEvent(new Event('change', { bubbles: true }));
    var preview = document.getElementById('featured-image-preview');
    var label = document.getElementById('featured-image-label');
    var clear = document.getElementById('featured-image-clear');
    if (preview) {
      preview.hidden = !url;
      if (url) preview.src = url; else preview.removeAttribute('src');
    }
    if (label) label.textContent = url || label.getAttribute('data-empty') || '';
    if (clear) clear.hidden = !url;
    updateUseFirst();
  }

  // updateUseFirst は「本文の最初の画像を使う」を、アイキャッチが未設定で本文に画像があるときだけ出す。
  function updateUseFirst() {
    if (!useFirstButton) return;
    useFirstButton.hidden = !featuredInput || !!featuredInput.value || !textarea || collectBodyImages(textarea.value).length === 0;
  }

  insertButton.addEventListener('click', function () {
    if (!state.selected || !textarea) return;
    var text = markdownFor(state.selected);
    dialog.close();
    insertAtCursor(text);
    window.cmsToast(msg('inserted'), 'success');
  });
  featuredButton.addEventListener('click', function () {
    if (!state.selected) return;
    setFeatured(state.selected.url);
    dialog.close();
    window.cmsToast(msg('featured'), 'success');
  });
  var clearFeatured = document.getElementById('featured-image-clear');
  if (clearFeatured) clearFeatured.addEventListener('click', function () { setFeatured(''); });
  if (useFirstButton) {
    useFirstButton.addEventListener('click', function () {
      var first = textarea ? collectBodyImages(textarea.value)[0] : null;
      if (!first) return;
      setFeatured(first.url);
      window.cmsToast(msg('featured'), 'success');
    });
    if (textarea) {
      var useFirstTimer = 0;
      textarea.addEventListener('input', function () {
        clearTimeout(useFirstTimer);
        useFirstTimer = setTimeout(updateUseFirst, 300);
      });
    }
    updateUseFirst();
  }

  searchForm.addEventListener('submit', function (e) {
    e.preventDefault();
    state.q = queryInput.value.trim();
    load(true);
  });
  moreButton.addEventListener('click', function () { if (!state.loading && state.hasNext) load(false); });
  dialog.querySelectorAll('[data-media-picker-close]').forEach(function (b) {
    b.addEventListener('click', function () { dialog.close(); });
  });
  dialog.addEventListener('click', function (e) { if (e.target === dialog) dialog.close(); });

  document.querySelectorAll('[data-media-picker-open]').forEach(function (b) {
    b.addEventListener('click', function () {
      // 開いた目的（本文への挿入・アイキャッチ）の操作を主ボタンにする。アイキャッチを選ぶときは「本文に挿入」を出さない。
      var featuredMode = b.getAttribute('data-media-picker-open') === 'featured';
      state.mode = featuredMode ? 'featured' : 'insert';
      featuredButton.classList.toggle('btn-primary', featuredMode);
      featuredButton.classList.toggle('btn-secondary', !featuredMode);
      insertButton.hidden = featuredMode;
      showUsedImages();
      dialog.showModal();
      // 別の画面・タブで追加した画像も選べるよう、開くたびに読み直す。
      load(true);
      var firstUsed = usedSection.hidden ? null : usedGrid.querySelector('button');
      (firstUsed || queryInput).focus();
    });
  });
})();
