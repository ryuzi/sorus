// 「公開サーバー」画面（settings_remote.html）のスクリプト。参加ボタンを押したら接続の確認中であることを見せ、
// サイト管理者には招待の発行・発行した招待の一覧・取り消しを行う。作った招待コードはこの画面で1回だけ表示し、
// 「コピー」でクリップボードに入れる。サーバーから受け取った文は textContent で入れ、HTML として解釈しない。
// 共通部品（window.sorus。sorus_common.js）を使う。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };
  var sorus = window.sorus;
  var el = sorus.el;
  var toast = sorus.toast;

  // request は CSRF トークン付きでローカルの API を呼び（sorus.requestJSON）、JSON を返す。失敗時は応答の message を持つ
  // Error を投げる。
  function request(method, url, body) {
    return sorus.requestJSON(method, url, body).then(function (res) {
      if (!res.ok) throw new Error(res.data.message || T('js_remote_failed_status', res.status));
      return res.data;
    }, function () {
      throw new Error(T('js_remote_local_unreachable'));
    });
  }

  // --- 参加 ---
  var joinForm = document.getElementById('remote-join-form');
  if (joinForm) {
    joinForm.addEventListener('submit', function () {
      var button = document.getElementById('remote-join-button');
      if (!button) return;
      // 送信を止めないよう、ボタンの無効化は送信の後に回す。
      setTimeout(function () {
        button.disabled = true;
        button.textContent = T('js_inv_checking');
      }, 0);
    });
  }

  // --- 招待（サイト管理者） ---
  var panel = document.querySelector('[data-remote-invites]');
  if (!panel) return;
  var form = document.getElementById('remote-invite-form');
  var result = document.getElementById('remote-invite-result');
  var codeBox = document.getElementById('remote-invite-code');
  var resultTitle = document.getElementById('remote-invite-result-title');
  var copyButton = document.getElementById('remote-invite-copy');
  var tbody = document.getElementById('remote-invite-rows');
  var statusEl = document.getElementById('remote-invite-status');
  var statusClasses = {
    active: 'bg-emerald-50 text-emerald-800',
    expired: 'bg-neutral-100 text-neutral-600',
    revoked: 'bg-rose-50 text-rose-700'
  };

  function setStatus(text) {
    statusEl.textContent = text || '';
    statusEl.hidden = !text;
  }

  function renderRows(tokens) {
    tbody.textContent = '';
    if (!tokens.length) {
      setStatus(T('js_inv_none'));
      return;
    }
    setStatus('');
    tokens.forEach(function (t) {
      var tr = el('tr', 'border-t border-neutral-100 align-top');
      tr.setAttribute('data-invite-jti', t.jti);
      var name = el('td', 'py-2 pr-3 text-neutral-900', t.sub);
      if (t.current) name.appendChild(el('span', 'ml-2 rounded-full bg-zinc-800 px-2 py-0.5 text-[11px] text-white', T('js_inv_this_app')));
      tr.appendChild(name);
      tr.appendChild(el('td', 'py-2 pr-3', t.role_label));
      tr.appendChild(el('td', 'py-2 pr-3 whitespace-nowrap', t.issued_at));
      tr.appendChild(el('td', 'py-2 pr-3 whitespace-nowrap', t.expires_at));
      var st = el('td', 'py-2 pr-3');
      st.appendChild(el('span', 'rounded-full px-2 py-0.5 text-xs ' + (statusClasses[t.status] || ''), t.status_label));
      tr.appendChild(st);
      tr.appendChild(el('td', 'py-2 pr-3 text-xs text-neutral-500', t.issued_by));
      var actions = el('td', 'py-2 text-right');
      if (t.status === 'active') {
        var b = el('button', 'btn btn-danger btn-sm', T('js_inv_revoke'));
        b.type = 'button';
        b.setAttribute('data-invite-revoke', t.jti);
        b.addEventListener('click', function () { revoke(t, b); });
        actions.appendChild(b);
      }
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
  }

  function load() {
    setStatus(T('js_loading'));
    request('GET', '/admin/remote/tokens').then(function (data) {
      renderRows(data.tokens || []);
    }).catch(function (err) {
      tbody.textContent = '';
      setStatus(err.message);
    });
  }

  function revoke(t, button) {
    var msg = t.current
      ? T('js_inv_revoke_self_confirm')
      : T('js_inv_revoke_confirm', t.sub);
    // 確認は管理画面共通の確認モーダル（window.sorusConfirm）で行う。
    window.sorusConfirm({ message: msg, danger: true }).then(function (ok) {
      if (!ok || button.disabled) return;
      var body = new URLSearchParams();
      if (t.current) body.set('confirm_self', '1');
      button.disabled = true;
      request('POST', '/admin/remote/tokens/' + encodeURIComponent(t.jti) + '/revoke', body).then(function (data) {
        toast(data.message || T('js_inv_revoked'), 'success');
        load();
      }).catch(function (err) {
        toast(err.message, 'error');
        button.disabled = false;
      });
    });
  }

  function copyCode() {
    var text = codeBox.value;
    var done = function () {
      copyButton.textContent = T('js_copied_short');
      setTimeout(function () { copyButton.textContent = T('js_copy'); }, 2000);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () {
        codeBox.select();
        if (document.execCommand('copy')) done();
      });
      return;
    }
    codeBox.select();
    if (document.execCommand('copy')) done();
  }

  if (form) {
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var submit = form.querySelector('button[type="submit"]');
      submit.disabled = true;
      request('POST', '/admin/remote/invites', new URLSearchParams(new FormData(form))).then(function (data) {
        codeBox.value = data.invite_code;
        resultTitle.textContent = T('js_inv_expires', data.message, data.expires_at);
        result.hidden = false;
        codeBox.focus();
        codeBox.select();
        form.reset();
        load();
      }).catch(function (err) {
        toast(err.message, 'error');
      }).then(function () { submit.disabled = false; });
    });
  }
  if (copyButton) copyButton.addEventListener('click', copyCode);
  load();
})();
