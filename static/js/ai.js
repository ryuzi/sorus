// 記事編集画面の AI ツールバー（templates/admin/partials/ai_modal.html の ai_toolbar）のメニュー。
// 「文章を整える ▾」「仕上げ・告知 ▾」は <details data-ai-menu> で作り、ここでは開閉だけを扱う。
// 各項目の実際の処理は static/js/editor.js（data-ai-action）と social.js（#social-open-button）が受け持つ。
// 「指示・トーンを指定して直す…」（翻訳モードでは「選択範囲を英語に翻訳・推敲」。data-ai-partial）だけはここで受け、
// 記事編集画面の window.sorusOpenPartialTranslate（templates/admin/article_edit.html）で #partial-translate-modal を開く。
(function () {
  'use strict';
  var toolbar = document.getElementById('ai-toolbar');
  if (!toolbar) return;

  // 選択範囲の AI 推敲・翻訳の画面（#partial-translate-modal）が無い画面では項目を出さない。
  // 本文の選択範囲は、メニューを開いてフォーカスが移っても textarea に残っているため、そのまま引き継がれる。
  var partialModal = document.getElementById('partial-translate-modal');
  toolbar.querySelectorAll('[data-ai-partial]').forEach(function (b) {
    if (!partialModal) return;
    b.hidden = false;
    b.addEventListener('click', function () {
      if (typeof window.sorusOpenPartialTranslate === 'function') window.sorusOpenPartialTranslate('editor');
    });
  });

  var menus = Array.prototype.slice.call(toolbar.querySelectorAll('details[data-ai-menu]'));
  if (!menus.length) return;

  function items(menu) {
    return Array.prototype.filter.call(menu.querySelectorAll('[data-ai-menu-panel] button'), function (b) {
      return !b.hidden && !b.disabled;
    });
  }

  function close(menu, focusSummary) {
    if (!menu.open) return;
    menu.open = false;
    if (focusSummary) {
      var s = menu.querySelector('summary');
      if (s) s.focus();
    }
  }

  // メニューの中身が画面の左右からはみ出さないよう、開いたときに寄せる向きを直す。ボタンは狭い画面では折り返すため、
  // 右揃えで開く「仕上げ・告知」が行の左端に来ることも、左揃えの「文章を整える」が右端に来ることもある。
  var EDGE = 8;
  function fitPanel(panel) {
    panel.style.left = '';
    panel.style.right = '';
    var vw = document.documentElement.clientWidth || window.innerWidth;
    var r = panel.getBoundingClientRect();
    if (r.right > vw - EDGE) {
      panel.style.left = 'auto';
      panel.style.right = '0';
      r = panel.getBoundingClientRect();
    }
    if (r.left < EDGE) {
      panel.style.right = 'auto';
      panel.style.left = '0';
    }
  }

  menus.forEach(function (menu) {
    var panel = menu.querySelector('[data-ai-menu-panel]');
    // 同時に開くメニューは1つだけにする。
    menu.addEventListener('toggle', function () {
      if (!menu.open) return;
      menus.forEach(function (other) { if (other !== menu) other.open = false; });
      if (panel) fitPanel(panel);
    });
    if (!panel) return;
    // 項目を押したらメニューを閉じる（処理は editor.js / social.js 側のクリックで始まる）。
    panel.addEventListener('click', function (e) {
      if (e.target.closest('button')) close(menu, false);
    });
    // メニューの中では上下キーで項目を移動できるようにする。
    menu.addEventListener('keydown', function (e) {
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      var list = items(menu);
      if (!list.length) return;
      if (!menu.open) menu.open = true;
      var i = list.indexOf(document.activeElement);
      var next = e.key === 'ArrowDown' ? (i + 1) % list.length : (i <= 0 ? list.length - 1 : i - 1);
      list[next].focus();
      e.preventDefault();
    });
  });

  // メニューの外を押したら閉じる。
  document.addEventListener('click', function (e) {
    menus.forEach(function (menu) {
      if (menu.open && !menu.contains(e.target)) close(menu, false);
    });
  });

  // Esc で閉じ、メニューのボタンへフォーカスを戻す。
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    menus.forEach(function (menu) {
      if (!menu.open) return;
      close(menu, menu.contains(document.activeElement));
      e.preventDefault();
    });
  });
})();
