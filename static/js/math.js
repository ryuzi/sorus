// LaTeX 数式の描画: サーバーが \( \) / \[ \] で囲んで出力した数式を KaTeX の auto-render で描画する。
// 公開ページの本文と、管理画面のプレビュー（data-math-root を付けた要素）で使う。
(function () {
  'use strict';

  var options = {
    delimiters: [
      { left: '\\[', right: '\\]', display: true },
      { left: '\\(', right: '\\)', display: false }
    ],
    // 書きかけの数式でもページ全体を壊さず、該当部分だけを赤字で示す。
    throwOnError: false,
    strict: 'ignore',
    // \href などで外部への遷移・任意の HTML を作らせない。
    trust: false
  };

  // cmsRenderMath は el の中の数式を描画する。KaTeX の読み込み前（CDN 不通を含む）は何もしない。
  window.cmsRenderMath = function (el) {
    if (!el || typeof window.renderMathInElement !== 'function') return;
    window.renderMathInElement(el, options);
  };

  document.querySelectorAll('[data-math-root]').forEach(window.cmsRenderMath);
})();
