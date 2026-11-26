// 記事編集画面のスクリプト: リアルタイムプレビュー（数式は KaTeX、図は Mermaid で描画）、AI 執筆アシスタント（推敲・タイトル提案・
// リード文生成）、排他ロックのハートビートと離脱時の解放、画像のドラッグ＆ドロップ・貼り付けアップロード、
// 校閲・申し送りメモのスレッド、添付資料のアップロード、リビジョン履歴の差分表示と復元。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };

  var form = document.getElementById('article-form');
  if (!form) return;

  var articleId = form.dataset.articleId;
  var readOnly = form.dataset.readOnly === 'true';
  var heartbeatMs = (parseInt(form.dataset.heartbeatSeconds, 10) || 30) * 1000;
  var textarea = document.getElementById('content_markdown');
  var preview = document.getElementById('preview');
  var csrf = window.cmsCSRFToken();

  // --- リアルタイムプレビュー ---------------------------------------------
  var previewTimer = null;
  var previewSeq = 0;
  function updatePreview() {
    var seq = ++previewSeq;
    var body = new URLSearchParams();
    body.set('content_markdown', textarea.value);
    fetch('/admin/preview', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf },
      body: body,
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) throw new Error('preview ' + res.status);
      return res.text();
    }).then(function (html) {
      // 遅れて届いた古い結果で新しいプレビューを上書きしない。HTML はサーバーでサニタイズ済み。
      if (seq !== previewSeq) return;
      preview.innerHTML = html;
      if (window.cmsRenderMath) window.cmsRenderMath(preview);
      if (window.cmsRenderMermaid) window.cmsRenderMermaid(preview);
    }).catch(function () { /* 次の入力で再試行する */ });
  }
  textarea.addEventListener('input', function () {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(updatePreview, 300);
  });

  // --- 本文のシンタックスハイライト（依存ゼロ） ------------------------------
  // 文字を透明にした textarea の背面に、同じ文字を色付きで描いたミラー（#editor-highlight）を重ねる。
  // ミラーは表示専用（aria-hidden）で、本文はかならず HTML エスケープしてから描く（本文を HTML として解釈しない）。
  // 色を付けるだけで文字は1文字も足し引きしないため、折り返し・カーソル位置は textarea と一致する（admin.css）。
  var editorWrap = document.getElementById('editor-wrap');
  var highlightCode = document.getElementById('editor-highlight');
  var highlightPre = highlightCode && highlightCode.parentNode;

  function escapeHTML(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  function mdSpan(cls, html) { return '<span class="' + cls + '">' + html + '</span>'; }
  function mdMark(s) { return mdSpan('md-mark', escapeHTML(s)); }

  // インラインの記法。同じ位置で複数が当てはまるときは先に書いたものを優先する。
  var inlineRules = [
    { re: /(`+)(.+?)\1/g, render: function (m) { return mdMark(m[1]) + mdSpan('md-code', escapeHTML(m[2])) + mdMark(m[1]); } },
    { re: /(!?\[)([^\]]*)(\]\()([^)\s]*)(\))/g, render: function (m) {
      return mdMark(m[1]) + mdSpan('md-link', renderInline(m[2])) + mdMark(m[3]) + mdSpan('md-url', escapeHTML(m[4])) + mdMark(m[5]);
    } },
    { re: /(\$\$)(.+?)(\$\$)/g, render: function (m) { return mdMark(m[1]) + mdSpan('md-math', escapeHTML(m[2])) + mdMark(m[3]); } },
    { re: /(\$)([^\s$](?:[^$]*[^\s$\\])?)(\$)/g, render: function (m) { return mdMark(m[1]) + mdSpan('md-math', escapeHTML(m[2])) + mdMark(m[3]); } },
    { re: /(\*\*|__)([^\s*_](?:.*?[^\s*_])?)(\1)/g, render: function (m) { return mdMark(m[1]) + mdSpan('md-strong', renderInline(m[2])) + mdMark(m[3]); } },
    { re: /(\*)([^\s*](?:.*?[^\s*])?)(\*)/g, render: function (m) { return mdMark(m[1]) + mdSpan('md-em', renderInline(m[2])) + mdMark(m[3]); } }
  ];

  function renderInline(text) {
    var out = '';
    var i = 0;
    while (i < text.length) {
      var best = null, bestRule = null;
      for (var r = 0; r < inlineRules.length; r++) {
        var re = inlineRules[r].re;
        re.lastIndex = i;
        var m = re.exec(text);
        if (m && m[0].length > 0 && (!best || m.index < best.index)) { best = m; bestRule = inlineRules[r]; }
      }
      if (!best) break;
      out += escapeHTML(text.slice(i, best.index)) + bestRule.render(best);
      i = best.index + best[0].length;
    }
    return out + escapeHTML(text.slice(i));
  }

  // highlightMarkdown は本文全体をハイライト用の HTML にする。行ごとに、コードブロック・数式ブロックの中かを追う。
  function highlightMarkdown(src) {
    var lines = src.split('\n');
    var out = [];
    var fence = null; // コードブロックを開いた記号（``` または ~~~）
    var mathBlock = false;
    for (var n = 0; n < lines.length; n++) {
      var line = lines[n];
      var trimmed = line.trim();
      var m;
      if (fence) {
        if (trimmed.indexOf(fence) === 0 && /^(`{3,}|~{3,})\s*$/.test(trimmed)) { fence = null; out.push(mdMark(line)); }
        else out.push(mdSpan('md-codeblock', escapeHTML(line)));
        continue;
      }
      if (mathBlock) {
        if (trimmed === '$$' || /\$\$\s*$/.test(trimmed)) mathBlock = false;
        out.push(mdSpan('md-math', escapeHTML(line)));
        continue;
      }
      if ((m = /^\s{0,3}(`{3,}|~{3,})/.exec(line))) {
        fence = m[1].slice(0, 3);
        out.push(mdMark(line));
        continue;
      }
      if (trimmed.indexOf('$$') === 0 && (trimmed === '$$' || !/.\$\$\s*$/.test(trimmed.slice(1)))) {
        mathBlock = true;
        out.push(mdSpan('md-math', escapeHTML(line)));
        continue;
      }
      if ((m = /^(\s{0,3}#{1,6})(\s.*)?$/.exec(line))) {
        out.push(mdMark(m[1]) + mdSpan('md-heading', renderInline(m[2] || '')));
        continue;
      }
      if ((m = /^(\s*(?:>\s?)+)(.*)$/.exec(line))) {
        out.push(mdMark(m[1]) + mdSpan('md-quote', renderInline(m[2])));
        continue;
      }
      if ((m = /^(\s*)([-*+]|\d+[.)])(\s+)(.*)$/.exec(line))) {
        out.push(escapeHTML(m[1]) + mdSpan('md-list', escapeHTML(m[2])) + escapeHTML(m[3]) + renderInline(m[4]));
        continue;
      }
      out.push(renderInline(line));
    }
    // 末尾が改行のとき、textarea は空の最後の行を表示する。ミラーにも行を作るため空白を1つ足す（文字の位置は変わらない）。
    return out.join('\n') + (src.slice(-1) === '\n' ? ' ' : '');
  }

  var highlightFrame = 0;
  function syncHighlightScroll() {
    if (!highlightPre) return;
    highlightPre.scrollTop = textarea.scrollTop;
    highlightPre.scrollLeft = textarea.scrollLeft;
  }
  // textarea の縦スクロールバーの分だけ、ミラーの右の余白を広げて折り返し幅をそろえる。
  function syncHighlightGeometry() {
    if (!highlightPre) return;
    var cs = window.getComputedStyle(textarea);
    var bars = textarea.offsetWidth - textarea.clientWidth - (parseFloat(cs.borderLeftWidth) || 0) - (parseFloat(cs.borderRightWidth) || 0);
    highlightPre.style.paddingRight = ((parseFloat(cs.paddingRight) || 0) + Math.max(0, bars)) + 'px';
    syncHighlightScroll();
  }
  function renderHighlight() {
    highlightFrame = 0;
    highlightCode.innerHTML = highlightMarkdown(textarea.value);
    syncHighlightGeometry();
  }
  function scheduleHighlight() {
    if (!highlightCode || highlightFrame) return;
    highlightFrame = window.requestAnimationFrame ? window.requestAnimationFrame(renderHighlight) : window.setTimeout(renderHighlight, 16);
  }
  if (editorWrap && highlightCode && highlightPre) {
    // input を出さずに value を書き換える処理（下書きの復元・AI の結果の反映など）でもミラーを更新する。
    var valueDesc = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value');
    if (valueDesc && valueDesc.get && valueDesc.set) {
      try {
        Object.defineProperty(textarea, 'value', {
          configurable: true,
          enumerable: valueDesc.enumerable,
          get: function () { return valueDesc.get.call(this); },
          set: function (v) { valueDesc.set.call(this, v); scheduleHighlight(); }
        });
      } catch (e) { /* 上書きできないブラウザでは input イベントだけで更新する */ }
    }
    // 日本語の変換中も input は届くので、その都度描き直す（確定時にも念のため描き直す）。
    textarea.addEventListener('input', scheduleHighlight);
    textarea.addEventListener('compositionend', scheduleHighlight);
    textarea.addEventListener('scroll', syncHighlightScroll);
    form.addEventListener('reset', function () { setTimeout(scheduleHighlight, 0); });
    if (window.ResizeObserver) new ResizeObserver(syncHighlightGeometry).observe(textarea);
    else window.addEventListener('resize', syncHighlightGeometry);
    editorWrap.classList.add('has-highlight');
    renderHighlight();
  }

  // --- 装飾ツールバー ----------------------------------------------------------
  // ボタンを押すと、カーソル位置・選択範囲に記法を入れる。execCommand('insertText') で入れて「元に戻す」で戻せるようにし、
  // 使えないブラウザでは setRangeText で入れて input を出す（プレビューと端末の下書きを更新する）。
  function replaceRange(start, end, text, selStart, selEnd) {
    textarea.focus();
    textarea.setSelectionRange(start, end);
    var ok = false;
    try { ok = document.execCommand('insertText', false, text); } catch (e) { ok = false; }
    if (!ok) {
      textarea.setRangeText(text, start, end, 'end');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    }
    textarea.setSelectionRange(start + selStart, start + selEnd);
  }

  // 行の先頭に付ける記法（見出し・箇条書き・引用）。選んだ行がすべて付いていれば外す。
  var linePrefixes = {
    heading: { add: '## ', has: /^##\s/, strip: /^#{1,6}\s+/ },
    list: { add: '- ', has: /^\s*[-*+]\s/, strip: /^(\s*)[-*+]\s+/ },
    quote: { add: '> ', has: /^>\s?/, strip: /^>\s?/ }
  };
  function toggleLinePrefix(kind) {
    var rule = linePrefixes[kind];
    var value = textarea.value;
    var start = textarea.selectionStart, end = textarea.selectionEnd;
    var lineStart = value.lastIndexOf('\n', start - 1) + 1;
    var endAt = end > start && value.charAt(end - 1) === '\n' ? end - 1 : end;
    var lineEnd = value.indexOf('\n', endAt);
    if (lineEnd < 0) lineEnd = value.length;
    var lines = value.slice(lineStart, lineEnd).split('\n');
    var target = lines.filter(function (l) { return l.trim() !== ''; });
    var remove = target.length > 0 && target.every(function (l) { return rule.has.test(l); });
    var next = lines.map(function (l) {
      if (remove) return l.replace(rule.strip, kind === 'list' ? '$1' : '');
      if (lines.length > 1 && l.trim() === '') return l;
      if (kind === 'heading') return rule.add + l.replace(rule.strip, '');
      if (kind === 'list') return l.replace(/^(\s*)/, '$1' + rule.add);
      return rule.add + l;
    }).join('\n');
    if (lines.length === 1 && start === end) replaceRange(lineStart, lineEnd, next, next.length, next.length);
    else replaceRange(lineStart, lineEnd, next, 0, next.length);
  }

  // 選択範囲を記号で挟む記法（太字・コード・数式）と、リンク。複数行のコード・数式はブロックにする。
  function wrapSelection(kind, placeholder) {
    var value = textarea.value;
    var start = textarea.selectionStart, end = textarea.selectionEnd;
    var sel = value.slice(start, end);
    var text = sel || placeholder || '';
    if (kind === 'link') {
      if (/^https?:\/\/\S+$/.test(sel)) {
        var label = placeholder || '';
        replaceRange(start, end, '[' + label + '](' + sel + ')', 1, 1 + label.length);
      } else {
        // 文字を選んでいればURLの部分を、選んでいなければ表示する文字の部分を選んだ状態にして、そのまま打ち替えられるようにする。
        var link = '[' + text + '](https://)';
        if (sel) replaceRange(start, end, link, text.length + 3, link.length - 1);
        else replaceRange(start, end, link, 1, 1 + text.length);
      }
      return;
    }
    if ((kind === 'code' || kind === 'math') && sel.indexOf('\n') >= 0) {
      var fenceMark = kind === 'code' ? '```' : '$$';
      var before = start > 0 && value.charAt(start - 1) !== '\n' ? '\n' : '';
      var after = end < value.length && value.charAt(end) !== '\n' ? '\n' : '';
      var body = sel.replace(/\n$/, '');
      var block = before + fenceMark + '\n' + body + '\n' + fenceMark + after;
      var bodyAt = before.length + fenceMark.length + 1;
      replaceRange(start, end, block, bodyAt, bodyAt + body.length);
      return;
    }
    var mark = { bold: '**', code: '`', math: '$' }[kind];
    if (sel.length > mark.length * 2 && sel.slice(0, mark.length) === mark && sel.slice(-mark.length) === mark) {
      var inner = sel.slice(mark.length, sel.length - mark.length);
      replaceRange(start, end, inner, 0, inner.length);
      return;
    }
    replaceRange(start, end, mark + text + mark, mark.length, mark.length + text.length);
  }

  var formatToolbar = document.getElementById('editor-format-toolbar');
  if (formatToolbar && !readOnly) {
    formatToolbar.addEventListener('click', function (e) {
      var btn = e.target.closest && e.target.closest('[data-md-format]');
      if (!btn || textarea.readOnly) return;
      var kind = btn.getAttribute('data-md-format');
      if (linePrefixes[kind]) toggleLinePrefix(kind);
      else wrapSelection(kind, btn.getAttribute('data-md-placeholder'));
    });
  }

  // --- スマートフォン向け: 編集 / プレビューのタブ -----------------------------
  // 画面幅が lg 未満のときだけタブを出し、選んでいない側を max-lg:hidden で隠す。lg 以上では両方を並べて表示するため、
  // プレビューのタブを選んだままウィンドウを広げても編集欄は消えない。
  var editorTabs = document.querySelectorAll('[data-editor-tab]');
  function selectEditorTab(name) {
    editorTabs.forEach(function (tab) {
      tab.setAttribute('aria-selected', tab.dataset.editorTab === name ? 'true' : 'false');
    });
    document.querySelectorAll('[data-editor-pane]').forEach(function (pane) {
      pane.classList.toggle('max-lg:hidden', pane.dataset.editorPane !== name);
    });
    if (name === 'preview') {
      clearTimeout(previewTimer);
      updatePreview();
    }
  }
  editorTabs.forEach(function (tab) {
    tab.addEventListener('click', function () {
      // 翻訳モードで「原文と比較」を表示中はプレビューの欄が隠れているため、プレビューのタブは「仕上がりプレビュー」に切り替える。
      if (tab.dataset.editorTab === 'preview' && viewMode() === 'compare') setViewMode('preview');
      selectEditorTab(tab.dataset.editorTab);
    });
  });

  // --- 翻訳モード: 「原文と比較」/「仕上がりプレビュー」の2択（admin_ux_phase5_spec.md §1.5） ----------
  // 原文バー（#source-article-banner）のボタン（data-view-mode）で、左の原文（#split-source-pane）と右の簡易プレビュー
  // （#editor-pane-preview）を入れ替える。編集欄はどちらでも出したまま。押されている側は aria-pressed で示す。
  var viewToggle = document.getElementById('translation-view-toggle');
  var splitPane = document.getElementById('split-source-pane');
  var previewPane = document.getElementById('editor-pane-preview');
  var viewButtons = viewToggle ? viewToggle.querySelectorAll('[data-view-mode]') : [];
  function viewMode() {
    if (!viewToggle || !splitPane) return '';
    return splitPane.classList.contains('hidden') ? 'preview' : 'compare';
  }
  function setViewMode(mode) {
    if (!viewToggle || !splitPane || !previewPane) return;
    var compare = mode === 'compare';
    splitPane.classList.toggle('hidden', !compare);
    previewPane.classList.toggle('hidden', compare);
    viewButtons.forEach(function (b) {
      b.setAttribute('aria-pressed', b.dataset.viewMode === mode ? 'true' : 'false');
    });
    if (compare) {
      // 狭い画面ではタブを「編集」に戻し、原文の下に編集欄を出す。
      selectEditorTab('edit');
      syncSourceScroll(textarea, sourceScroll);
    } else {
      clearTimeout(previewTimer);
      updatePreview();
    }
  }
  viewButtons.forEach(function (b) {
    b.addEventListener('click', function () { setViewMode(b.dataset.viewMode); });
  });

  // 原文と編集欄のスクロールを、全体に対する位置の割合で合わせる（「スクロールを合わせる」を外すと止まる）。
  // 相手側を動かしたことで起きる scroll イベントは1回だけ読み飛ばし、行き来して止まらなくなるのを防ぐ。
  var sourceScroll = document.getElementById('source-body-scroll');
  var scrollSyncToggle = document.getElementById('source-scroll-sync');
  var scrollEcho = null;
  function syncSourceScroll(from, to) {
    if (!from || !to || (scrollSyncToggle && !scrollSyncToggle.checked)) return;
    if (to.clientHeight === 0) return; // 隠れている側は合わせない
    var max = from.scrollHeight - from.clientHeight;
    var ratio = max > 0 ? from.scrollTop / max : 0;
    var top = Math.round(ratio * (to.scrollHeight - to.clientHeight));
    if (Math.abs(to.scrollTop - top) < 1) return;
    scrollEcho = to;
    to.scrollTop = top;
  }
  if (sourceScroll) {
    [[textarea, sourceScroll], [sourceScroll, textarea]].forEach(function (pair) {
      pair[0].addEventListener('scroll', function () {
        if (scrollEcho === pair[0]) { scrollEcho = null; return; }
        syncSourceScroll(pair[0], pair[1]);
      }, { passive: true });
    });
    if (scrollSyncToggle) scrollSyncToggle.addEventListener('change', function () { syncSourceScroll(textarea, sourceScroll); });
  }

  // --- スマートフォン向け: 画面下の操作バー -----------------------------------
  // 「下書き保存」「公開」は、上部の同じ役割のボタン（data-proxy-for の id）を押したのと同じにする（送る action・
  // formaction・予約の日時の扱いを1か所にまとめる）。対応するボタンが無ければ出さず、文言と押せる状態は写す。
  document.querySelectorAll('[data-proxy-for]').forEach(function (proxy) {
    var target = document.getElementById(proxy.dataset.proxyFor);
    if (!target) {
      proxy.remove();
      return;
    }
    proxy.textContent = target.textContent.trim();
    proxy.disabled = target.disabled;
    proxy.addEventListener('click', function () {
      if (proxy.disabled || target.disabled) return;
      target.click();
    });
  });
  var actionBar = document.getElementById('mobile-action-bar');
  if (actionBar && !actionBar.querySelector('[data-proxy-for]')) actionBar.remove();

  // --- 種類（記事 / ページ） ---------------------------------------------------
  // ページはカテゴリを持たないため、ページを選んだらカテゴリの欄を隠して説明を出す（保存後はサーバーの描画と同じ）。
  var kindSelect = document.getElementById('kind');
  var categoryField = document.getElementById('category-field');
  var kindHelp = document.getElementById('kind-help');
  if (kindSelect && categoryField) {
    kindSelect.addEventListener('change', function () {
      var isPage = kindSelect.value === 'page';
      categoryField.hidden = isPage;
      if (kindHelp) kindHelp.hidden = !isPage;
    });
  }

  // --- AI 執筆アシスタント ----------------------------------------------------
  var toolbarAI = document.getElementById('ai-toolbar');
  var aiModal = document.getElementById('ai-modal');
  if (toolbarAI && aiModal) setupAI();

  function setupAI() {
    var titleInput = document.getElementById('title');
    var excerptInput = document.getElementById('excerpt');
    var modalTitle = document.getElementById('ai-modal-title');
    var loadingText = document.getElementById('ai-loading-text');
    var errorText = document.getElementById('ai-error-text');
    var guideText = document.getElementById('ai-guide-text');
    var diffView = document.getElementById('ai-diff');
    var noChange = document.getElementById('ai-proofread-nochange');
    var scopeLabel = document.getElementById('ai-proofread-scope');
    var applyButton = document.getElementById('ai-apply-proofread');
    var titleList = document.getElementById('ai-title-list');
    var summaryText = document.getElementById('ai-summary-text');
    var summaryCount = document.getElementById('ai-summary-count');
    var factSummary = document.getElementById('ai-fact-summary');
    var factClaims = document.getElementById('ai-fact-claims');
    var factSources = document.getElementById('ai-fact-sources');
    var factSourceList = document.getElementById('ai-fact-source-list');
    var searchEntryPoint = document.getElementById('ai-search-entry-point');
    var academicText = document.getElementById('ai-academic-text');
    var applyAcademicButton = document.getElementById('ai-apply-academic');
    var slugInput = document.getElementById('slug');
    var draftTopic = document.getElementById('ai-draft-topic');
    var draftNotes = document.getElementById('ai-draft-notes');
    var draftContext = document.getElementById('ai-draft-context');
    var draftGenerateBtn = document.getElementById('ai-draft-generate-btn');
    var draftPreviewTitle = document.getElementById('ai-draft-preview-title');
    var draftPreviewSlug = document.getElementById('ai-draft-preview-slug');
    var draftPreviewSummary = document.getElementById('ai-draft-preview-summary');
    var draftPreviewContent = document.getElementById('ai-draft-preview-content');
    var applyDraftButton = document.getElementById('ai-apply-draft');
    var useRAG = document.getElementById('ai-use-rag');
    var buttons = toolbarAI.querySelectorAll('[data-ai-action]');
    var configured = toolbarAI.dataset.aiConfigured === 'true';
    // 使うAI（APIキーを登録したAIが2つ以上あるときだけ出る）。選んだAIは /admin/ai/* の要求に provider として付け、
    // 次に開いたときのためにこの端末に覚えておく（覚えられなくても、選んだ値はそのまま使える）。
    var providerSelect = document.getElementById('ai-provider-select');
    var providerStoreKey = 'sorus:ai-provider';
    if (providerSelect) {
      try {
        var remembered = localStorage.getItem(providerStoreKey);
        var known = Array.prototype.some.call(providerSelect.options, function (o) { return o.value === remembered; });
        if (remembered && known) providerSelect.value = remembered;
      } catch (_) {}
      providerSelect.addEventListener('change', function () {
        try { localStorage.setItem(providerStoreKey, providerSelect.value); } catch (_) {}
      });
    }
    function setAIProvider(body) {
      if (providerSelect && providerSelect.value) body.set('provider', providerSelect.value);
    }

    var actions = {
      'draft': { title: T('js_ai_draft'), loading: T('js_ai_draft_loading') },
      'proofread': { title: T('js_ai_proofread'), loading: T('js_ai_proofread_loading') },
      'suggest-titles': { title: T('js_ai_titles'), loading: T('js_ai_titles_loading') },
      'summarize': { title: T('js_ai_summary'), loading: T('js_ai_summary_loading') },
      'fact-check': { title: T('js_ai_factcheck'), loading: T('js_ai_factcheck_loading'), endpoint: 'fact-check' },
      'simplify': { title: T('js_ai_simplify'), loading: T('js_ai_simplify_loading'), endpoint: 'academic', academic: true },
      'translate': { title: T('js_ai_translate'), loading: T('js_ai_translate_loading'), endpoint: 'academic', academic: true }
    };
    var controller = null;
    // pending は反映待ちの推敲結果。反映時に本文が推敲前から変わっていないかを snapshot で確かめる。
    var pending = null;

    function showPane(name, title) {
      if (title) modalTitle.textContent = title;
      aiModal.querySelectorAll('[data-ai-pane]').forEach(function (el) { el.hidden = el.dataset.aiPane !== name; });
      aiModal.querySelectorAll('[data-ai-footer]').forEach(function (el) { el.hidden = el.dataset.aiFooter !== name; });
    }

    function openModal() {
      if (aiModal.open) return;
      if (typeof aiModal.showModal === 'function') aiModal.showModal();
      else aiModal.setAttribute('open', '');
    }

    function closeModal() {
      if (typeof aiModal.close === 'function') aiModal.close();
      else { aiModal.removeAttribute('open'); onClose(); }
    }

    function onClose() {
      if (controller) controller.abort();
      controller = null;
      pending = null;
      setBusy(false);
    }

    function setBusy(busy) {
      buttons.forEach(function (b) { b.disabled = busy || readOnly; });
    }

    function changeField(el, value) {
      el.value = value;
      el.dispatchEvent(new Event('input', { bubbles: true }));
    }

    function showGuide(guide) {
      if (guide) guideText.textContent = guide;
      showPane('guide', T('js_ai_guide'));
      openModal();
    }

    function run(action) {
      var meta = actions[action];
      if (readOnly) {
        window.cmsToast(T('js_ai_readonly'), 'error');
        return;
      }
      if (!configured) {
        showGuide('');
        return;
      }
      if (action === 'draft') {
        if (draftTopic && !draftTopic.value && titleInput && titleInput.value) {
          draftTopic.value = titleInput.value;
        }
        showPane('draft-input', T('js_ai_draft'));
        openModal();
        if (draftTopic) draftTopic.focus();
        return;
      }
      // 推敲は選択中の段落だけを対象にできる。タイトルとリード文は常に本文全体から作る。
      var ctx = { snapshot: textarea.value, start: 0, end: textarea.value.length, selection: false };
      if (action === 'proofread' && textarea.selectionStart !== textarea.selectionEnd) {
        ctx.start = textarea.selectionStart;
        ctx.end = textarea.selectionEnd;
        ctx.selection = true;
      }
      var text = ctx.snapshot.slice(ctx.start, ctx.end);
      if (!text.trim()) {
        window.cmsToast(T('js_ai_need_body'), 'error');
        return;
      }

      if (controller) controller.abort();
      var current = controller = new AbortController();
      pending = null;
      loadingText.textContent = meta.loading;
      showPane('loading', meta.title);
      openModal();
      setBusy(true);

      var body = new URLSearchParams();
      body.set('text', text);
      setAIProvider(body);
      body.set('use_rag', useRAG && useRAG.checked ? 'true' : 'false');
      if (articleId) body.set('article_id', articleId);
      if (action === 'translate') { body.set('action', 'translate'); body.set('target_lang', 'en'); }
      else if (action === 'simplify') body.set('action', 'simplify');
      fetch('/admin/ai/' + (meta.endpoint || action), {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
        body: body,
        credentials: 'same-origin',
        signal: current.signal
      }).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (json) {
          if (!res.ok) {
            if (res.status === 401) throw new Error(T('js_session_expired'));
            throw new Error(json.message || T('js_ai_failed_status', res.status));
          }
          return json;
        });
      }).then(function (json) {
        if (current !== controller) return;
        if (!json.configured) {
          configured = false;
          showGuide(json.guide);
          return;
        }
        if (action === 'proofread') renderProofread(json, ctx);
        else if (action === 'fact-check') renderFactCheck(json, ctx);
        else if (action === 'simplify') renderProofread(json, ctx, meta.title);
        else if (action === 'translate') renderAcademicText((json.text || (json.translation_result && json.translation_result.text) || ''), ctx, meta.title);
        else if (action === 'suggest-titles') renderTitles(json.titles || []);
        else renderSummary(json.summary || '');
      }).catch(function (err) {
        if (current !== controller || err.name === 'AbortError') return;
        errorText.textContent = err.message || T('js_ai_failed');
        showPane('error', meta.title);
      }).finally(function () {
        if (current === controller) setBusy(false);
      });
    }

    function renderProofread(json, ctx, title) {
      // diff_html はサーバーでエスケープ済みの <ins>/<del> だけを含む。
      diffView.innerHTML = json.diff_html || '';
      noChange.hidden = !!json.changed;
      scopeLabel.textContent = ctx.selection ? T('js_ai_scope_selection') : T('js_ai_scope_body');
      pending = json.changed ? { text: json.improved_text, ctx: ctx } : null;
      showPane('proofread', title || actions.proofread.title);
      applyButton.hidden = !pending;
    }

    function renderAcademicText(text, ctx, title) {
      academicText.value = text;
      pending = text ? { text: text, ctx: ctx } : null;
      showPane('academic-text', title);
      applyAcademicButton.hidden = !pending;
    }

    function renderFactCheck(json, ctx) {
      factSummary.textContent = json.summary || T('js_fact_summary_default');
      factClaims.textContent = '';
      (json.claims || []).forEach(function (claim) {
        var item = document.createElement('li');
        item.className = 'fact-claim rounded-lg border border-neutral-200 p-3';
        var badge = document.createElement('span');
        badge.className = 'fact-verdict fact-verdict-' + (['verified', 'unverified', 'inaccurate'].indexOf(claim.verdict) >= 0 ? claim.verdict : 'unverified');
        badge.textContent = { verified: T('js_fact_verified'), unverified: T('js_fact_unverified'), inaccurate: T('js_fact_inaccurate') }[claim.verdict] || T('js_fact_unverified');
        var statement = document.createElement('p');
        statement.className = 'mt-2 font-medium text-neutral-800';
        statement.textContent = claim.claim || '';
        var explanation = document.createElement('p');
        explanation.className = 'mt-1 text-sm text-neutral-600';
        explanation.textContent = claim.explanation || '';
        item.appendChild(badge);
        item.appendChild(statement);
        item.appendChild(explanation);
        if (claim.suggestion) {
          var adopt = document.createElement('button');
          adopt.type = 'button';
          adopt.className = 'btn btn-primary btn-sm mt-2';
          adopt.textContent = T('js_fact_apply');
          adopt.addEventListener('click', function () {
            if (readOnly || textarea.value !== ctx.snapshot) {
              window.cmsToast(T('js_fact_changed'), 'error');
              return;
            }
            var offset = textarea.value.indexOf(claim.claim);
            if (offset < 0) {
              window.cmsToast(T('js_fact_not_found'), 'error');
              return;
            }
            textarea.setRangeText(claim.suggestion, offset, offset + claim.claim.length, 'select');
            textarea.dispatchEvent(new Event('input', { bubbles: true }));
            closeModal();
            textarea.focus();
            window.cmsToast(T('js_fact_applied'), 'success');
          });
          item.appendChild(adopt);
        }
        (claim.sources || []).forEach(function (source) { appendSource(item, source); });
        factClaims.appendChild(item);
      });
      factSourceList.textContent = '';
      (json.sources || []).forEach(function (source) { appendSource(factSourceList, source); });
      factSources.hidden = factSourceList.childElementCount === 0;
      if (json.search_entry_point && json.grounded) {
        searchEntryPoint.srcdoc = json.search_entry_point;
        searchEntryPoint.hidden = false;
        searchEntryPoint.style.height = '110px';
      } else {
        searchEntryPoint.removeAttribute('srcdoc');
        searchEntryPoint.hidden = true;
      }
      pending = null;
      showPane('fact-check', actions['fact-check'].title);
    }

    function appendSource(parent, source) {
      if (!source || !/^https?:\/\//i.test(source.url || '')) return;
      var link = document.createElement('a');
      link.className = 'fact-source';
      link.href = source.url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.textContent = source.title || source.url;
      parent.appendChild(link);
    }

    function renderTitles(titles) {
      titleList.textContent = '';
      titles.forEach(function (t) {
        var li = document.createElement('li');
        li.className = 'flex items-center justify-between gap-3 rounded-lg border border-neutral-200 bg-white px-4 py-3 hover:border-neutral-400';
        var label = document.createElement('span');
        label.className = 'font-medium text-neutral-800';
        label.textContent = t;
        var adopt = document.createElement('button');
        adopt.type = 'button';
        adopt.className = 'btn btn-primary btn-sm shrink-0';
        adopt.textContent = T('js_title_adopt');
        adopt.addEventListener('click', function () {
          if (readOnly) return;
          changeField(titleInput, t);
          closeModal();
          window.cmsToast(T('js_title_applied'), 'success');
        });
        li.appendChild(label);
        li.appendChild(adopt);
        titleList.appendChild(li);
      });
      showPane('titles', actions['suggest-titles'].title);
    }

    function renderSummary(summary) {
      summaryText.value = summary;
      summaryCount.textContent = Array.from(summary).length;
      showPane('summary', actions.summarize.title);
    }

    applyButton.addEventListener('click', function () {
      if (!pending || readOnly) return;
      if (textarea.value !== pending.ctx.snapshot) {
        window.cmsToast(T('js_proofread_changed'), 'error');
        return;
      }
      textarea.setRangeText(pending.text, pending.ctx.start, pending.ctx.end, pending.ctx.selection ? 'select' : 'start');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
      closeModal();
      textarea.focus();
      window.cmsToast(T('js_proofread_applied'), 'success');
    });
    applyAcademicButton.addEventListener('click', function () {
      if (!pending || readOnly) return;
      if (textarea.value !== pending.ctx.snapshot) {
        window.cmsToast(T('js_generate_changed'), 'error');
        return;
      }
      textarea.setRangeText(pending.text, pending.ctx.start, pending.ctx.end, 'select');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
      closeModal();
      textarea.focus();
      window.cmsToast(T('js_generate_applied'), 'success');
    });

    if (draftGenerateBtn) {
      draftGenerateBtn.addEventListener('click', function () {
        if (readOnly) return;
        var topic = draftTopic ? draftTopic.value.trim() : '';
        if (!topic) {
          window.cmsToast(T('js_draft_topic_required'), 'error');
          if (draftTopic) draftTopic.focus();
          return;
        }
        if (controller) controller.abort();
        var current = controller = new AbortController();
        loadingText.textContent = T('js_draft_from_notes');
        showPane('loading', T('js_ai_draft'));
        setBusy(true);

        var body = new URLSearchParams();
        body.set('topic', topic);
        setAIProvider(body);
        body.set('notes', draftNotes ? draftNotes.value : '');
        body.set('context', draftContext ? draftContext.value : '');
        if (articleId) body.set('article_id', articleId);
        body.set('use_rag', useRAG && useRAG.checked ? 'true' : 'false');

        fetch('/admin/ai/draft', {
          method: 'POST',
          headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
          body: body,
          credentials: 'same-origin',
          signal: current.signal
        }).then(function (res) {
          return res.json().catch(function () { return {}; }).then(function (json) {
            if (!res.ok) {
              if (res.status === 401) throw new Error(T('js_session_expired'));
              throw new Error(json.message || T('js_ai_failed_status', res.status));
            }
            return json;
          });
        }).then(function (json) {
          if (current !== controller) return;
          if (!json.configured) {
            configured = false;
            showGuide(json.guide);
            return;
          }
          var draft = json.draft || {};
          if (draftPreviewTitle) draftPreviewTitle.value = draft.title || '';
          if (draftPreviewSlug) draftPreviewSlug.value = draft.slug || '';
          if (draftPreviewSummary) draftPreviewSummary.value = draft.summary || '';
          if (draftPreviewContent) draftPreviewContent.value = draft.content || '';
          showPane('draft-preview', T('js_ai_draft'));
        }).catch(function (err) {
          if (current !== controller || err.name === 'AbortError') return;
          errorText.textContent = err.message || T('js_ai_failed');
          showPane('error', T('js_ai_draft'));
        }).finally(function () {
          if (current === controller) setBusy(false);
        });
      });
    }

    if (applyDraftButton) {
      applyDraftButton.addEventListener('click', function () {
        if (readOnly) return;
        var hasContent = (titleInput && titleInput.value.trim()) || textarea.value.trim();
        if (!hasContent) {
          applyDraft();
          return;
        }
        window.sorusConfirm({ message: T('js_draft_overwrite_confirm') }).then(function (ok) {
          if (ok && !readOnly) applyDraft();
        });
      });
      function applyDraft() {
        if (draftPreviewTitle && titleInput && draftPreviewTitle.value.trim()) {
          changeField(titleInput, draftPreviewTitle.value.trim());
        }
        // 既存記事で既にスラッグが設定されている場合はURL変更によるリンク切れを防ぐため上書きしない
        if (draftPreviewSlug && slugInput && draftPreviewSlug.value.trim()) {
          if (!articleId || !slugInput.value.trim()) {
            changeField(slugInput, draftPreviewSlug.value.trim());
          }
        }
        if (draftPreviewSummary && excerptInput && draftPreviewSummary.value.trim()) {
          changeField(excerptInput, draftPreviewSummary.value.trim());
        }
        if (draftPreviewContent) {
          changeField(textarea, draftPreviewContent.value);
        }
        closeModal();
        textarea.focus();
        window.cmsToast(T('js_draft_applied'), 'success');
      }
    }

    summaryText.addEventListener('input', function () {
      summaryCount.textContent = Array.from(summaryText.value).length;
    });
    document.getElementById('ai-summary-excerpt').addEventListener('click', function () {
      if (readOnly || !summaryText.value.trim()) return;
      changeField(excerptInput, summaryText.value.trim());
      closeModal();
      window.cmsToast(T('js_summary_set_excerpt'), 'success');
    });
    document.getElementById('ai-summary-insert').addEventListener('click', function () {
      if (readOnly || !summaryText.value.trim()) return;
      textarea.setRangeText(summaryText.value.trim() + '\n\n', 0, 0, 'start');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
      closeModal();
      window.cmsToast(T('js_summary_inserted'), 'success');
    });

    buttons.forEach(function (b) {
      b.addEventListener('click', function () { run(b.dataset.aiAction); });
    });
    aiModal.querySelectorAll('[data-ai-close]').forEach(function (b) { b.addEventListener('click', closeModal); });
    aiModal.addEventListener('close', onClose);
    // 背景（dialog 要素自身）のクリックで閉じる。
    aiModal.addEventListener('click', function (e) { if (e.target === aiModal) closeModal(); });
  }

  // --- 未保存の変更 ------------------------------------------------------------
  // 承認・差し戻し・復元は保存済みの内容が対象のため、本文を編集中なら確認する。
  var savedState = formState();
  function formState() {
    return new URLSearchParams(new FormData(form)).toString();
  }
  function isDirty() {
    return formState() !== savedState;
  }
  // confirmUnsaved は、本文を編集中ならフォームの送信を止めて確認のモーダル（window.sorusConfirm）で確かめ、
  // 「実行する」なら押したボタンのまま送り直す（window.sorusResubmit）。送ってよいときは true を返す。
  function confirmUnsaved(e, key) {
    var f = e.target;
    if (f.sorusUnsavedConfirmed) {
      f.sorusUnsavedConfirmed = false;
      return true;
    }
    if (!isDirty()) return true;
    e.preventDefault();
    var submitter = e.submitter || null;
    window.sorusConfirm({ message: T(key) }).then(function (ok) {
      if (!ok) return;
      f.sorusUnsavedConfirmed = true;
      window.sorusResubmit(f, submitter);
      f.sorusUnsavedConfirmed = false;
    });
    return false;
  }
  ['approve-form', 'reject-form'].forEach(function (id) {
    var f = document.getElementById(id);
    if (!f) return;
    f.addEventListener('submit', function (e) { confirmUnsaved(e, 'js_unsaved_continue'); });
  });

  // --- 校閲・申し送りメモ ------------------------------------------------------
  var comments = document.getElementById('comments');
  if (comments) setupComments();

  function setupComments() {
    var list = document.getElementById('comment-list');
    var commentForm = document.getElementById('comment-form');
    var input = document.getElementById('comment-content');
    var submit = document.getElementById('comment-submit');
    var url = comments.dataset.commentsUrl;

    function show(html) {
      var atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 40;
      list.innerHTML = html; // サーバーでエスケープ済みのパーシャル。
      if (atBottom) list.scrollTop = list.scrollHeight;
    }
    function refresh() {
      if (document.hidden) return;
      fetch(url, { headers: { 'HX-Request': 'true' }, credentials: 'same-origin' })
        .then(function (res) { return res.ok ? res.text() : null; })
        .then(function (html) { if (html !== null) show(html); })
        .catch(function () { /* 次の周期で再試行する */ });
    }
    list.scrollTop = list.scrollHeight;
    // 他のメンバーのメモを自動で取り込む（HTMX の hx-trigger="every 30s" 相当）。
    setInterval(refresh, 30000);

    commentForm.addEventListener('submit', function (e) {
      e.preventDefault();
      if (!input.value.trim()) return;
      submit.disabled = true;
      var body = new URLSearchParams();
      body.set('content', input.value);
      fetch(url, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrf, 'HX-Request': 'true' },
        body: body,
        credentials: 'same-origin'
      }).then(function (res) {
        if (res.ok) return res.text();
        return res.json().catch(function () { return {}; }).then(function (json) {
          throw new Error(json.message || T('js_comment_failed'));
        });
      }).then(function (html) {
        input.value = '';
        show(html);
        list.scrollTop = list.scrollHeight;
      }).catch(function (err) {
        window.cmsToast(err.message, 'error');
      }).finally(function () { submit.disabled = false; });
    });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) commentForm.requestSubmit();
    });
  }

  // --- リビジョン履歴 ----------------------------------------------------------
  var revisionButton = document.getElementById('revision-button');
  var revisionModal = document.getElementById('revision-modal');
  if (revisionButton && revisionModal) setupRevisions();

  function setupRevisions() {
    var listPane = document.getElementById('revision-list');
    var diffPane = document.getElementById('revision-diff');
    var placeholder = diffPane.innerHTML;

    function load(url, pane) {
      return fetch(url, { headers: { 'HX-Request': 'true' }, credentials: 'same-origin' }).then(function (res) {
        if (!res.ok) throw new Error(T('js_history_failed_status', res.status));
        return res.text();
      }).then(function (html) {
        pane.innerHTML = html; // サーバーでエスケープ済みのパーシャル。
      }).catch(function (err) {
        pane.textContent = err.message;
      });
    }

    revisionButton.addEventListener('click', function () {
      diffPane.innerHTML = placeholder;
      load(revisionButton.dataset.revisionsUrl, listPane);
      if (typeof revisionModal.showModal === 'function') revisionModal.showModal();
      else revisionModal.setAttribute('open', '');
    });
    listPane.addEventListener('click', function (e) {
      var item = e.target.closest('[data-revision-diff]');
      if (!item) return;
      listPane.querySelectorAll('.revision-item').forEach(function (el) { el.classList.toggle('is-active', el === item); });
      load(item.dataset.revisionDiff, diffPane);
    });
    // 復元はフォーム送信で行い、復元後の編集画面を読み込み直す。未保存の変更は失われるため確認する。
    diffPane.addEventListener('submit', function (e) {
      if (e.target.id !== 'revision-restore-form') return;
      if (!confirmUnsaved(e, 'js_unsaved_restore')) return;
      submitting = true;
    });
    revisionModal.querySelectorAll('[data-revision-close]').forEach(function (b) {
      b.addEventListener('click', function () {
        if (typeof revisionModal.close === 'function') revisionModal.close();
        else revisionModal.removeAttribute('open');
      });
    });
    revisionModal.addEventListener('click', function (e) {
      if (e.target === revisionModal && typeof revisionModal.close === 'function') revisionModal.close();
    });
  }

  // --- 添付資料 ----------------------------------------------------------------
  var attachments = document.getElementById('attachment-list');
  if (attachments) setupAttachments();

  function setupAttachments() {
    function request(method, url, body) {
      return fetch(url, {
        method: method,
        headers: { 'X-CSRF-Token': csrf, 'HX-Request': 'true' },
        body: body,
        credentials: 'same-origin'
      }).then(function (res) {
        if (res.ok) return res.text();
        return res.json().catch(function () { return {}; }).then(function (json) {
          if (res.status === 413 && !json.message) json.message = T('js_file_too_large');
          throw new Error(json.message || T('js_attachment_failed_status', res.status));
        });
      }).then(function (html) {
        attachments.innerHTML = html; // サーバーでエスケープ済みのパーシャル。
      });
    }

    attachments.addEventListener('submit', function (e) {
      if (e.target.id !== 'attachment-form') return;
      e.preventDefault();
      var fileInput = document.getElementById('attachment-input');
      if (!fileInput.files.length) return;
      var button = document.getElementById('attachment-upload-button');
      button.disabled = true;
      button.textContent = T('js_uploading');
      var data = new FormData();
      data.append('file', fileInput.files[0]);
      request('POST', e.target.action, data).then(function () {
        window.cmsToast(T('js_attachment_added'), 'success');
      }).catch(function (err) {
        window.cmsToast(err.message, 'error');
        button.disabled = false;
        button.textContent = T('js_upload');
      });
    });

    attachments.addEventListener('click', function (e) {
      var del = e.target.closest('[data-attachment-delete]');
      if (del) {
        window.sorusConfirm({ message: T('js_attachment_delete_confirm', del.dataset.filename), danger: true }).then(function (ok) {
          if (!ok) return;
          request('DELETE', '/admin/articles/' + encodeURIComponent(articleId) + '/attachments/' + encodeURIComponent(del.dataset.attachmentDelete))
            .then(function () { window.cmsToast(T('js_attachment_deleted'), 'success'); })
            .catch(function (err) { window.cmsToast(err.message, 'error'); });
        });
        return;
      }
      var link = e.target.closest('[data-attachment-link]');
      if (link && !readOnly) {
        var label = link.dataset.filename.replace(/[\[\]\\]/g, '');
        var start = textarea.selectionStart;
        textarea.setRangeText('[' + label + '](' + link.dataset.attachmentLink + ')', start, textarea.selectionEnd, 'end');
        textarea.dispatchEvent(new Event('input', { bubbles: true }));
        textarea.focus();
      }
    });
  }

  // 新規作成画面と閲覧専用モードでは、ロックも画像アップロードも扱わない。
  if (!articleId || readOnly) return;

  // --- 排他ロック ------------------------------------------------------------
  var submitting = false;
  var released = false;

  function setReadOnly(message) {
    readOnly = true;
    ['save-button', 'publish-button'].forEach(function (id) {
      var b = document.getElementById(id);
      if (b) b.disabled = true;
    });
    document.querySelectorAll('[data-proxy-for]').forEach(function (proxy) {
      proxy.disabled = true;
    });
    textarea.readOnly = true;
    document.querySelectorAll('#ai-toolbar [data-ai-action], [data-ai-partial], #translate-article-btn, #source-partial-translate-btn').forEach(function (b) { b.disabled = true; });
    window.cmsToast(message, 'error');
  }

  function heartbeat() {
    // 通信断中はロック取得を試さず、復帰時の online イベントで直ちに再取得する。
    if (readOnly || !navigator.onLine) return;
    fetch('/admin/articles/' + encodeURIComponent(articleId) + '/lock', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
      credentials: 'same-origin'
    }).then(function (res) {
      if (res.status === 409) {
        return res.json().then(function (data) {
          clearInterval(heartbeatTimer);
          setReadOnly(data.message || T('js_lock_lost'));
        });
      }
      if (res.status === 401) {
        clearInterval(heartbeatTimer);
        window.cmsToast(T('js_session_expired'), 'error');
      }
    }).catch(function () { /* 一時的な通信断。TTL 内に次のハートビートで回復する */ });
  }
  var heartbeatTimer = setInterval(heartbeat, heartbeatMs);
  window.addEventListener('online', heartbeat);

  function releaseLock() {
    if (submitting || released || readOnly) return;
    released = true;
    var data = new FormData();
    data.append('_csrf', csrf);
    navigator.sendBeacon('/admin/articles/' + encodeURIComponent(articleId) + '/unlock', data);
  }
  // 保存の送信ではロックを手放さない（保存後に同じ編集画面へ戻るため）。
  form.addEventListener('submit', function (e) { if (e.defaultPrevented) { submitting = false; return; } submitting = true; });
  window.addEventListener('beforeunload', releaseLock);
  window.addEventListener('pagehide', releaseLock);
  // 戻る操作で bfcache から復帰したら、すぐにロックを取り直す。
  window.addEventListener('pageshow', function (e) {
    if (e.persisted) {
      released = false;
      submitting = false;
      heartbeat();
    }
  });

  // --- 画像のドラッグ＆ドロップ・貼り付け ------------------------------------
  var dropzone = document.getElementById('dropzone');
  var dropLabel = document.getElementById('dropzone-label');
  var fileInput = document.getElementById('image-input');
  var defaultLabel = dropLabel ? dropLabel.textContent : '';
  var uploading = 0;

  function insertAtCursor(text) {
    var start = textarea.selectionStart;
    var end = textarea.selectionEnd;
    var before = textarea.value.slice(0, start);
    var prefix = before && !before.endsWith('\n') ? '\n' : '';
    var insert = prefix + text + '\n';
    textarea.setRangeText(insert, start, end, 'end');
    textarea.focus();
    textarea.dispatchEvent(new Event('input'));
  }

  function setUploading(delta) {
    uploading += delta;
    if (dropLabel) dropLabel.textContent = uploading > 0 ? T('js_image_uploading', uploading) : defaultLabel;
  }

  function upload(file) {
    if (!/^image\/(jpeg|png|webp)$/.test(file.type)) {
      window.cmsToast(T('js_image_type'), 'error');
      return;
    }
    var data = new FormData();
    data.append('file', file, file.name || 'image.png');
    setUploading(1);
    fetch('/admin/media/upload', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf, 'Accept': 'application/json' },
      body: data,
      credentials: 'same-origin'
    }).then(function (res) {
      return res.json().then(function (json) {
        if (!res.ok) throw new Error(json.message || T('js_image_upload_failed'));
        return json;
      });
    }).then(function (json) {
      insertAtCursor(json.markdown);
      window.cmsToast(T('js_image_inserted'), 'success');
    }).catch(function (err) {
      window.cmsToast(err.message || T('js_image_upload_failed'), 'error');
    }).finally(function () { setUploading(-1); });
  }

  function uploadAll(files) {
    Array.prototype.forEach.call(files || [], upload);
  }

  [textarea, dropzone].forEach(function (el) {
    if (!el) return;
    el.addEventListener('dragover', function (e) {
      if (e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types, 'Files') >= 0) {
        e.preventDefault();
        el.classList.add('is-dragover');
      }
    });
    el.addEventListener('dragleave', function () { el.classList.remove('is-dragover'); });
    el.addEventListener('drop', function (e) {
      el.classList.remove('is-dragover');
      if (e.dataTransfer && e.dataTransfer.files.length > 0) {
        e.preventDefault();
        uploadAll(e.dataTransfer.files);
      }
    });
  });
  textarea.addEventListener('paste', function (e) {
    var files = e.clipboardData && e.clipboardData.files;
    if (files && files.length > 0) {
      e.preventDefault();
      uploadAll(files);
    }
  });
  if (fileInput) {
    fileInput.addEventListener('change', function () {
      uploadAll(fileInput.files);
      fileInput.value = '';
    });
  }
})();
