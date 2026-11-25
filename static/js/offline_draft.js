// 記事フォームの入力を端末内へ退避し、通信断や誤離脱のあとに復元できるようにする。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };

  var form = document.getElementById('article-form');
  if (!form || form.dataset.readOnly === 'true') return;

  var articleId = form.dataset.articleId;
  var key = articleId ? 'sorus:draft:' + articleId : 'sorus:draft:new';
  var savedDraftToken = form.dataset.savedDraftToken;
  var fields = ['title', 'slug', 'kind', 'category_id', 'content_markdown', 'excerpt'];
  var status = document.getElementById('offline-save-status');
  var mobileStatus = document.getElementById('offline-save-status-mobile');
  var banner = document.getElementById('offline-draft-banner');
  var timer;
  var restoredDraft = null;
  var submitting = false;
  var current = readFields();
  var pendingChoice = false;
  var baseUpdatedAt = Number(form.dataset.updatedAt) || 0;
  if (savedDraftToken) {
    try {
      Object.keys(localStorage).forEach(function (k) {
        if (k.indexOf('sorus:draft:') !== 0) return;
        try { var saved = JSON.parse(localStorage.getItem(k) || 'null'); if (saved && saved.token === savedDraftToken) localStorage.removeItem(k); } catch (_) {}
      });
    } catch (_) {}
  }
  function message(name, fallback) { return form.dataset['msg' + name] || fallback; }
  function clearAllDrafts() { try { Object.keys(localStorage).forEach(function(k) { if (k.indexOf('sorus:draft:') === 0) localStorage.removeItem(k); }); } catch (_) {} }

  function setStatus(message) {
    if (status) status.textContent = message;
    if (mobileStatus) mobileStatus.textContent = message;
  }

  function readFields() {
    var values = {};
    fields.forEach(function (name) {
      var field = form.elements.namedItem(name);
      if (field && typeof field.value === 'string') values[name] = field.value;
    });
    return values;
  }

  function saveDraft() {
    var values = readFields();
    if (pendingChoice || submitting) return;
    var dirty = fields.some(function(name) { return values[name] !== current[name]; });
    if (!dirty) { try { localStorage.removeItem(key); } catch (_) {} setStatus(''); return; }
    var old = readDraft();
    values.token = token;
    values.savedAt = Date.now();
    values.baseUpdatedAt = old && Number.isFinite(Number(old.baseUpdatedAt)) ? Number(old.baseUpdatedAt) : baseUpdatedAt;
    try {
      window.localStorage.setItem(key, JSON.stringify(values));
      setStatus(message('Saved', T('offline_status_saved')));
    } catch (_) {
      setStatus(message('Unavailable', T('offline_status_unavailable')));
    }
  }

  function scheduleSave() {
    if (pendingChoice) return;
    setStatus(message('Saving', T('offline_status_saving')));
    window.clearTimeout(timer);
    timer = window.setTimeout(saveDraft, 1000);
  }

  function readDraft() {
    try {
      var raw = window.localStorage.getItem(key);
      if (!raw) return null;
      var draft = JSON.parse(raw);
      return draft && typeof draft === 'object' && Number.isFinite(Number(draft.savedAt)) ? draft : null;
    } catch (_) {
      return null;
    }
  }

  function showRestoreBanner(draft) {
    restoredDraft = draft;
    var time = document.getElementById('offline-draft-time');
    if (time) time.textContent = new Date(Number(draft.savedAt)).toLocaleString();
    renderDiff();
    if (banner) banner.classList.remove('hidden');
  }

  // ---- 差分の表示: 現在のフォーム値（サーバーから読んだ内容）と端末の下書きを比べる ----
  // 文言はバナーの data-msg-* / data-label-* 属性（テンプレートが辞書から埋め込む）から読む。
  // 下書きは利用者の入力なので、DOM へは必ず textContent で入れる（HTML 文字列として解釈させない）。
  var diffToggle = document.getElementById('offline-draft-diff-toggle');
  var diffBox = document.getElementById('offline-draft-diff');
  var diffFields = ['title', 'slug', 'excerpt', 'kind', 'category_id', 'content_markdown'];

  function bannerText(attr, fallback) {
    var value = banner && banner.dataset ? banner.dataset[attr] : '';
    return value || fallback;
  }
  function format(text) {
    var args = Array.prototype.slice.call(arguments, 1);
    return String(text).replace(/%s/g, function () { return args.length ? String(args.shift()) : ''; });
  }
  function camel(name) { return name.replace(/_([a-z])/g, function (_, c) { return c.toUpperCase(); }); }
  function fieldLabel(name) {
    var attr = camel('label_' + name);
    return bannerText(attr, name);
  }
  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }
  function str(value) { return typeof value === 'string' ? value : ''; }
  function normalizeNewlines(value) { return str(value).replace(/\r\n?/g, '\n'); }
  function charCount(value) { return Array.from(str(value)).length; }

  // displayValue は、選択肢（種類・カテゴリ）なら値ではなく画面に出ている名前を返す。
  function displayValue(name, value) {
    var field = form.elements.namedItem(name);
    if (field && field.tagName === 'SELECT') {
      for (var i = 0; i < field.options.length; i++) {
        if (field.options[i].value === value) return field.options[i].text.trim();
      }
    }
    return value;
  }

  // lineDiff は行単位の差分を [{op: ' ' | '-' | '+', text}] で返す。前後の共通行を除いた残りを LCS で比べ、
  // 大きすぎる場合は「消える行のあとに加わる行」をまとめて示す（端末の負荷を抑える）。
  function lineDiff(before, after) {
    var a = normalizeNewlines(before).split('\n');
    var b = normalizeNewlines(after).split('\n');
    var start = 0;
    while (start < a.length && start < b.length && a[start] === b[start]) start++;
    var endA = a.length, endB = b.length;
    while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA--; endB--; }
    var ops = [];
    var i;
    for (i = 0; i < start; i++) ops.push({ op: ' ', text: a[i] });
    var midA = a.slice(start, endA), midB = b.slice(start, endB);
    var n = midA.length, m = midB.length;
    if (n * m > 1000000) {
      midA.forEach(function (line) { ops.push({ op: '-', text: line }); });
      midB.forEach(function (line) { ops.push({ op: '+', text: line }); });
    } else {
      var width = m + 1;
      var table = new Uint32Array((n + 1) * width);
      for (var x = n - 1; x >= 0; x--) {
        for (var y = m - 1; y >= 0; y--) {
          table[x * width + y] = midA[x] === midB[y] ? table[(x + 1) * width + y + 1] + 1 : Math.max(table[(x + 1) * width + y], table[x * width + y + 1]);
        }
      }
      var p = 0, q = 0;
      while (p < n && q < m) {
        if (midA[p] === midB[q]) { ops.push({ op: ' ', text: midA[p] }); p++; q++; }
        else if (table[(p + 1) * width + q] >= table[p * width + q + 1]) { ops.push({ op: '-', text: midA[p] }); p++; }
        else { ops.push({ op: '+', text: midB[q] }); q++; }
      }
      for (; p < n; p++) ops.push({ op: '-', text: midA[p] });
      for (; q < m; q++) ops.push({ op: '+', text: midB[q] });
    }
    for (i = endA; i < a.length; i++) ops.push({ op: ' ', text: a[i] });
    return ops;
  }

  // collapseContext は、変更行の前後 context 行だけを残し、長い変更なし区間を {skip: 行数} にまとめる。
  function collapseContext(ops, context) {
    var keep = ops.map(function () { return false; });
    ops.forEach(function (item, index) {
      if (item.op === ' ') return;
      for (var k = Math.max(0, index - context); k <= Math.min(ops.length - 1, index + context); k++) keep[k] = true;
    });
    var out = [];
    var skipped = [];
    // 省く行が 1 行だけなら、目印に置き換えても短くならないのでそのまま見せる。
    function flush() {
      if (skipped.length === 1) out.push(skipped[0]);
      else if (skipped.length) out.push({ skip: skipped.length });
      skipped = [];
    }
    ops.forEach(function (item, index) {
      if (keep[index]) { flush(); out.push(item); } else { skipped.push(item); }
    });
    flush();
    return out;
  }

  function valueBox(heading, value, tone) {
    var box = el('div', 'notice ' + (tone === 'local' ? 'notice-success' : 'notice-neutral'));
    box.appendChild(el('p', 'mb-1 text-xs font-medium text-neutral-500', heading));
    var empty = value === '';
    box.appendChild(el('p', 'whitespace-pre-wrap break-words ' + (empty ? 'italic text-neutral-400' : 'text-neutral-900'), empty ? bannerText('msgEmpty', '(empty)') : value));
    return box;
  }

  function renderBodyDiff(section, before, after) {
    var beforeCount = charCount(before), afterCount = charCount(after);
    var delta = afterCount - beforeCount;
    var deltaText = (delta > 0 ? '+' : '') + delta.toLocaleString();
    section.appendChild(el('p', 'text-xs text-amber-800', format(bannerText('msgChars', 'Characters: %s → %s (%s)'), beforeCount.toLocaleString(), afterCount.toLocaleString(), deltaText)));
    var ops = lineDiff(before, after);
    if (!ops.some(function (item) { return item.op !== ' '; })) {
      section.appendChild(el('p', 'text-xs text-neutral-600', bannerText('msgNoBodyDiff', 'The body has no differences.')));
      return;
    }
    section.appendChild(el('p', 'text-xs text-amber-800', bannerText('msgDiffLegend', '- lines are removed; + lines are added.')));
    var pre = el('div', 'max-h-80 overflow-auto rounded-lg border border-neutral-200 bg-white py-1 font-mono text-xs leading-5');
    pre.setAttribute('role', 'group');
    pre.setAttribute('aria-label', fieldLabel('content_markdown'));
    collapseContext(ops, 2).forEach(function (item) {
      if (item.skip) {
        pre.appendChild(el('div', 'px-2 text-center text-neutral-400', format(bannerText('msgLinesSkipped', '… %s unchanged lines …'), item.skip.toLocaleString())));
        return;
      }
      var cls = item.op === '+' ? 'bg-emerald-50 text-emerald-900' : item.op === '-' ? 'bg-rose-50 text-rose-900' : 'text-neutral-600';
      var row = el('div', 'flex gap-2 px-2 ' + cls);
      row.appendChild(el('span', 'w-3 shrink-0 select-none text-center font-semibold', item.op === ' ' ? '' : (item.op === '-' ? '\u2212' : '+')));
      row.appendChild(el('span', 'min-w-0 flex-1 whitespace-pre-wrap break-all', item.text === '' ? '\u00a0' : item.text));
      pre.appendChild(row);
    });
    section.appendChild(pre);
  }

  // renderDiff は、変更のある項目だけを並べた差分を diffBox に組み立て直す。変更のある項目数を返す。
  function renderDiff() {
    if (!diffBox || !restoredDraft) return 0;
    while (diffBox.firstChild) diffBox.removeChild(diffBox.firstChild);
    var changed = diffFields.filter(function (name) {
      return typeof restoredDraft[name] === 'string' && normalizeNewlines(restoredDraft[name]) !== normalizeNewlines(current[name]);
    });
    if (!changed.length) {
      diffBox.appendChild(el('p', 'text-xs text-neutral-600', bannerText('msgNoDiff', 'No differences from the current article.')));
      return 0;
    }
    diffBox.appendChild(el('p', 'text-xs font-medium text-amber-800', format(bannerText('msgChangedFields', 'Changed fields: %s'), changed.length)));
    changed.forEach(function (name) {
      var section = el('section', 'space-y-2');
      section.setAttribute('data-diff-field', name);
      section.appendChild(el('h3', 'text-sm font-semibold text-amber-900', fieldLabel(name)));
      var before = str(current[name]), after = str(restoredDraft[name]);
      if (name === 'content_markdown') {
        renderBodyDiff(section, before, after);
      } else {
        var grid = el('div', 'grid gap-2 sm:grid-cols-2');
        grid.appendChild(valueBox(bannerText('msgCurrent', 'Current (server)'), displayValue(name, before), 'current'));
        grid.appendChild(valueBox(bannerText('msgLocal', 'Unsaved draft'), displayValue(name, after), 'local'));
        section.appendChild(grid);
      }
      diffBox.appendChild(section);
    });
    return changed.length;
  }

  function setDiffOpen(open) {
    if (!diffBox) return;
    if (open) renderDiff();
    diffBox.classList.toggle('hidden', !open);
    if (diffToggle) {
      diffToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
      diffToggle.textContent = open ? bannerText('msgHideDiff', 'Hide changes') : bannerText('msgViewDiff', 'View changes');
    }
  }
  if (diffToggle) diffToggle.addEventListener('click', function () {
    setDiffOpen(diffToggle.getAttribute('aria-expanded') !== 'true');
  });

  function setField(name, value) {
    var field = form.elements.namedItem(name);
    if (!field || typeof value !== 'string') return;
    field.value = value;
    field.dispatchEvent(new Event('input', { bubbles: true }));
    field.dispatchEvent(new Event('change', { bubbles: true }));
  }

  var existingDraft = readDraft();
  var token = existingDraft && existingDraft.token ? existingDraft.token : ((window.crypto && window.crypto.randomUUID) ? window.crypto.randomUUID() : 'dt_' + Date.now() + '_' + Math.random().toString(36).slice(2));
  var tokenField = document.getElementById('draft-token');
  if (tokenField) tokenField.value = token;
  if (existingDraft) {
    current = readFields();
    var hasDifference = fields.some(function(name) { return existingDraft[name] !== current[name]; });
    if (!hasDifference) {
      try { localStorage.removeItem(key); } catch (_) {} existingDraft = null; pendingChoice = false;
    } else {
      var serverUpdatedAt = Number(form.dataset.updatedAt) || 0;
      var draftBase = Number(existingDraft.baseUpdatedAt) || 0;
      if (draftBase <= serverUpdatedAt) {
        pendingChoice = true;
        var title = document.getElementById('offline-draft-banner-title');
        if (title && draftBase < serverUpdatedAt) title.textContent = title.dataset.conflictTitle || message('Conflict', T('offline_conflict'));
        showRestoreBanner(existingDraft);
      }
    }
  }

  if (articleId) {
    try {
      var newDraft = JSON.parse(localStorage.getItem('sorus:draft:new') || 'null');
      if (newDraft && fields.every(function(name) { return newDraft[name] === current[name]; })) localStorage.removeItem('sorus:draft:new');
    } catch (_) { /* Ignore invalid or unavailable local storage. */ }
  }

  form.addEventListener('input', scheduleSave);
  form.addEventListener('change', scheduleSave);
  window.addEventListener('beforeunload', function () { if (!submitting) saveDraft(); });
  window.addEventListener('pagehide', function () { if (!submitting) saveDraft(); });

  var restoreButton = document.getElementById('offline-draft-restore-btn');
  if (restoreButton) restoreButton.addEventListener('click', function () {
    if (!restoredDraft) return;
    fields.forEach(function (name) { setField(name, restoredDraft[name]); });
    setDiffOpen(false);
    if (banner) banner.classList.add('hidden');
    pendingChoice = false;
    scheduleSave();
  });

  var discardButton = document.getElementById('offline-draft-discard-btn');
  if (discardButton) discardButton.addEventListener('click', function () {
    window.sorusConfirm({ message: message('DiscardConfirm', T('offline_discard_confirm')), danger: true }).then(function (ok) {
      if (!ok) return;
      try { window.localStorage.removeItem(key); } catch (_) { /* 保存領域が無効でも表示は閉じる */ }
      restoredDraft = null;
      pendingChoice = false;
      setDiffOpen(false);
      if (banner) banner.classList.add('hidden');
      setStatus('');
    });
  });

  form.addEventListener('submit', function (event) {
    if (!navigator.onLine) {
      event.preventDefault();
      saveDraft();
      if (window.cmsToast) window.cmsToast(message('SubmitError', T('offline_submit_error')), 'error');
      return;
    }
    submitting = true;
    window.clearTimeout(timer);
    var pending = readFields(); pending.token = token; pending.savedAt = Date.now(); pending.baseUpdatedAt = baseUpdatedAt;
    try { window.localStorage.setItem(key, JSON.stringify(pending)); } catch (_) {}
  }, true);

  window.addEventListener('pageshow', function () { submitting = false; });

  function updateConnectionStatus() {
    if (navigator.onLine) {
      setStatus(existingDraft && !pendingChoice ? message('Saved', T('offline_status_saved')) : message('Online', T('offline_status_online')));
    } else {
      setStatus(message('Offline', T('offline_status_offline_short')));
    }
  }
  window.addEventListener('online', updateConnectionStatus);
  window.addEventListener('offline', updateConnectionStatus);
  updateConnectionStatus();
})();
