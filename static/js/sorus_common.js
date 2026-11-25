// 管理画面の共通部品（window.sorus）。layout.html が admin.js の後、各画面のスクリプトより前に読み込む。
//
//   sorus.csrf()                       CSRF トークン
//   sorus.el(tag, cls, text)           要素を作る（text は textContent で入れ、HTML として解釈しない）
//   sorus.toast(message, kind)         画面右下の通知（admin.js の cmsToast）。message が空なら出さない
//   sorus.requestJSON(method, url, body) CSRF トークン付きでローカルの API を呼び、Promise<{ok, status, data}> を返す
//                                     （HTTP のエラーでも resolve し、つながらなかったときだけ reject する）
//   sorus.postJSON(url, body)          requestJSON('POST', url, body)
//   sorus.fmtDate(v)                   日時を管理画面の表示（日本時間の YYYY/MM/DD HH:MM。サーバーの formatTime と同じ）にする
//   sorus.diffTable(rows, opts)        左右に並べた差分の表（変わった行の前後 2 行より離れた同じ行はまとめる）
//   sorus.confirmTakeover(message)     ほかの人が PC で編集中の記事を引き継ぐか確かめる（Promise<boolean>）
//   sorus.remoteDirty.add(key)         共通バナー（#remote-dirty-banner）に未反映の項目 key を足して出す
//   sorus.remoteDirty.clear()          公開サーバーへ反映し終えたので、バナーの未反映の一覧と反映ボタンを隠す
(function () {
  'use strict';
  if (window.sorus) return;
  var T = window.sorusT || function (key) { return key; };

  function csrf() {
    if (typeof window.cmsCSRFToken === 'function') return window.cmsCSRFToken();
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.content : '';
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  function toast(message, kind) {
    if (message && typeof window.cmsToast === 'function') window.cmsToast(message, kind);
  }

  function requestJSON(method, url, body) {
    return fetch(url, {
      method: method,
      credentials: 'same-origin',
      headers: { 'X-CSRF-Token': csrf(), 'Accept': 'application/json' },
      body: body
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        return { ok: res.ok, status: res.status, data: data || {} };
      });
    });
  }

  function postJSON(url, body) {
    return requestJSON('POST', url, body);
  }

  function fmtDate(v) {
    var d = new Date(v);
    if (!v || isNaN(d.getTime())) return String(v || '');
    var pad = function (n) { return (n < 10 ? '0' : '') + n; };
    try {
      var parts = {};
      new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Tokyo', year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(d).forEach(function (p) { parts[p.type] = p.value; });
      return parts.year + '/' + parts.month + '/' + parts.day + ' ' + parts.hour + ':' + parts.minute;
    } catch (e) {
      return d.getFullYear() + '/' + pad(d.getMonth() + 1) + '/' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
    }
  }

  // --- 差分の表 ---
  // 行 r は { op: 'same'|'del'|'add'|'change', left, right }（サーバーの差分と同じ形）。左が今の内容、右が相手の内容。
  var diffClass = {
    same: ['text-neutral-600', 'text-neutral-600'],
    del: ['bg-rose-50 text-rose-900', 'bg-neutral-50'],
    add: ['bg-neutral-50', 'bg-emerald-50 text-emerald-900'],
    change: ['bg-rose-50 text-rose-900', 'bg-emerald-50 text-emerald-900']
  };

  // diffTable は rows を左右に並べた差分の表を描く。opts:
  //   tbody      描き込む既存の <tbody>（あれば中身を描き直してそれを返す。無ければ <table> を作って返す）
  //   headers    [左の見出し, 右の見出し]（新しく <table> を作るときだけ）
  //   titleRow   先頭に太字で出すタイトルの行（{op, left, right}）
  //   titleText  タイトルの文を表示用にする関数（無ければそのまま）
  //   sameText   まとめた同じ行の数 n の案内を返す関数
  //   cellClass  セルに足すクラス（例: 'w-1/2 font-mono text-xs'）
  //   context    変わった行の前後に残す同じ行の数（既定 2）
  function diffTable(rows, opts) {
    opts = opts || {};
    rows = rows || [];
    var context = opts.context == null ? 2 : opts.context;
    var extra = opts.cellClass ? ' ' + opts.cellClass : '';
    var table = null;
    var tbody = opts.tbody;
    if (tbody) {
      tbody.textContent = '';
    } else {
      table = el('table', 'mt-2 w-full table-fixed border-collapse');
      if (opts.headers) {
        var head = el('tr', '');
        opts.headers.forEach(function (h) {
          head.appendChild(el('th', 'px-2 py-1 text-left text-xs font-semibold text-neutral-600', h));
        });
        var thead = el('thead', '');
        thead.appendChild(head);
        table.appendChild(thead);
      }
      tbody = el('tbody', 'divide-y divide-neutral-100');
      table.appendChild(tbody);
    }
    function cell(text, cls) {
      return el('td', 'whitespace-pre-wrap break-words px-2 py-0.5 align-top' + extra + ' ' + cls, text);
    }
    function addRow(r, title) {
      var cls = diffClass[r.op] || diffClass.same;
      var tr = el('tr', title ? 'border-b border-neutral-300' : '');
      tr.setAttribute(title ? 'data-diff-title' : 'data-diff-op', r.op);
      var fmt = title && opts.titleText ? opts.titleText : function (s) { return s; };
      tr.appendChild(cell(fmt(r.left), cls[0] + (title ? ' font-semibold' : '')));
      tr.appendChild(cell(fmt(r.right), cls[1] + (title ? ' font-semibold' : '')));
      tbody.appendChild(tr);
    }
    if (opts.titleRow) addRow(opts.titleRow, true);
    var near = rows.map(function () { return false; });
    rows.forEach(function (r, i) {
      if (r.op === 'same') return;
      for (var k = Math.max(0, i - context); k <= Math.min(rows.length - 1, i + context); k++) near[k] = true;
    });
    var skipped = 0;
    function flush() {
      if (!skipped) return;
      var tr = el('tr', '');
      tr.setAttribute('data-diff-skipped', String(skipped));
      var td = el('td', 'px-2 py-1 text-center text-xs text-neutral-400', opts.sameText ? opts.sameText(skipped) : String(skipped));
      td.colSpan = 2;
      tr.appendChild(td);
      tbody.appendChild(tr);
      skipped = 0;
    }
    rows.forEach(function (r, i) {
      if (!near[i]) { skipped++; return; }
      flush();
      addRow(r, false);
    });
    flush();
    return table || tbody;
  }

  function confirmTakeover(message) {
    return window.sorusConfirm({ message: message || T('js_ms_load_failed') });
  }

  // --- 公開サーバーへの未反映バナー（layout.html の #remote-dirty-banner） ---
  // バナーは公開サーバーへ反映できる管理者の画面にだけある。項目の名前は #remote-dirty-items の
  // data-remote-dirty-label-<key> から、区切りは data-remote-dirty-sep から引く。
  var remoteDirty = {
    add: function (key) {
      var banner = document.getElementById('remote-dirty-banner');
      var items = document.getElementById('remote-dirty-items');
      if (!banner || !items || !key) return;
      var heading = items.firstElementChild;
      if (items.hidden && heading) {
        // 反映が済んで隠した一覧は、反映済みの項目なので作り直す。
        while (heading.nextSibling) items.removeChild(heading.nextSibling);
        items.setAttribute('data-remote-dirty', '');
      }
      if (!items.querySelector('[data-remote-dirty-item="' + key + '"]')) {
        var hasItems = !!items.querySelector('[data-remote-dirty-item]');
        items.appendChild(document.createTextNode(hasItems ? (items.getAttribute('data-remote-dirty-sep') || ', ') : ' '));
        var span = el('span', 'whitespace-nowrap', items.getAttribute('data-remote-dirty-label-' + key) || key);
        span.setAttribute('data-remote-dirty-item', key);
        items.appendChild(span);
        var keys = (items.getAttribute('data-remote-dirty') || '').split(',').filter(Boolean);
        if (keys.indexOf(key) < 0) keys.push(key);
        items.setAttribute('data-remote-dirty', keys.join(','));
      }
      items.hidden = false;
      var pushButton = document.getElementById('remote-push-settings');
      if (pushButton) pushButton.hidden = false;
      var pushResult = document.getElementById('remote-push-result');
      if (pushResult) pushResult.hidden = true;
      banner.hidden = false;
    },
    clear: function () {
      var items = document.getElementById('remote-dirty-items');
      if (items) items.hidden = true;
      var pushButton = document.getElementById('remote-push-settings');
      if (pushButton) pushButton.hidden = true;
    }
  };

  window.sorus = {
    csrf: csrf,
    el: el,
    toast: toast,
    requestJSON: requestJSON,
    postJSON: postJSON,
    fmtDate: fmtDate,
    diffTable: diffTable,
    confirmTakeover: confirmTakeover,
    remoteDirty: remoteDirty
  };
})();
