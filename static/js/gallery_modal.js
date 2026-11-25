// フォトギャラリーの作成・挿入: 記事編集画面から離れずに、メディアライブラリの写真を選んで（その場でアップロードもして）
// :::gallery を本文のカーソル位置に挿入する。タグで指定するときは :::gallery tag="..." を挿入し、公開時にそのタグの
// 写真へ展開させる（internal/markdown/gallery.go）。並べ方は layout="justified|masonry|cards"（既定 justified）で書く。
// 本文の :::gallery ブロックの中にカーソルを置いて開くと、そのブロックの並べ方・タグ・写真を読み戻し、挿入でそのブロックを置き換える。
// data-gallery-modal-open を付けたボタンで開く。
(function () {
  'use strict';

  var dialog = document.getElementById('gallery-modal');
  if (!dialog) return;
  function byId(id) { return document.getElementById(id); }

  var textarea = byId('content_markdown');
  var tabs = Array.prototype.slice.call(dialog.querySelectorAll('[data-gallery-tab]'));
  var panes = Array.prototype.slice.call(dialog.querySelectorAll('[data-gallery-pane]'));
  var notice = byId('gallery-modal-notice');
  // 写真を選んで作成（個別指定）
  var searchForm = byId('gallery-modal-search');
  var queryInput = byId('gallery-modal-q');
  var countEl = byId('gallery-modal-count');
  var grid = byId('gallery-modal-grid');
  var statusEl = byId('gallery-modal-status');
  var moreButton = byId('gallery-modal-more');
  var dropzone = byId('gallery-modal-dropzone');
  var dropLabel = byId('gallery-modal-drop-label');
  var fileInput = byId('gallery-modal-file');
  var tray = byId('gallery-tray');
  var trayEmpty = byId('gallery-tray-empty');
  var insertPick = byId('gallery-insert-pick');
  // タグで指定（動的自動更新）
  var chips = byId('gallery-tag-chips');
  var chipsStatus = byId('gallery-tag-chips-status');
  var tagForm = byId('gallery-tag-form');
  var tagInput = byId('gallery-tag-input');
  var tagOptions = byId('gallery-tag-options');
  var tagCurrent = byId('gallery-tag-current');
  var tagStatus = byId('gallery-tag-status');
  var tagPreview = byId('gallery-tag-preview');
  var insertDynamic = byId('gallery-insert-dynamic');
  var insertFixed = byId('gallery-insert-fixed');
  var limitSelect = byId('gallery-limit-select');
  var limitCustomWrap = byId('gallery-limit-custom-wrap');
  var limitCustom = byId('gallery-limit-custom');
  var sortSelect = byId('gallery-sort-select');
  // 並べ方（2つのタブで共通）
  var layoutInputs = Array.prototype.slice.call(dialog.querySelectorAll('input[name="gallery-layout"]'));
  var layoutHelp = byId('gallery-layout-help');

  // タグの写真の一覧で 1 回に読む枚数（API の上限）と、固定ギャラリーにするときに読むページ数の上限。
  var TAG_PER_PAGE = 60;
  var MAX_TAG_PAGES = 50;

  var defaultDropLabel = dropLabel.textContent;
  var state = {
    tab: 'pick',
    // 写真の一覧（検索語・読み込んだページ・古い応答を捨てるための番号）
    q: '', page: 0, hasNext: false, loading: false, request: 0, total: 0,
    // selected は選んだ写真（挿入する順）。alt・caption はここで編集した値。
    selected: [],
    // uploads はアップロードを待つファイル。写真の順を保ち、変換の負荷を抑えるため 1 枚ずつ送る。
    uploads: [], uploading: false, uploaded: 0,
    // tagsLoaded は開いてからタグの一覧を読んだか。tag は選んだタグ、tagTotal はそのタグの写真の数。
    tagsLoaded: false, tag: '', tagTotal: 0, tagRequest: 0, inserting: false,
    // layout は新しく挿入するときの並べ方（ページを開いている間は前回の選択を覚える）。
    // editing は読み戻した本文の :::gallery ブロック（galleryBlockAt の結果）。挿入でこの範囲を置き換える。
    // editedLast は前回、ブロックを読み戻して開いたか（新しく挿入するときは、読み戻した写真の選択を残さない）。
    layout: 'justified', editing: null, editedLast: false
  };

  function msg(name, n) {
    var text = dialog.getAttribute('data-msg-' + name) || '';
    return n === undefined ? text : text.split('{n}').join(String(n));
  }

  function canInsert() {
    return !!textarea && !textarea.readOnly;
  }

  function stem(name) {
    return String(name || '').replace(/\.[^.]*$/, '');
  }

  function showNotice(text, isError) {
    notice.textContent = text || '';
    notice.hidden = !text;
    notice.classList.toggle('text-rose-700', !!isError);
  }

  function getJSON(url) {
    return fetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' }).then(function (res) {
      if (!res.ok) throw new Error(msg('error'));
      return res.json();
    });
  }

  // --- タブ ---------------------------------------------------------------
  function selectTab(name, focus) {
    state.tab = name;
    tabs.forEach(function (tab) {
      var on = tab.getAttribute('data-gallery-tab') === name;
      tab.setAttribute('aria-selected', on ? 'true' : 'false');
      tab.tabIndex = on ? 0 : -1;
      if (on && focus) tab.focus();
    });
    panes.forEach(function (pane) { pane.hidden = pane.getAttribute('data-gallery-pane') !== name; });
    if (name === 'tag') loadTags();
  }
  tabs.forEach(function (tab, i) {
    tab.addEventListener('click', function () { selectTab(tab.getAttribute('data-gallery-tab')); });
    // 矢印キー・Home・End でタブを移る（WAI-ARIA のタブの操作）。
    tab.addEventListener('keydown', function (e) {
      var to = null;
      if (e.key === 'ArrowRight') to = tabs[(i + 1) % tabs.length];
      else if (e.key === 'ArrowLeft') to = tabs[(i - 1 + tabs.length) % tabs.length];
      else if (e.key === 'Home') to = tabs[0];
      else if (e.key === 'End') to = tabs[tabs.length - 1];
      if (!to) return;
      e.preventDefault();
      selectTab(to.getAttribute('data-gallery-tab'), true);
    });
  });

  // --- 写真の一覧（個別指定） -------------------------------------------------
  // indexOfSelected は選んだ写真のうち、id（または URL）が同じものの位置を返す。本文から読み戻した写真には
  // メディアの id が無いため、一覧の写真とは URL で照らし合わせる。
  function indexOfSelected(id, url) {
    for (var i = 0; i < state.selected.length; i++) {
      if (state.selected[i].id === id || (url && state.selected[i].url === url)) return i;
    }
    return -1;
  }

  function toggle(item) {
    var i = indexOfSelected(item.id, item.url);
    if (i >= 0) {
      state.selected.splice(i, 1);
    } else {
      state.selected.push({
        id: item.id,
        url: item.url,
        thumbnail_url: item.thumbnail_url || item.url,
        filename: item.filename || '',
        alt: item.alt || stem(item.filename),
        caption: item.caption || ''
      });
    }
    renderTray();
  }

  function renderGridItem(item) {
    var li = document.createElement('li');
    li.setAttribute('data-gallery-item', item.id);
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'media-picker-item relative block w-full overflow-hidden rounded-lg border border-neutral-200 bg-white text-left hover:border-zinc-400';
    b.setAttribute('aria-pressed', 'false');
    b.setAttribute('data-media-id', item.id);
    b.setAttribute('data-media-url', item.url || '');
    b.title = item.filename || item.alt || '';
    var frame = document.createElement('span');
    frame.className = 'block aspect-square bg-neutral-100';
    var img = document.createElement('img');
    img.src = item.thumbnail_url || item.url;
    img.alt = item.alt || '';
    img.loading = 'lazy';
    img.className = 'h-full w-full object-cover';
    frame.appendChild(img);
    // 選んだ順の番号。並べ替えると振り直す（syncGrid）。
    var badge = document.createElement('span');
    badge.className = 'gallery-pick-badge';
    badge.setAttribute('aria-hidden', 'true');
    badge.hidden = true;
    var label = document.createElement('span');
    label.className = 'block truncate px-1.5 py-1 text-[11px] text-neutral-600';
    label.textContent = item.alt || item.filename || '';
    b.appendChild(frame);
    b.appendChild(badge);
    b.appendChild(label);
    b.addEventListener('click', function () { toggle(item); });
    li.appendChild(b);
    return li;
  }

  // syncGrid は一覧の各写真に、選んでいるか（aria-pressed）と選んだ順の番号を反映する。
  function syncGrid() {
    grid.querySelectorAll('[data-media-id]').forEach(function (b) {
      var i = indexOfSelected(b.getAttribute('data-media-id'), b.getAttribute('data-media-url'));
      b.setAttribute('aria-pressed', i >= 0 ? 'true' : 'false');
      var badge = b.querySelector('.gallery-pick-badge');
      badge.hidden = i < 0;
      badge.textContent = i >= 0 ? String(i + 1) : '';
    });
  }

  // load は検索語 state.q の次のページを読み、一覧の末尾に足す。reset なら一覧を作り直す（選んだ写真はそのまま）。
  function load(reset) {
    if (reset) {
      state.page = 0;
      state.hasNext = false;
      grid.textContent = '';
    }
    var req = ++state.request;
    state.loading = true;
    moreButton.hidden = true;
    statusEl.textContent = msg('loading');
    var params = new URLSearchParams({ page: String(state.page + 1) });
    if (state.q) params.set('q', state.q);
    getJSON(dialog.getAttribute('data-api') + '?' + params.toString()).then(function (json) {
      if (req !== state.request) return;
      state.page = json.page;
      state.hasNext = json.has_next;
      (json.items || []).forEach(function (item) {
        if (!grid.querySelector('[data-gallery-item="' + cssEscape(item.id) + '"]')) grid.appendChild(renderGridItem(item));
      });
      syncGrid();
      state.total = json.total;
      countEl.textContent = msg('count', state.total);
      statusEl.textContent = grid.children.length === 0 ? msg('empty') : '';
      moreButton.hidden = !state.hasNext;
    }).catch(function (err) {
      if (req !== state.request) return;
      statusEl.textContent = (err && err.message) || msg('error');
    }).finally(function () {
      if (req === state.request) state.loading = false;
    });
  }

  function cssEscape(s) {
    return window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, '\\$&');
  }

  // --- 選んだ写真（トレイ） ---------------------------------------------------
  function trayButton(text, label, action, disabled) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'rounded-md border border-neutral-300 bg-white px-2 py-0.5 text-xs text-neutral-700 hover:bg-neutral-100 disabled:cursor-not-allowed disabled:opacity-40';
    b.textContent = text;
    b.setAttribute('aria-label', label + ' ' + text);
    b.setAttribute('data-tray-action', action);
    b.disabled = disabled;
    return b;
  }

  function trayField(item, key, labelText, maxLength) {
    var wrap = document.createElement('div');
    var id = 'gallery-tray-' + key + '-' + item.id;
    var label = document.createElement('label');
    label.htmlFor = id;
    label.className = 'block text-[11px] font-medium text-neutral-500';
    label.textContent = labelText;
    var input = document.createElement('input');
    input.type = 'text';
    input.id = id;
    input.maxLength = maxLength;
    input.value = item[key];
    input.className = 'w-full rounded-md border border-neutral-300 bg-white px-2 py-1 text-xs focus:border-zinc-500 focus:outline-none';
    input.setAttribute('data-tray-field', key);
    input.addEventListener('input', function () { item[key] = input.value; });
    wrap.appendChild(label);
    wrap.appendChild(input);
    return wrap;
  }

  // renderTray は選んだ写真の一覧を作り直す。focus（{id, action}）を渡すと、操作したボタンにフォーカスを戻す。
  function renderTray(focus) {
    tray.textContent = '';
    var n = state.selected.length;
    state.selected.forEach(function (item, i) {
      var li = document.createElement('li');
      li.className = 'flex gap-2 rounded-lg border border-neutral-200 bg-white p-2';
      li.setAttribute('data-tray-id', item.id);
      var img = document.createElement('img');
      img.src = item.thumbnail_url;
      img.alt = '';
      img.className = 'h-16 w-16 flex-shrink-0 rounded-md bg-neutral-100 object-cover';
      var body = document.createElement('div');
      body.className = 'flex min-w-0 flex-1 flex-col gap-1';
      var head = document.createElement('div');
      head.className = 'flex flex-wrap items-center justify-between gap-1';
      var label = msg('item', i + 1);
      var num = document.createElement('span');
      num.className = 'text-xs font-semibold text-neutral-700';
      num.textContent = label;
      var actions = document.createElement('span');
      actions.className = 'flex gap-1';
      actions.appendChild(trayButton(msg('prev'), label, 'prev', i === 0));
      actions.appendChild(trayButton(msg('next'), label, 'next', i === n - 1));
      actions.appendChild(trayButton(msg('remove'), label, 'remove', false));
      head.appendChild(num);
      head.appendChild(actions);
      body.appendChild(head);
      body.appendChild(trayField(item, 'alt', msg('alt'), 300));
      body.appendChild(trayField(item, 'caption', msg('caption'), 1000));
      li.appendChild(img);
      li.appendChild(body);
      tray.appendChild(li);
    });
    trayEmpty.hidden = n > 0;
    tray.hidden = n === 0;
    insertPick.textContent = msg('insert-pick', n);
    insertPick.disabled = n === 0 || !canInsert();
    syncGrid();
    if (focus) restoreTrayFocus(focus);
  }

  function restoreTrayFocus(focus) {
    var li = tray.querySelector('[data-tray-id="' + cssEscape(focus.id) + '"]');
    if (!li) {
      // 外した写真の位置にあった写真（無ければ最後の写真）の「外す」へ。写真が無くなったら検索欄へ。
      var items = tray.querySelectorAll('[data-tray-action="remove"]');
      var next = items[Math.min(focus.index, items.length - 1)];
      if (next) next.focus();
      else queryInput.focus();
      return;
    }
    var b = li.querySelector('[data-tray-action="' + focus.action + '"]');
    if (b && b.disabled) b = li.querySelector('[data-tray-action="' + (focus.action === 'prev' ? 'next' : 'prev') + '"]');
    if (b && !b.disabled) b.focus();
  }

  tray.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-tray-action]');
    if (!b) return;
    var li = b.closest('[data-tray-id]');
    var i = indexOfSelected(li.getAttribute('data-tray-id'));
    if (i < 0) return;
    var action = b.getAttribute('data-tray-action');
    var item = state.selected[i];
    if (action === 'remove') {
      state.selected.splice(i, 1);
    } else {
      var j = action === 'prev' ? i - 1 : i + 1;
      if (j < 0 || j >= state.selected.length) return;
      state.selected[i] = state.selected[j];
      state.selected[j] = item;
    }
    renderTray({ id: item.id, action: action, index: i });
  });

  // --- Markdown の組み立てと挿入 ------------------------------------------------
  // 代替テキストは Markdown の画像構文、キャプションは画像のタイトル（"..."）を壊す文字と改行を除く（handlers_dam.go と同じ）。
  function cleanAlt(s) {
    return String(s || '').replace(/[[\]\\]/g, '').replace(/[\r\n]+/g, ' ').trim();
  }
  function cleanCaption(s) {
    return String(s || '').replace(/["\\]/g, '').replace(/[\r\n]+/g, ' ').trim();
  }
  function imageLine(item) {
    var alt = cleanAlt(item.alt) || cleanAlt(stem(item.filename));
    var caption = cleanCaption(item.caption);
    return '![' + alt + '](' + item.url + (caption ? ' "' + caption + '"' : '') + ')';
  }
  function staticGallery(items) {
    return ':::gallery' + layoutAttr() + '\n' + items.map(imageLine).join('\n') + '\n:::';
  }
  // validTag はタグを :::gallery tag="..." に書けるか（" と改行は書けない）を返す。
  function validTag(tag) {
    return tag !== '' && !/["\r\n]/.test(tag);
  }

  // savedRange はモーダルを開いたときの本文のカーソル位置（[開始, 終了, スクロール位置]）。モーダルを閉じてフォーカスが
  // 本文に戻ると、ブラウザによってはカーソルが先頭に戻るため、開く前の位置を覚えておいて挿入と復元に使う。
  var savedRange = null;
  function saveRange() {
    savedRange = textarea ? [textarea.selectionStart, textarea.selectionEnd, textarea.scrollTop] : null;
  }

  // insertBlock は本文のカーソル位置（選択範囲があれば置き換え）に、前後を空行で区切った独立したブロックとして text を入れ、
  // すぐに input を発火してプレビューを更新させる。
  function insertBlock(text) {
    var len = textarea.value.length;
    var start = savedRange ? Math.min(savedRange[0], len) : textarea.selectionStart;
    var end = savedRange ? Math.min(Math.max(savedRange[1], start), len) : textarea.selectionEnd;
    savedRange = null;
    var before = textarea.value.slice(0, start);
    var after = textarea.value.slice(end);
    var prefix = before === '' || /\n\n$/.test(before) ? '' : (/\n$/.test(before) ? '\n' : '\n\n');
    var suffix = after === '' || /^\n/.test(after) ? '\n' : '\n\n';
    textarea.setRangeText(prefix + text + suffix, start, end, 'end');
    textarea.focus();
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
  }

  // replaceEditedBlock は読み戻した :::gallery ブロックを text に置き換える。開いてから本文のその範囲が変わっていたら
  // （別の操作で書き換わったなど）置き換えず false を返し、呼び出し側はカーソル位置への挿入にする。
  function replaceEditedBlock(text) {
    var block = state.editing;
    state.editing = null;
    if (!block || textarea.value.slice(block.start, block.end) !== block.source) return false;
    savedRange = null;
    textarea.setRangeText(text, block.start, block.end, 'end');
    textarea.focus();
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    return true;
  }

  function finishInsert(text, message) {
    dialog.close();
    if (!replaceEditedBlock(text)) insertBlock(text);
    if (window.cmsToast) window.cmsToast(message, 'success');
  }

  insertPick.addEventListener('click', function () {
    if (state.selected.length === 0 || !canInsert()) return;
    var text = staticGallery(state.selected);
    state.selected = [];
    renderTray();
    finishInsert(text, msg('inserted'));
  });

  // --- アップロード（モーダル内のドラッグ＆ドロップ・ファイル選択） ----------------------
  function updateUploadLabel() {
    var left = state.uploads.length + (state.uploading ? 1 : 0);
    dropLabel.textContent = left > 0 ? msg('uploading', left) : defaultDropLabel;
    dropzone.setAttribute('aria-busy', left > 0 ? 'true' : 'false');
  }

  function enqueue(files) {
    var rejected = false;
    Array.prototype.forEach.call(files || [], function (file) {
      if (/^image\/(jpeg|png|webp)$/.test(file.type)) state.uploads.push(file);
      else rejected = true;
    });
    if (rejected) showNotice(msg('upload-type'), true);
    updateUploadLabel();
    if (!state.uploading) uploadNext();
  }

  function uploadNext() {
    var file = state.uploads.shift();
    if (!file) {
      state.uploading = false;
      updateUploadLabel();
      if (state.uploaded > 0) showNotice(msg('uploaded'), false);
      state.uploaded = 0;
      return;
    }
    state.uploading = true;
    updateUploadLabel();
    var data = new FormData();
    data.append('file', file, file.name || 'image.png');
    fetch(dialog.getAttribute('data-upload'), {
      method: 'POST',
      headers: { 'X-CSRF-Token': window.cmsCSRFToken ? window.cmsCSRFToken() : '', 'Accept': 'application/json' },
      body: data,
      credentials: 'same-origin'
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (json) {
        if (!res.ok || !json.id) throw new Error(json.message || msg('upload-error'));
        return json;
      });
    }).then(function (json) {
      addUploaded({
        id: json.id,
        url: json.url,
        thumbnail_url: json.small_url || json.medium_url || json.url,
        filename: file.name || '',
        alt: json.alt || '',
        caption: json.caption || ''
      });
      state.uploaded++;
    }).catch(function (err) {
      showNotice(((file.name ? file.name + ': ' : '') + ((err && err.message) || msg('upload-error'))), true);
    }).finally(uploadNext);
  }

  // addUploaded はアップロードした写真を一覧の先頭に出し、選んだ写真の最後に加える。
  function addUploaded(item) {
    var old = grid.querySelector('[data-gallery-item="' + cssEscape(item.id) + '"]');
    if (old) {
      old.remove();
    } else {
      state.total++;
      countEl.textContent = msg('count', state.total);
    }
    grid.insertBefore(renderGridItem(item), grid.firstChild);
    statusEl.textContent = '';
    if (indexOfSelected(item.id, item.url) < 0) toggle(item);
    else syncGrid();
  }

  fileInput.addEventListener('change', function () {
    enqueue(fileInput.files);
    fileInput.value = '';
  });

  function hasFiles(e) {
    return !!e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], 'Files') >= 0;
  }
  // モーダルのどこに落としても受け取る（ブラウザがファイルを開いて編集中の画面から離れないようにする）。
  dialog.addEventListener('dragover', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dropzone.classList.add('is-dragover');
  });
  dialog.addEventListener('dragleave', function (e) {
    if (!e.relatedTarget || !dialog.contains(e.relatedTarget)) dropzone.classList.remove('is-dragover');
  });
  dialog.addEventListener('drop', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dropzone.classList.remove('is-dragover');
    if (state.tab !== 'pick') selectTab('pick');
    enqueue(e.dataTransfer.files);
  });

  searchForm.addEventListener('submit', function (e) {
    e.preventDefault();
    state.q = queryInput.value.trim();
    load(true);
  });
  moreButton.addEventListener('click', function () { if (!state.loading && state.hasNext) load(false); });

  // --- タグで指定（動的自動更新） ---------------------------------------------------
  function loadTags() {
    if (state.tagsLoaded) return;
    state.tagsLoaded = true;
    chipsStatus.textContent = msg('loading');
    getJSON(dialog.getAttribute('data-tags-api')).then(function (json) {
      var tags = json.tags || [];
      chips.textContent = '';
      tagOptions.textContent = '';
      tags.forEach(function (tag) {
        var li = document.createElement('li');
        var b = document.createElement('button');
        b.type = 'button';
        b.className = 'rounded-full border border-neutral-300 bg-white px-3 py-1 text-sm text-neutral-700 hover:border-neutral-500 aria-pressed:border-neutral-900 aria-pressed:bg-neutral-900 aria-pressed:text-white';
        b.textContent = tag;
        b.setAttribute('data-tag', tag);
        b.setAttribute('aria-pressed', tag === state.tag ? 'true' : 'false');
        b.addEventListener('click', function () { chooseTag(tag); });
        li.appendChild(b);
        chips.appendChild(li);
        var option = document.createElement('option');
        option.value = tag;
        tagOptions.appendChild(option);
      });
      chipsStatus.textContent = tags.length === 0 ? msg('tag-none') : '';
    }).catch(function (err) {
      state.tagsLoaded = false;
      chipsStatus.textContent = (err && err.message) || msg('error');
    });
  }

  function setFixedLabel(n) {
    insertFixed.textContent = msg('insert-fixed', n);
    insertFixed.disabled = n === 0 || !canInsert() || state.inserting;
  }

  // setTag はタグを選んだ状態にする（写真の一覧は読まない）。chooseTag は選んで、そのタグの写真を読む。
  function setTag(tag) {
    state.tag = tag;
    tagInput.value = tag;
    chips.querySelectorAll('[data-tag]').forEach(function (b) {
      b.setAttribute('aria-pressed', b.getAttribute('data-tag') === tag ? 'true' : 'false');
    });
    tagCurrent.textContent = tag;
    tagCurrent.hidden = tag === '';
    updateDynamicButton();
  }

  function chooseTag(tag) {
    setTag(tag);
    loadTagPreview();
  }

  function selectedLimit() {
    if (limitSelect.value === 'all') return 0;
    var value = limitSelect.value === 'custom' ? Number(limitCustom.value) : Number(limitSelect.value);
    return Number.isInteger(value) && value >= 1 && value <= 100 ? value : -1;
  }

  function updateDynamicButton() {
    var limit = selectedLimit();
    limitCustomWrap.hidden = limitSelect.value !== 'custom';
    insertDynamic.disabled = !validTag(state.tag) || !canInsert() || limit < 0;
  }
  limitSelect.addEventListener('change', updateDynamicButton);
  limitCustom.addEventListener('input', updateDynamicButton);
  sortSelect.addEventListener('change', updateDynamicButton);

  // loadTagPreview は選んだタグの写真（新しい順・最大 TAG_PER_PAGE 枚）を一覧し、いまの枚数を知らせる。
  function loadTagPreview() {
    var tag = state.tag;
    var req = ++state.tagRequest;
    tagPreview.textContent = '';
    state.tagTotal = 0;
    setFixedLabel(0);
    if (tag === '') {
      tagStatus.textContent = msg('tag-prompt');
      return;
    }
    tagStatus.textContent = msg('loading');
    var params = new URLSearchParams({ tag: tag, per_page: String(TAG_PER_PAGE) });
    getJSON(dialog.getAttribute('data-api') + '?' + params.toString()).then(function (json) {
      if (req !== state.tagRequest) return;
      (json.items || []).forEach(function (item) {
        var li = document.createElement('li');
        li.className = 'aspect-square overflow-hidden rounded-md border border-neutral-200 bg-neutral-100';
        var img = document.createElement('img');
        img.src = item.thumbnail_url || item.url;
        img.alt = item.alt || '';
        img.title = item.caption || item.alt || item.filename || '';
        img.loading = 'lazy';
        img.className = 'h-full w-full object-cover';
        li.appendChild(img);
        tagPreview.appendChild(li);
      });
      state.tagTotal = json.total || 0;
      var text = state.tagTotal === 0 ? msg('tag-empty') : msg('tag-count', state.tagTotal);
      if (json.has_next) text += ' ' + msg('tag-truncated', TAG_PER_PAGE);
      if (!validTag(tag)) text += ' ' + msg('tag-invalid');
      tagStatus.textContent = text;
      setFixedLabel(state.tagTotal);
    }).catch(function (err) {
      if (req !== state.tagRequest) return;
      tagStatus.textContent = (err && err.message) || msg('error');
    });
  }

  tagForm.addEventListener('submit', function (e) {
    e.preventDefault();
    // タグはカンマ区切りで保存されるため、1つのタグにカンマは入らない。改行も除く。
    chooseTag(tagInput.value.replace(/[,，、\r\n]/g, '').trim());
  });

  insertDynamic.addEventListener('click', function () {
    var limit = selectedLimit();
    if (!validTag(state.tag) || !canInsert() || limit < 0) return;
    var attrs = ' tag="' + state.tag + '"';
    if (limit > 0) attrs += ' limit="' + limit + '"';
    attrs += ' sort="' + sortSelect.value + '"';
    attrs += layoutAttr();
    finishInsert(':::gallery' + attrs + '\n:::', msg('inserted-dynamic'));
  });

  // fetchAllTagged はタグの写真をすべて（新しい順。公開時の動的ギャラリーと同じ順）読む。
  function fetchAllTagged(tag) {
    var items = [];
    function page(n) {
      var params = new URLSearchParams({ tag: tag, page: String(n), per_page: String(TAG_PER_PAGE) });
      return getJSON(dialog.getAttribute('data-api') + '?' + params.toString()).then(function (json) {
        items = items.concat(json.items || []);
        return json.has_next && n < MAX_TAG_PAGES ? page(n + 1) : items;
      });
    }
    return page(1);
  }

  insertFixed.addEventListener('click', function () {
    var tag = state.tag;
    if (tag === '' || state.tagTotal === 0 || !canInsert() || state.inserting) return;
    state.inserting = true;
    insertFixed.disabled = true;
    fetchAllTagged(tag).then(function (items) {
      items = items.filter(function (item) { return item.url; });
      if (items.length === 0) throw new Error(msg('tag-empty'));
      finishInsert(staticGallery(items), msg('inserted'));
    }).catch(function (err) {
      tagStatus.textContent = (err && err.message) || msg('error');
    }).finally(function () {
      state.inserting = false;
      setFixedLabel(state.tagTotal);
    });
  });

  // --- 並べ方 ---------------------------------------------------------------
  var LAYOUTS = ['justified', 'masonry', 'cards'];
  function normalizeLayout(value) {
    return LAYOUTS.indexOf(value) >= 0 ? value : LAYOUTS[0];
  }
  function selectedLayout() {
    for (var i = 0; i < layoutInputs.length; i++) {
      if (layoutInputs[i].checked) return normalizeLayout(layoutInputs[i].value);
    }
    return LAYOUTS[0];
  }
  function setLayout(value) {
    value = normalizeLayout(value);
    layoutInputs.forEach(function (input) { input.checked = input.value === value; });
    syncLayoutHelp();
  }
  // syncLayoutHelp は狭い画面向けに、選んでいる並べ方の説明（各カードの説明と同じ文）を下に出す。
  function syncLayoutHelp() {
    var desc = byId('gallery-layout-desc-' + selectedLayout());
    if (layoutHelp) layoutHelp.textContent = desc ? desc.textContent : '';
  }
  // layoutAttr は :::gallery に書く layout 属性。既定の justified も書き、記事の Markdown から並べ方が分かるようにする。
  function layoutAttr() {
    return ' layout="' + selectedLayout() + '"';
  }
  layoutInputs.forEach(function (input) {
    input.addEventListener('change', function () {
      if (!state.editing) state.layout = selectedLayout();
      syncLayoutHelp();
    });
  });

  // --- 本文の :::gallery ブロックの読み戻し -----------------------------------------
  // 開き・閉じの行の形は internal/markdown/gallery.go と同じ（行頭の空白は 3 つまで、属性は name="value" の並び）。
  var OPEN_RE = /^ {0,3}:::gallery((?:\s+[A-Za-z][A-Za-z0-9_-]*\s*=\s*"[^"]*")*)\s*$/;
  var CLOSE_RE = /^ {0,3}:::\s*$/;
  var IMAGE_RE = /!\[((?:[^\]\\]|\\.)*)\]\(\s*<?([^\s)>]+)>?(?:\s+"((?:[^"\\]|\\.)*)")?\s*\)/g;

  function galleryAttr(attrs, name) {
    var m = new RegExp('(?:^|\\s)' + name + '\\s*=\\s*"([^"]*)"').exec(attrs);
    return m ? m[1].trim() : '';
  }
  function unescapeMarkdown(s) {
    return String(s || '').replace(/\\(.)/g, '$1');
  }

  // galleryBlockAt は本文 text の位置 pos を含む :::gallery ... ::: のブロックを探し、
  // { start, end（閉じの ::: の行末）, source, attrs, body } を返す。カーソルがブロックの外なら null。
  function galleryBlockAt(text, pos) {
    var lines = text.split('\n');
    var offsets = [];
    var offset = 0;
    for (var i = 0; i < lines.length; i++) {
      offsets.push(offset);
      offset += lines[i].length + 1;
    }
    var at = 0;
    while (at + 1 < lines.length && offsets[at + 1] <= pos) at++;
    for (var open = at; open >= 0; open--) {
      var m = OPEN_RE.exec(lines[open]);
      if (m) {
        for (var close = open + 1; close < lines.length; close++) {
          if (!CLOSE_RE.test(lines[close])) continue;
          if (close < at) return null;
          var start = offsets[open];
          var end = offsets[close] + lines[close].length;
          return { start: start, end: end, source: text.slice(start, end), attrs: m[1], body: lines.slice(open + 1, close).join('\n') };
        }
        return null; // 閉じの ::: が無い :::gallery はギャラリーではない
      }
      if (open !== at && CLOSE_RE.test(lines[open])) return null; // 閉じたブロックより後ろにいる
    }
    return null;
  }

  // imagesIn はブロックの中の画像 ![alt](url "caption") を、選んだ写真（トレイ）の形にする。
  function imagesIn(body) {
    var items = [];
    var m;
    IMAGE_RE.lastIndex = 0;
    while ((m = IMAGE_RE.exec(body)) !== null) {
      items.push({
        id: 'markdown:' + items.length + ':' + m[2],
        url: m[2],
        thumbnail_url: m[2],
        filename: '',
        alt: unescapeMarkdown(m[1]),
        caption: unescapeMarkdown(m[3])
      });
    }
    return items;
  }

  // editableBlock は本文のカーソル（選択範囲）を含む :::gallery ブロックを返す。選択範囲がブロックからはみ出すなら null。
  function editableBlock() {
    if (!canInsert()) return null;
    var block = galleryBlockAt(textarea.value, textarea.selectionStart);
    return block && textarea.selectionEnd <= block.end ? block : null;
  }

  // applyBlock は読み戻したブロックの並べ方・タグの条件（または写真）をモーダルに入れる。
  function applyBlock(block) {
    state.editing = block;
    state.editedLast = true;
    setLayout(galleryAttr(block.attrs, 'layout'));
    var tag = galleryAttr(block.attrs, 'tag');
    if (tag) {
      var limit = parseInt(galleryAttr(block.attrs, 'limit'), 10);
      if (!(limit >= 1)) {
        limitSelect.value = 'all';
      } else {
        limit = Math.min(limit, 100);
        var preset = ['4', '6', '8', '12'].indexOf(String(limit)) >= 0;
        limitSelect.value = preset ? String(limit) : 'custom';
        if (!preset) limitCustom.value = String(limit);
      }
      var sort = galleryAttr(block.attrs, 'sort');
      sortSelect.value = ['latest', 'oldest', 'random'].indexOf(sort) >= 0 ? sort : 'latest';
      setTag(tag);
      selectTab('tag');
    } else {
      state.selected = imagesIn(block.body);
      renderTray();
      selectTab('pick');
    }
  }

  // startFresh は新しく挿入するときの状態にする（前回ブロックを読み戻していたら、その写真の選択は外す）。
  function startFresh() {
    state.editing = null;
    if (state.editedLast) {
      state.selected = [];
      renderTray();
    }
    state.editedLast = false;
    setLayout(state.layout);
  }

  // --- 開閉 ---------------------------------------------------------------
  dialog.querySelectorAll('[data-gallery-modal-close]').forEach(function (b) {
    b.addEventListener('click', function () { dialog.close(); });
  });
  // 外側（背景）を押すと閉じる。入力欄で文字を選びながら外側で離したときは閉じない。Esc はブラウザの dialog の標準の動作で閉じる。
  var pressedOutside = false;
  dialog.addEventListener('pointerdown', function (e) { pressedOutside = e.target === dialog; });
  dialog.addEventListener('click', function (e) {
    if (e.target === dialog && pressedOutside) dialog.close();
    pressedOutside = false;
  });
  dialog.addEventListener('close', function () {
    dropzone.classList.remove('is-dragover');
    // 挿入せずに閉じたときは、本文のカーソル位置を開く前に戻す。
    if (savedRange && textarea && document.activeElement === textarea) {
      textarea.setSelectionRange(savedRange[0], savedRange[1]);
      textarea.scrollTop = savedRange[2];
    }
    savedRange = null;
    state.editing = null;
  });

  document.querySelectorAll('[data-gallery-modal-open]').forEach(function (b) {
    b.addEventListener('click', function () {
      if (dialog.open) return;
      showNotice('');
      saveRange();
      var block = editableBlock();
      dialog.showModal();
      // 別の画面・タブで追加した写真やタグも選べるよう、開くたびに読み直す。選んだ写真は挿入するまで残す。
      state.tagsLoaded = false;
      load(true);
      if (block) applyBlock(block);
      else startFresh();
      if (state.tab === 'tag') {
        loadTags();
        if (state.tag) loadTagPreview();
        tagInput.focus();
      } else {
        queryInput.focus();
      }
    });
  });

  renderTray();
  syncLayoutHelp();
})();
