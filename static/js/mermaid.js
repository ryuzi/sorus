// Mermaid の図の描画: サーバーが <pre class="mermaid"> に入れて出力した図の定義（internal/markdown/mermaid.go）を、
// 同梱の Mermaid（static/vendor/mermaid/mermaid.min.js。外部の配信元は使わない）で SVG にする。
// 公開ページの本文と、管理画面のプレビューで使う。Mermaid 本体は大きいため、このスクリプトの data-mermaid-src に書いた
// URL から、図があるときだけ読み込む（ページの表示・操作を待たせない）。
// 描いた SVG は pre の直後に足す div.mermaid-diagram に入れ、pre（図の定義）は隠して残す（配色が変わったら描き直す）。
// 読み込めないとき・定義に誤りがあるときは、図の定義のテキスト（pre）をそのまま見せる。
(function () {
  'use strict';

  var SELECTOR = 'pre.mermaid';
  var DIAGRAM_CLASS = 'mermaid-diagram';
  var current = document.currentScript;
  var vendorSrc = current ? current.getAttribute('data-mermaid-src') : '';
  var darkQuery = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  var seq = 0;
  var loading = null;
  var configuredTheme = '';

  // themeName は図の配色（Mermaid のテーマ）を返す。ページが暗い配色を使えるか（<meta name="color-scheme"> の値。
  // 公開サイトはテーマの color_scheme、管理画面は明るい配色だけで meta なし）と、OS の配色（prefers-color-scheme）で決める。
  function themeName() {
    var meta = document.querySelector('meta[name="color-scheme"]');
    var tokens = ((meta && meta.getAttribute('content')) || '').toLowerCase().split(/\s+/);
    var dark = tokens.indexOf('dark') >= 0;
    var light = tokens.indexOf('light') >= 0;
    if (dark && !light) return 'dark';
    if (dark && darkQuery && darkQuery.matches) return 'dark';
    return 'default';
  }

  function configure(mermaid) {
    var theme = themeName();
    if (theme === configuredTheme) return;
    configuredTheme = theme;
    mermaid.initialize({
      startOnLoad: false,
      // 図の文字の HTML をサニタイズし、click による任意のスクリプト・リンクを許さない。
      securityLevel: 'strict',
      theme: theme,
      // 定義の誤りは Mermaid のエラー図ではなく、定義のテキストをそのまま見せる（書きかけのプレビューも読める）。
      suppressErrorRendering: true,
      fontFamily: window.getComputedStyle(document.body).fontFamily || undefined
    });
  }

  // loadMermaid は Mermaid 本体を1回だけ読み込み、読み込めたら window.mermaid を渡す。
  function loadMermaid() {
    if (window.mermaid) return Promise.resolve(window.mermaid);
    if (loading) return loading;
    if (!vendorSrc) return Promise.reject(new Error('mermaid: data-mermaid-src is missing'));
    loading = new Promise(function (resolve, reject) {
      var s = document.createElement('script');
      s.src = vendorSrc;
      s.async = true;
      s.onload = function () {
        if (window.mermaid) resolve(window.mermaid);
        else reject(new Error('mermaid: not loaded'));
      };
      s.onerror = function () {
        loading = null;
        reject(new Error('mermaid: failed to load ' + vendorSrc));
      };
      document.head.appendChild(s);
    });
    return loading;
  }

  // diagramFor は pre の図を入れる div を返す（無ければ pre の直後に作る）。
  function diagramFor(el) {
    var div = el.sorusMermaidDiagram;
    if (div && div.parentNode === el.parentNode) return div;
    div = document.createElement('div');
    div.className = DIAGRAM_CLASS;
    el.parentNode.insertBefore(div, el.nextSibling);
    el.sorusMermaidDiagram = div;
    return div;
  }

  function renderOne(mermaid, el) {
    // pre の中身は HTML エスケープした図の定義だけ（サーバーでサニタイズ済み）。textContent で元の定義に戻す。
    var src = el.textContent;
    if (!src || !src.trim() || !el.parentNode) return Promise.resolve();
    var id = 'sorus-mermaid-' + (++seq);
    el.setAttribute('aria-busy', 'true');
    return mermaid.render(id, src).then(function (result) {
      el.removeAttribute('aria-busy');
      // プレビューの描き直しなどで、描画中に要素が外されていたら何もしない。
      if (!el.isConnected) return;
      var div = diagramFor(el);
      // SVG は Mermaid が securityLevel: 'strict' でサニタイズしたもの。
      div.innerHTML = result.svg;
      div.hidden = false;
      el.hidden = true;
      el.classList.add('is-rendered');
      el.classList.remove('is-error');
      if (typeof result.bindFunctions === 'function') result.bindFunctions(div);
    }, function (err) {
      el.removeAttribute('aria-busy');
      if (!el.isConnected) return;
      if (el.sorusMermaidDiagram) el.sorusMermaidDiagram.hidden = true;
      el.hidden = false;
      el.classList.remove('is-rendered');
      el.classList.add('is-error');
      if (window.console && console.warn) console.warn('mermaid:', err && err.message ? err.message : err);
    });
  }

  function renderNodes(nodes) {
    if (!nodes.length) return Promise.resolve();
    return loadMermaid().then(function (mermaid) {
      configure(mermaid);
      return Promise.all(nodes.map(function (el) { return renderOne(mermaid, el); }));
    }, function (err) {
      if (window.console && console.warn) console.warn(err && err.message ? err.message : err);
    });
  }

  function findIn(root) {
    var nodes = [];
    if (!root) return nodes;
    if (root.matches && root.matches(SELECTOR)) nodes.push(root);
    if (root.querySelectorAll) Array.prototype.push.apply(nodes, root.querySelectorAll(SELECTOR));
    return nodes;
  }

  // cmsRenderMermaid は root の中のまだ描いていない図を描画する（管理画面のプレビューは描き直すたびに呼ぶ）。
  window.cmsRenderMermaid = function (root) {
    return renderNodes(findIn(root || document).filter(function (el) {
      return !el.classList.contains('is-rendered') && !el.hasAttribute('aria-busy');
    }));
  };

  // OS の配色が変わったら、描いた図を新しい配色で描き直す。
  function onSchemeChange() {
    if (!window.mermaid || themeName() === configuredTheme) return;
    renderNodes(findIn(document).filter(function (el) { return el.classList.contains('is-rendered'); }));
  }
  if (darkQuery) {
    if (darkQuery.addEventListener) darkQuery.addEventListener('change', onSchemeChange);
    else if (darkQuery.addListener) darkQuery.addListener(onSchemeChange);
  }

  window.cmsRenderMermaid(document);
})();
