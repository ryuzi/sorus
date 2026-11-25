// メディアライブラリ（/admin/media）: 画像の詳細ダイアログ（メタデータの編集・AI 自動生成・URL のコピー）、
// 削除の確認ダイアログ、ドラッグ＆ドロップでの一括アップロード、複数選択と一括のタグの付け外し。
(function () {
  'use strict';

  var csrf = window.cmsCSRFToken();
  var messages = document.getElementById('media-messages');
  function msg(name, n) {
    var text = messages ? messages.getAttribute('data-' + name) || '' : '';
    return n === undefined ? text : text.replace('{n}', String(n));
  }

  // 公開サーバーへ未反映になったとき（応答の remote_dirty）、画面上部の共通バナー（layout.html の #remote-dirty-banner。
  // この画面では未反映が無くても隠して置いてある）に「フォトギャラリー／メディア」を足して出す（sorus_common.js）。
  function showRemoteDirty(json) {
    if (!json || !json.remote_dirty) return;
    window.sorus.remoteDirty.add('media');
  }

  // カードのタグの表示を tags（", " 区切り）にする。タグが無ければ「タグなし」と分かるように出す。
  function setTagPreview(card, tags) {
    var preview = card.querySelector('.media-tags-preview');
    if (!preview) return;
    preview.textContent = tags || msg('no-tags');
    preview.classList.toggle('is-empty', !tags);
  }

  // 一括のタグ操作の後は一覧を読み直す（絞り込みの結果とタグの枚数を新しくする）。結果の通知は読み直した後に出す。
  var flashKey = 'sorus-media-flash';
  try {
    var flash = window.sessionStorage.getItem(flashKey);
    if (flash) {
      window.sessionStorage.removeItem(flashKey);
      window.cmsToast(flash, 'success');
    }
  } catch (e) { /* 保存できない環境では通知を出さないだけ */ }
  function reloadWithToast(text) {
    try { window.sessionStorage.setItem(flashKey, text); } catch (e) { /* 通知なしで読み直す */ }
    window.location.reload();
  }

  // --- 複数選択と一括のタグ操作 -------------------------------------------------
  // 「このページをすべて選択」（#media-select-all）は、選んだ数に合わせて、全部ならチェック、一部なら中間の表示にする。
  var bulkBar = document.getElementById('media-bulk-bar');
  var bulkChecks = Array.prototype.slice.call(document.querySelectorAll('.media-select-checkbox'));
  var bulkCount = document.getElementById('media-bulk-count');
  var selectAll = document.getElementById('media-select-all');
  function selectedChecks() { return bulkChecks.filter(function (box) { return box.checked; }); }
  function refreshBulkBar() {
    var selected = selectedChecks();
    if (selectAll) {
      selectAll.checked = bulkChecks.length > 0 && selected.length === bulkChecks.length;
      selectAll.indeterminate = selected.length > 0 && selected.length < bulkChecks.length;
    }
    if (!bulkBar) return;
    bulkBar.classList.toggle('hidden', selected.length === 0);
    if (bulkCount) bulkCount.textContent = msg('bulk-selected', selected.length);
  }
  bulkChecks.forEach(function (box) {
    box.addEventListener('click', function (e) { e.stopPropagation(); });
    box.addEventListener('change', refreshBulkBar);
  });
  if (selectAll) selectAll.addEventListener('change', function () {
    bulkChecks.forEach(function (box) { box.checked = selectAll.checked; });
    refreshBulkBar();
  });
  var bulkClear = document.getElementById('media-bulk-clear');
  if (bulkClear) bulkClear.addEventListener('click', function () {
    bulkChecks.forEach(function (box) { box.checked = false; });
    refreshBulkBar();
  });
  function cardTags(card) {
    return (card.dataset.tags || '').split(',').map(function (t) { return t.trim(); }).filter(Boolean);
  }
  // knownTagStats は、タグを足すときの候補（これまでに使ったタグ）。GET /admin/api/media/tags を1回だけ読み、
  // { tags: タグ名の順, popular: [{tag, count}] 多い順 } にする。読めなければこの画面に出ている写真のタグを使う。
  var knownTagsPromise = null;
  function knownTagStats() {
    if (!knownTagsPromise) {
      var onPage = [];
      document.querySelectorAll('.media-card').forEach(function (card) {
        cardTags(card).forEach(function (t) { if (onPage.indexOf(t) < 0) onPage.push(t); });
      });
      var fallback = function () {
        onPage.sort();
        return { tags: onPage, popular: onPage.map(function (t) { return { tag: t, count: 0 }; }) };
      };
      knownTagsPromise = fetch('/admin/api/media/tags', { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
        .then(function (res) { if (!res.ok) throw new Error('tags'); return res.json(); })
        .then(function (json) {
          if (!Array.isArray(json.tags)) return fallback();
          var popular = Array.isArray(json.popular) ? json.popular : json.tags.map(function (t) { return { tag: t, count: 0 }; });
          return { tags: json.tags, popular: popular };
        })
        .catch(function () { knownTagsPromise = null; return fallback(); });
    }
    return knownTagsPromise;
  }
  function knownTags() {
    return knownTagStats().then(function (stats) { return stats.tags; });
  }
  // タグの入力は管理画面共通の入力モーダル（window.sorusPrompt）で行う。足すときはこれまでのタグ、外すときは選んだ写真に
  // 付いているタグを、押すと入力欄に入る候補として並べる。
  function applyBulkTags(mode) {
    var selected = selectedChecks();
    if (!selected.length) return;
    var suggestions;
    if (mode === 'add') {
      suggestions = knownTags();
    } else {
      var onSelected = [];
      selected.forEach(function (box) {
        cardTags(box.closest('li').querySelector('.media-card')).forEach(function (t) { if (onSelected.indexOf(t) < 0) onSelected.push(t); });
      });
      suggestions = Promise.resolve(onSelected.sort());
    }
    suggestions.then(function (list) {
      return window.sorusPrompt({
        title: msg(mode === 'add' ? 'bulk-title-add' : 'bulk-title-remove'),
        message: msg(mode === 'add' ? 'bulk-prompt-add' : 'bulk-prompt-remove'),
        label: msg('bulk-tags-label'),
        placeholder: msg('bulk-tags-placeholder'),
        okLabel: msg(mode === 'add' ? 'bulk-title-add' : 'bulk-title-remove'),
        suggestions: list,
        suggestionsLabel: msg(mode === 'add' ? 'bulk-suggest-add' : 'bulk-suggest-remove')
      });
    }).then(function (entered) {
      if (entered === null) return;
      sendBulkTags(mode, selected, entered);
    });
  }
  function sendBulkTags(mode, selected, entered) {
    var tags = entered.split(/[,，、]/).map(function (tag) { return tag.trim(); }).filter(Boolean);
    if (!tags.length) return;
    var payload = { media_ids: selected.map(function (box) { return box.closest('li').querySelector('.media-card').dataset.mediaId; }), add_tags: mode === 'add' ? tags : [], remove_tags: mode === 'remove' ? tags : [] };
    fetch('/admin/api/media/bulk-tags', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json', 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (json) {
        if (!res.ok) throw new Error(json.message || msg('bulk-error'));
        return json;
      });
    }).then(function (json) {
      // 公開サーバーへの未反映は、読み直した画面の共通バナーに出る。
      reloadWithToast(msg('bulk-success', json.updated_count));
    }).catch(function (err) { window.cmsToast(err && err.message && err.message !== 'Failed to fetch' ? err.message : msg('bulk-error'), 'error'); });
  }
  var bulkAdd = document.getElementById('media-bulk-add');
  var bulkRemove = document.getElementById('media-bulk-remove');
  if (bulkAdd) bulkAdd.addEventListener('click', function () { applyBulkTags('add'); });
  if (bulkRemove) bulkRemove.addEventListener('click', function () { applyBulkTags('remove'); });
  refreshBulkBar();

  // --- 詳細ダイアログ ---------------------------------------------------------
  var inspector = document.getElementById('media-inspector');
  var metaForm = document.getElementById('media-meta-form');
  var saveButton = document.getElementById('media-save');
  var analyzeButton = document.getElementById('media-ai-analyze');
  var analyzeLabel = analyzeButton ? analyzeButton.textContent : '';
  var aiSuggestions = document.getElementById('media-ai-suggestions');
  var deleteDialog = document.getElementById('media-delete-dialog');
  var deleteForm = document.getElementById('media-delete-form');
  var current = null;

  function setText(id, text) {
    var el = document.getElementById(id);
    if (el) el.textContent = text || '';
  }
  function setValue(id, value) {
    var el = document.getElementById(id);
    if (el) el.value = value || '';
  }
  function showUsage(el, n) {
    if (!el) return;
    n = parseInt(n, 10) || 0;
    el.hidden = n === 0;
    el.textContent = el.getAttribute('data-template').replace('{n}', String(n));
  }

  // --- 詳細ダイアログのタグ ---------------------------------------------------------
  // 送る値はカンマ区切りの欄（#media-tags）だけで、ここの部品はすべてその欄を読み書きする。
  //   付いているタグ: × 付きのチップ（#media-tag-selected）。× で外す。
  //   探して足す: 1つの欄（#media-tag-search、combobox）。部分一致の候補（最大 8 件）を出し、既存に無ければ先頭に
  //     「新しいタグとして追加」を出す。↑↓ で選び Enter で追加、Esc で閉じる、空欄で Backspace なら最後のタグを外す。
  //   よく使うタグ（多い順・年以外の上位）と年のタグ（YYYY年）、すべてのタグは押すと付け外しできるチップ。
  var tagInput = document.getElementById('media-tags');
  var tagEditor = document.getElementById('media-tag-editor');
  var tagSelected = document.getElementById('media-tag-selected');
  var tagSearch = document.getElementById('media-tag-search');
  var tagOptions = document.getElementById('media-tag-options');
  var tagStatus = document.getElementById('media-tag-status');
  var tagChips = document.getElementById('media-tag-chips');
  var tagPopularGroup = document.getElementById('media-tag-popular-group');
  var tagYears = document.getElementById('media-tag-years');
  var tagYearGroup = document.getElementById('media-tag-year-group');
  var tagAll = document.getElementById('media-tag-all-tags');
  var tagAllList = document.getElementById('media-tag-all-tags-list');
  var POPULAR_TAG_LIMIT = 12;
  var MAX_TAG_OPTIONS = 8;
  // 年のタグ（例: 2026年）の書き方は画面から受け取る（data-year-tag-suffix。タグはデータなので表示言語によらない）。
  var yearTagSuffix = (tagEditor && tagEditor.getAttribute('data-year-tag-suffix')) || '';
  var YEAR_TAG = new RegExp('^(\\d{4})' + yearTagSuffix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '$');
  var tagStats = { tags: [], popular: [] };
  var activeOption = -1;

  function editorMsg(name, values) {
    var text = (tagEditor && tagEditor.getAttribute('data-msg-' + name)) || '';
    Object.keys(values || {}).forEach(function (k) { text = text.split('{' + k + '}').join(values[k]); });
    return text;
  }
  function inputTags() {
    var out = [];
    if (!tagInput) return out;
    tagInput.value.split(/[,，、]/).forEach(function (t) {
      t = t.trim();
      if (t && out.indexOf(t) < 0) out.push(t);
    });
    return out;
  }
  function setInputTags(list) {
    tagInput.value = list.join(', ');
    syncTagChips();
  }
  function toggleTag(tag) {
    var now = inputTags();
    var at = now.indexOf(tag);
    if (at >= 0) now.splice(at, 1);
    else now.push(tag);
    setInputTags(now);
  }
  function addTag(tag) {
    tag = String(tag || '').replace(/[,，、]/g, ' ').trim();
    if (!tag) return;
    var now = inputTags();
    if (now.indexOf(tag) < 0) now.push(tag);
    setInputTags(now);
  }
  // normalizeTag は検索用に、全角・半角（NFKC）と大文字・小文字、ひらがな・カタカナの違いをそろえる。
  function normalizeTag(t) {
    var s = String(t || '');
    if (s.normalize) s = s.normalize('NFKC');
    return s.toLowerCase().replace(/[\u30a1-\u30f6]/g, function (c) { return String.fromCharCode(c.charCodeAt(0) - 0x60); }).trim();
  }
  function chipButton(tag, count) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'tag-chip';
    b.setAttribute('data-tag', tag);
    b.setAttribute('aria-pressed', 'false');
    b.textContent = tag;
    if (count) {
      var n = document.createElement('span');
      n.className = 'tag-chip-count';
      n.textContent = String(count);
      b.appendChild(n);
    }
    return b;
  }
  // syncTagChips は付いているタグのチップと、各チップの押下状態を欄の値に合わせる。
  function syncTagChips() {
    if (!tagInput) return;
    var now = inputTags();
    if (tagSelected) {
      tagSelected.textContent = '';
      now.forEach(function (t) {
        var li = document.createElement('li');
        li.className = 'tag-chip is-selected';
        li.setAttribute('data-selected-tag', t);
        var name = document.createElement('span');
        name.textContent = t;
        var x = document.createElement('button');
        x.type = 'button';
        x.className = 'tag-chip-remove';
        x.setAttribute('data-remove-tag', t);
        x.setAttribute('aria-label', editorMsg('remove', { t: t }));
        x.textContent = '×';
        li.appendChild(name);
        li.appendChild(x);
        tagSelected.appendChild(li);
      });
      if (!now.length) {
        var empty = document.createElement('li');
        empty.className = 'text-xs text-neutral-400';
        empty.textContent = editorMsg('none');
        tagSelected.appendChild(empty);
      }
    }
    if (tagEditor) {
      Array.prototype.forEach.call(tagEditor.querySelectorAll('button[data-tag]'), function (chip) {
        chip.setAttribute('aria-pressed', now.indexOf(chip.getAttribute('data-tag')) >= 0 ? 'true' : 'false');
      });
    }
    if (tagSearch && tagSearch.value.trim()) renderTagOptions();
  }
  // renderTagChips はよく使うタグ・年のタグ・すべてのタグを、これまでに使ったタグから作り直す。
  function renderTagChips() {
    if (!tagInput) return;
    syncTagChips();
    knownTagStats().then(function (stats) {
      tagStats = stats;
      var popular = [];
      var years = [];
      (stats.popular || []).forEach(function (tc) {
        if (YEAR_TAG.test(tc.tag)) years.push(tc.tag);
        else if (popular.length < POPULAR_TAG_LIMIT) popular.push(tc);
      });
      // 年は新しい順。今年のタグがまだ無くても、押すだけで付けられるよう候補に出す。
      var thisYear = new Date().getFullYear() + yearTagSuffix;
      if (yearTagSuffix && years.indexOf(thisYear) < 0) years.push(thisYear);
      years.sort(function (a, b) { return b.localeCompare(a); });
      if (tagChips) {
        tagChips.textContent = '';
        popular.forEach(function (tc) { tagChips.appendChild(chipButton(tc.tag, tc.count)); });
        tagPopularGroup.hidden = popular.length === 0;
      }
      if (tagYears) {
        tagYears.textContent = '';
        years.forEach(function (t) { tagYears.appendChild(chipButton(t)); });
        tagYearGroup.hidden = years.length === 0;
      }
      var others = (stats.tags || []).filter(function (t) { return !YEAR_TAG.test(t); });
      if (tagAll && tagAllList) {
        tagAllList.textContent = '';
        others.forEach(function (t) { tagAllList.appendChild(chipButton(t)); });
        tagAll.querySelector('summary').textContent = editorMsg('all', { n: String(others.length) });
        tagAll.hidden = others.length <= popular.length;
      }
      syncTagChips();
    });
  }
  // tagMatches は欄に入力中の語に部分一致する、まだ付いていないタグ（前方一致を先に）を返す。
  function tagMatches(q) {
    var nq = normalizeTag(q);
    if (!nq) return [];
    var now = inputTags();
    var head = [];
    var rest = [];
    (tagStats.popular || []).forEach(function (tc) {
      if (now.indexOf(tc.tag) >= 0) return;
      var at = normalizeTag(tc.tag).indexOf(nq);
      if (at === 0) head.push(tc);
      else if (at > 0) rest.push(tc);
    });
    return head.concat(rest).slice(0, MAX_TAG_OPTIONS);
  }
  function closeTagOptions() {
    if (!tagOptions) return;
    tagOptions.hidden = true;
    tagOptions.textContent = '';
    activeOption = -1;
    tagSearch.setAttribute('aria-expanded', 'false');
    tagSearch.removeAttribute('aria-activedescendant');
  }
  function renderTagOptions() {
    var q = tagSearch.value.replace(/[,，、]/g, ' ').trim();
    tagOptions.textContent = '';
    activeOption = -1;
    tagSearch.removeAttribute('aria-activedescendant');
    if (!q) { closeTagOptions(); return; }
    var options = [];
    var exact = (tagStats.tags || []).some(function (t) { return normalizeTag(t) === normalizeTag(q); }) || inputTags().indexOf(q) >= 0;
    if (!exact) options.push({ tag: q, isNew: true });
    tagMatches(q).forEach(function (tc) { options.push({ tag: tc.tag, count: tc.count }); });
    options.forEach(function (o, i) {
      var li = document.createElement('li');
      li.id = 'media-tag-option-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', 'false');
      li.setAttribute('data-option-tag', o.tag);
      li.className = 'flex cursor-pointer items-center justify-between gap-2 px-3 py-2 hover:bg-neutral-100' + (o.isNew ? ' font-medium text-neutral-900' : ' text-neutral-700');
      var name = document.createElement('span');
      name.className = 'min-w-0 truncate';
      name.textContent = o.isNew ? editorMsg('add-new', { t: o.tag }) : o.tag;
      li.appendChild(name);
      if (o.count) {
        var n = document.createElement('span');
        n.className = 'shrink-0 text-xs text-neutral-400';
        n.textContent = String(o.count);
        li.appendChild(n);
      }
      tagOptions.appendChild(li);
    });
    tagOptions.hidden = options.length === 0;
    tagSearch.setAttribute('aria-expanded', options.length ? 'true' : 'false');
    if (tagStatus) tagStatus.textContent = editorMsg('results', { n: String(options.length) });
    if (options.length) moveTagOption(0);
  }
  function moveTagOption(i) {
    var items = tagOptions.querySelectorAll('[role="option"]');
    if (!items.length) return;
    activeOption = (i + items.length) % items.length;
    Array.prototype.forEach.call(items, function (li, k) {
      var on = k === activeOption;
      li.setAttribute('aria-selected', on ? 'true' : 'false');
      li.classList.toggle('bg-neutral-100', on);
    });
    tagSearch.setAttribute('aria-activedescendant', items[activeOption].id);
  }
  function chooseTagOption(li) {
    addTag(li ? li.getAttribute('data-option-tag') : tagSearch.value);
    tagSearch.value = '';
    closeTagOptions();
    tagSearch.focus();
  }
  if (tagEditor && tagInput) {
    tagEditor.addEventListener('click', function (e) {
      var remove = e.target.closest('[data-remove-tag]');
      if (remove) {
        var now = inputTags();
        var at = now.indexOf(remove.getAttribute('data-remove-tag'));
        if (at >= 0) now.splice(at, 1);
        setInputTags(now);
        if (tagSearch) tagSearch.focus();
        return;
      }
      var chip = e.target.closest('button[data-tag]');
      if (chip) toggleTag(chip.getAttribute('data-tag'));
    });
    tagInput.addEventListener('input', syncTagChips);
  }
  if (tagSearch && tagOptions) {
    tagSearch.addEventListener('input', function () {
      // カンマを打ったら、その前までをタグとして足す。
      if (/[,，、]/.test(tagSearch.value)) {
        var parts = tagSearch.value.split(/[,，、]/);
        tagSearch.value = parts.pop();
        parts.forEach(addTag);
      }
      renderTagOptions();
    });
    tagSearch.addEventListener('keydown', function (e) {
      if (e.isComposing || e.keyCode === 229) return;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        if (tagOptions.hidden) renderTagOptions();
        moveTagOption(activeOption + (e.key === 'ArrowDown' ? 1 : -1));
        e.preventDefault();
      } else if (e.key === 'Enter') {
        // Enter は詳細の保存（フォームの送信）ではなく、タグの追加にする。
        e.preventDefault();
        var items = tagOptions.querySelectorAll('[role="option"]');
        if (tagSearch.value.trim()) chooseTagOption(items[activeOption] || null);
      } else if (e.key === 'Escape') {
        if (!tagOptions.hidden || tagSearch.value) {
          // ダイアログを閉じず、候補と入力だけを消す。
          e.preventDefault();
          e.stopPropagation();
          tagSearch.value = '';
          closeTagOptions();
        }
      } else if (e.key === 'Backspace' && !tagSearch.value) {
        var now = inputTags();
        if (now.length) {
          now.pop();
          setInputTags(now);
        }
      }
    });
    tagOptions.addEventListener('mousedown', function (e) {
      // 候補を押した瞬間に欄のフォーカスが外れて候補が消えないようにする。
      e.preventDefault();
    });
    tagOptions.addEventListener('click', function (e) {
      var li = e.target.closest('[role="option"]');
      if (li) chooseTagOption(li);
    });
    tagSearch.addEventListener('blur', function () { setTimeout(closeTagOptions, 150); });
  }

  function openInspector(card) {
    current = card;
    var d = card.dataset;
    setText('media-inspector-title', d.filename);
    var img = document.getElementById('media-inspector-image');
    img.src = d.url;
    img.alt = d.alt;
    setText('media-inspector-size', d.width + ' x ' + d.height + ' px');
    setText('media-inspector-mime', d.mime);
    setText('media-inspector-created', d.created);
    setText('media-inspector-uploader', d.uploader || '-');
    showUsage(document.getElementById('media-inspector-usage'), d.usage);
    setValue('media-url-markdown', d.markdown);
    setValue('media-url-large', d.url);
    setText('media-url-original', d.original);
    setText('media-url-medium', d.medium);
    setText('media-url-small', d.small);
    metaForm.action = '/admin/media/' + encodeURIComponent(d.mediaId);
    setValue('media-alt', d.alt);
    setValue('media-caption', d.caption);
    setValue('media-tags', d.tags);
    if (tagSearch) { tagSearch.value = ''; closeTagOptions(); }
    clearAISuggestions();
    renderTagChips();
    inspector.showModal();
  }

  // update は保存後の値（サーバーが整えた値）をカードと詳細ダイアログに反映する。
  function update(card, item) {
    var d = card.dataset;
    d.alt = item.alt;
    d.caption = item.caption;
    d.tags = item.ai_tags;
    d.markdown = item.markdown;
    d.usage = String(item.usage_count);
    var thumb = card.querySelector('img');
    if (thumb) thumb.alt = item.alt;
    // 応答を待つ間にダイアログで別の画像を開いていれば、入力欄はその画像のまま残す。
    if (current !== card) return;
    setValue('media-alt', item.alt);
    setValue('media-caption', item.caption);
    setValue('media-tags', item.ai_tags);
    setValue('media-url-markdown', item.markdown);
    syncTagChips();
  }

  if (inspector) {
    document.querySelectorAll('.media-card').forEach(function (card) {
      card.addEventListener('click', function () { openInspector(card); });
    });
    inspector.querySelectorAll('[data-media-close]').forEach(function (b) {
      b.addEventListener('click', function () { inspector.close(); });
    });
    inspector.addEventListener('click', function (e) { if (e.target === inspector) inspector.close(); });

    metaForm.addEventListener('submit', function (e) {
      e.preventDefault();
      if (!current) return;
      var card = current;
      saveButton.disabled = true;
      fetch(metaForm.action, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
        body: new URLSearchParams(new FormData(metaForm)),
        credentials: 'same-origin'
      }).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (json) {
          if (!res.ok) throw new Error(json.message || msg('save-error'));
          return json;
        });
      }).then(function (item) {
        setTagPreview(card, item.ai_tags || '');
        knownTagsPromise = null; // 新しく入力したタグも、次からは候補に出す。
        update(card, item);
        if (current === card) {
          clearAISuggestions();
          renderTagChips();
        }
        window.cmsToast(msg('saved'), 'success');
        showRemoteDirty(item);
      }).catch(function (err) {
        window.cmsToast(err.message || msg('save-error'), 'error');
      }).finally(function () { saveButton.disabled = false; });
    });
  }

  // --- AI 自動生成 -------------------------------------------------------------
  // 画像を AI に解析させ、空欄を結果で埋める。入力済みの説明は案だけを示し、保存は利用者が選んだ後に行う。
  // タグはサーバーが今のタグを残したまま AI のタグを足して保存する（応答の ai_tags）。
  // 解析中にダイアログで別の画像を開いた場合も、結果は解析を始めた画像のカードに反映する（update を参照）。
  if (inspector && analyzeButton) {
    analyzeButton.addEventListener('click', function () {
      if (!current) return;
      var card = current;
      analyzeButton.disabled = true;
      saveButton.disabled = true;
      analyzeButton.textContent = msg('analyzing');
      analyzeButton.setAttribute('aria-busy', 'true');
      fetch('/admin/api/media/' + encodeURIComponent(card.dataset.mediaId) + '/analyze', {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
        credentials: 'same-origin'
      }).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (json) {
          if (!res.ok) throw new Error(json.message || msg('analyze-error'));
          return json;
        });
      }).then(function (item) {
        setTagPreview(card, item.ai_tags || '');
        knownTagsPromise = null;
        update(card, item);
        if (current === card) {
          showAISuggestions(item);
          renderTagChips();
        }
        window.cmsToast(msg('analyzed'), 'success');
        showRemoteDirty(item);
      }).catch(function (err) {
        window.cmsToast(err.message || msg('analyze-error'), 'error');
      }).finally(function () {
        analyzeButton.disabled = false;
        saveButton.disabled = false;
        analyzeButton.textContent = analyzeLabel;
        analyzeButton.removeAttribute('aria-busy');
      });
    });
  }

  function showAISuggestions(item) {
    if (!aiSuggestions) return;
    aiSuggestions.replaceChildren();
    var rows = [
      {key: 'alt', field: 'media-alt', title: aiSuggestions.dataset.titleAlt},
      {key: 'caption', field: 'media-caption', title: aiSuggestions.dataset.titleCaption}
    ].filter(function (row) { return item['suggested_' + row.key]; });
    aiSuggestions.hidden = rows.length === 0;
    if (!rows.length) return;
    var note = document.createElement('p');
    note.className = 'text-xs text-neutral-600';
    note.textContent = aiSuggestions.dataset.note;
    aiSuggestions.appendChild(note);
    rows.forEach(function (row) {
      var box = document.createElement('div');
      box.className = 'rounded-md border border-amber-200 bg-white p-2 text-xs';
      var title = document.createElement('p');
      title.className = 'font-semibold text-neutral-800';
      title.textContent = row.title;
      box.appendChild(title);
      [[aiSuggestions.dataset.current, document.getElementById(row.field).value],
        [aiSuggestions.dataset.suggestion, item['suggested_' + row.key]]].forEach(function (pair) {
        var line = document.createElement('p');
        line.className = 'mt-1 whitespace-pre-wrap break-words text-neutral-700';
        var label = document.createElement('strong');
        label.textContent = pair[0] + ': ';
        line.appendChild(label);
        line.appendChild(document.createTextNode(pair[1]));
        box.appendChild(line);
      });
      var use = document.createElement('button');
      use.type = 'button';
      use.className = 'mt-2 rounded border border-neutral-300 px-2 py-1 font-medium text-neutral-800 hover:bg-neutral-50';
      use.textContent = aiSuggestions.dataset.use;
      use.addEventListener('click', function () {
        document.getElementById(row.field).value = item['suggested_' + row.key];
      });
      box.appendChild(use);
      aiSuggestions.appendChild(box);
    });
  }

  function clearAISuggestions() {
    if (!aiSuggestions) return;
    aiSuggestions.replaceChildren();
    aiSuggestions.hidden = true;
  }

  // --- 削除の確認ダイアログ ---------------------------------------------------
  var deleteOpen = document.getElementById('media-delete-open');
  if (deleteOpen && deleteDialog) {
    deleteOpen.addEventListener('click', function () {
      if (!current) return;
      deleteForm.action = '/admin/media/' + encodeURIComponent(current.dataset.mediaId) + '/delete';
      setText('media-delete-name', current.dataset.filename);
      showUsage(document.getElementById('media-delete-usage'), current.dataset.usage);
      deleteDialog.showModal();
    });
    deleteDialog.querySelectorAll('[data-media-delete-cancel]').forEach(function (b) {
      b.addEventListener('click', function () { deleteDialog.close(); });
    });
  }

  // --- 一括アップロード ---------------------------------------------------------
  var dropzone = document.getElementById('media-dropzone');
  var dropLabel = document.getElementById('media-dropzone-label');
  var fileInput = document.getElementById('media-upload-input');
  if (!dropzone || !fileInput) return;
  var defaultLabel = dropLabel.textContent;

  function uploadOne(file) {
    var data = new FormData();
    data.append('file', file, file.name || 'image.png');
    return fetch('/admin/media/upload', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
      body: data,
      credentials: 'same-origin'
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (json) {
        if (!res.ok) throw new Error((file.name ? file.name + ': ' : '') + (json.message || msg('upload-error')));
        return json;
      });
    });
  }

  // uploadAll は 1 枚ずつ順にアップロードする（大きな画像の変換でサーバーを詰まらせない）。
  // すべて成功したら一覧を読み直し、失敗があれば理由を表示してその場に留まる。
  function uploadAll(files) {
    var list = Array.prototype.filter.call(files || [], function (f) {
      if (/^image\/(jpeg|png|webp)$/.test(f.type)) return true;
      window.cmsToast((f.name ? f.name + ': ' : '') + msg('upload-type'), 'error');
      return false;
    });
    if (list.length === 0) return;
    var ok = 0;
    var failed = 0;
    var chain = Promise.resolve();
    list.forEach(function (file, i) {
      chain = chain.then(function () {
        dropLabel.textContent = msg('uploading', list.length - i);
        return uploadOne(file).then(function () { ok++; }, function (err) {
          failed++;
          window.cmsToast(err.message, 'error');
        });
      });
    });
    chain.then(function () {
      dropLabel.textContent = defaultLabel;
      if (failed === 0) {
        window.location.href = '/admin/media?flash=media_uploaded';
      } else if (ok > 0) {
        window.cmsToast(msg('upload-partial', ok), 'success');
      }
    });
  }

  dropzone.addEventListener('dragover', function (e) {
    if (e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types, 'Files') >= 0) {
      e.preventDefault();
      dropzone.classList.add('is-dragover');
    }
  });
  dropzone.addEventListener('dragleave', function () { dropzone.classList.remove('is-dragover'); });
  dropzone.addEventListener('drop', function (e) {
    dropzone.classList.remove('is-dragover');
    if (e.dataTransfer && e.dataTransfer.files.length > 0) {
      e.preventDefault();
      uploadAll(e.dataTransfer.files);
    }
  });
  fileInput.addEventListener('change', function () {
    uploadAll(fileInput.files);
    fileInput.value = '';
  });
})();

// 画像の詳細ダイアログの「使用中の記事」: カードを開いたとき、その画像を使っている記事の一覧を読み込む。
(function () {
  var container = document.getElementById('media-inspector-articles-container');
  var list = document.getElementById('media-inspector-articles');
  if (!container || !list) return;

  function label(name) { return container.getAttribute('data-label-' + name) || ''; }

  // articleRow は使用中の記事1件の行。タイトルは公開中なら公開ページ（/admin の外なので、デスクトップ版では admin.js が
  // 既定のブラウザで開く）、それ以外はプレビュー（data-desktop-preview で既定のブラウザに渡す）。行末に小さく「編集」を残す。
  function articleRow(a) {
    var li = document.createElement('li');
    li.className = 'flex items-baseline justify-between gap-2 py-1.5';
    li.setAttribute('data-article-id', a.id);
    var title = document.createElement('a');
    var id = encodeURIComponent(a.id);
    if (a.published && a.public_url) {
      title.href = a.public_url;
    } else {
      title.href = '/admin/articles/' + id + '/preview';
      title.setAttribute('data-desktop-preview', '/admin/articles/' + id + '/preview-open');
    }
    title.target = '_blank';
    title.rel = 'noopener noreferrer';
    title.className = 'media-used-title min-w-0 break-all font-medium text-zinc-800 underline hover:text-zinc-600';
    title.textContent = a.title || a.slug || a.id;
    var sr = document.createElement('span');
    sr.className = 'sr-only';
    sr.textContent = label('new-tab');
    title.appendChild(sr);
    var left = document.createElement('span');
    left.className = 'min-w-0';
    left.appendChild(title);
    // どこで使っているか（アイキャッチ・本文）と、未公開かを小さなバッジで添える。
    function addBadge(name, kind) {
      var badge = document.createElement('span');
      badge.className = 'ml-1.5 whitespace-nowrap rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-600';
      badge.setAttribute('data-usage', kind);
      badge.textContent = label(name);
      left.appendChild(badge);
    }
    if (a.as_featured) addBadge('featured', 'featured');
    if (a.in_body) addBadge('body', 'body');
    if (!a.published) addBadge('draft', 'draft');
    var edit = document.createElement('a');
    edit.href = '/admin/articles/' + id + '/edit';
    edit.className = 'media-used-edit shrink-0 text-[11px] text-neutral-500 underline hover:text-neutral-800';
    edit.textContent = label('edit');
    li.appendChild(left);
    li.appendChild(edit);
    return li;
  }

  function loadArticles(mediaId) {
    container.hidden = true;
    list.textContent = '';
    if (!mediaId) return;

    fetch('/admin/api/media/' + encodeURIComponent(mediaId) + '/articles', {
      headers: { 'Accept': 'application/json' },
      credentials: 'same-origin'
    }).then(function(res) {
      if (!res.ok) throw new Error('HTTP ' + res.status);
      return res.json();
    }).then(function(data) {
      list.textContent = '';
      if (data && data.articles && data.articles.length > 0) {
        data.articles.forEach(function(a) {
          list.appendChild(articleRow(a));
        });
        container.hidden = false;
      } else {
        container.hidden = true;
      }
    }).catch(function() {
      container.hidden = true;
    });
  }

  document.querySelectorAll('.media-card').forEach(function(card) {
    card.addEventListener('click', function() {
      var usage = parseInt(card.dataset.usage, 10) || 0;
      if (usage > 0 && card.dataset.mediaId) {
        loadArticles(card.dataset.mediaId);
      } else {
        container.hidden = true;
        list.textContent = '';
      }
    });
  });
})();
