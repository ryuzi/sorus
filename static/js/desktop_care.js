// ダッシュボードの data-desktop-part を持つ要素に、その URL の部分（自動バックアップの状態・更新のお知らせ）を読み込み、
// 更新のお知らせの「コピー」「閉じる」を受け持つ。コピーしたあとの文言はボタンの data-copied-label（表示言語の文言。
// updateBannerTemplate が入れる）を使い、このスクリプトには文言を書かない。共通部品（window.sorus。sorus_common.js）を使う。
(function () {
  'use strict';
  document.querySelectorAll('[data-desktop-part]').forEach(function (box) {
    fetch(box.getAttribute('data-desktop-part'), { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.text() : ''; })
      .then(function (html) { box.innerHTML = html; })
      .catch(function () {});
  });
  function copied(btn) {
    var label = btn.textContent;
    var done = btn.getAttribute('data-copied-label');
    if (!done) return;
    btn.textContent = done;
    setTimeout(function () { btn.textContent = label; }, 2000);
  }
  document.addEventListener('click', function (e) {
    var copy = e.target.closest('[data-copy-target]');
    if (copy) {
      var src = document.getElementById(copy.getAttribute('data-copy-target'));
      if (!src) return;
      var text = src.textContent;
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () { copied(copy); }, function () {});
      } else {
        var range = document.createRange();
        range.selectNodeContents(src);
        var sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        try { document.execCommand('copy'); copied(copy); } catch (err) {}
      }
      return;
    }
    var dismiss = e.target.closest('[data-dismiss-update]');
    if (dismiss) {
      var banner = document.getElementById('update-banner');
      if (banner) banner.hidden = true;
      fetch(dismiss.getAttribute('data-dismiss-update'), {
        method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': window.sorus.csrf() }
      }).catch(function () {});
    }
  });
})();
