// 「サイト運用」画面（settings_operations.html）の「公開サーバーの閲覧制限」のスクリプト。
// 画面を開くと公開サーバーの状態を読み込み（GET /admin/remote/site-access）、読み込めたときだけフォームを使えるようにする。
// 読み込めなければ「分からない」と表示し、保護されているとは表示しない。保存（POST）に失敗して反映されたか分からないときも、
// 前の状態を出したままにせず「分からない」に戻す。確かめた URL がすべてパスワードを求めた（checked）ときも、確かめたのは
// 一部の URL だけのため、成功の色・成功のトーストにはしない（成功のトーストは無効にしたときだけ）。この PC から変更できない
// とき（状態の can_save が false、または保存が blocked で断られたとき。公開サーバーの URL が https でない、公開サーバーが
// この接続を TLS で直接受けていない）は、理由を表示して保存のフォームを使えなくし、入力したパスワードを消す。
// サーバーから受け取った文は textContent で入れ、HTML として解釈しない。
// 文言は画面の data-* 属性（表示言語）から読む。共通部品（window.sorus。sorus_common.js）を使う。
(function () {
  'use strict';
  var root = document.querySelector('[data-remote-site-access]');
  if (!root) return;
  var text = root.dataset;
  var statusBox = document.getElementById('remote-site-access-status');
  var summary = document.getElementById('remote-site-access-summary');
  var details = document.getElementById('remote-site-access-details');
  var stateEl = document.getElementById('remote-site-access-state');
  var userEl = document.getElementById('remote-site-access-user');
  var passEl = document.getElementById('remote-site-access-password');
  var checksEl = document.getElementById('remote-site-access-checks');
  var checkedEl = document.getElementById('remote-site-access-checked');
  var resultEl = document.getElementById('remote-site-access-result');
  var reload = document.getElementById('remote-site-access-reload');
  var form = document.getElementById('remote-site-access-form');
  var fields = document.getElementById('remote-site-access-fields');
  var submit = document.getElementById('remote-site-access-submit');
  var hint = document.getElementById('remote_access_pass_hint');
  var blockedEl = document.getElementById('remote-site-access-blocked');
  var enabled = form.elements.namedItem('enabled');
  var user = form.elements.namedItem('user');
  var pass = form.elements.namedItem('password');
  var revision = form.elements.namedItem('revision');
  // 配色は管理画面共通の通知（admin.css の .notice-*）に任せ、ここでは意味のクラスだけを付ける。
  var baseClass = statusBox.className.replace(/(^|\s)notice(-[a-z]+)?(?=\s|$)/g, ' ').trim();
  var tones = {
    ok: 'notice notice-success',
    warn: 'notice notice-warn',
    error: 'notice notice-error',
    info: 'notice notice-info',
    neutral: 'notice notice-neutral'
  };
  var resultTones = {
    ok: 'notice notice-success',
    info: 'notice notice-info',
    error: 'notice notice-error'
  };
  var current = null;

  var toast = window.sorus.toast;

  // request は公開サーバーの閲覧制限の API を呼ぶ（sorus.requestJSON）。サーバーは Cache-Control: no-store を返す。
  function request(method, body) {
    return window.sorus.requestJSON(method, '/admin/remote/site-access', body).then(function (res) {
      if (!res.ok) {
        var err = new Error(res.data.message || text.errorGeneric);
        err.status = res.status;
        err.data = res.data;
        throw err;
      }
      return res.data;
    }, function () {
      var err = new Error(text.errorLocal);
      err.status = 0;
      err.data = {};
      throw err;
    });
  }

  function setTone(tone) {
    statusBox.className = baseClass + ' ' + (tones[tone] || tones.neutral);
  }

  // setResult は操作の結果を出す。kind は ok（無効にした）・info（確かめた範囲だけ）・error。
  function setResult(msg, kind) {
    resultEl.textContent = msg || '';
    resultEl.hidden = !msg;
    resultEl.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    resultEl.className = 'mt-3 ' + (resultTones[kind] || resultTones.error);
  }

  // clearState は前に読み込んだ状態を画面から消す。分からないときに古い状態（有効・確かめた結果）を残さない。
  function clearState() {
    current = null;
    details.hidden = true;
    blockedEl.hidden = true;
    blockedEl.textContent = '';
    checkedEl.textContent = '';
    while (checksEl.firstChild) checksEl.removeChild(checksEl.firstChild);
    form.hidden = true;
    fields.disabled = true;
    form.reset();
    revision.value = '';
  }

  // showUnknown は状態が分からないことを表示する。版が無いと安全に保存できないため、フォームは出さない。
  function showUnknown(msg) {
    clearState();
    setTone('error');
    summary.textContent = text.unknown;
    setResult(msg, 'error');
  }

  // block はこの PC から変更できない理由を表示し、保存のフォームを使えなくする。入力したパスワードは送れないため消す。
  function block(msg) {
    if (current) current.can_save = false;
    blockedEl.textContent = msg || text.errorGeneric;
    blockedEl.hidden = false;
    pass.value = '';
    fields.disabled = true;
  }

  function render(state) {
    current = state;
    setTone(state.tone);
    summary.textContent = state.summary;
    stateEl.textContent = state.state_label;
    userEl.textContent = state.user || text.none;
    passEl.textContent = state.password_label;
    while (checksEl.firstChild) checksEl.removeChild(checksEl.firstChild);
    (state.checks || []).forEach(function (ch) {
      var li = document.createElement('li');
      li.textContent = ch.label;
      li.setAttribute('data-check-result', ch.result);
      checksEl.appendChild(li);
    });
    checkedEl.textContent = state.checked_label;
    details.hidden = false;
    enabled.checked = !!state.enabled;
    user.value = state.user || '';
    pass.value = '';
    pass.placeholder = state.password_set ? text.passKeep : '';
    hint.textContent = state.password_set ? text.hintSet : text.hintNew;
    revision.value = state.revision || '';
    form.hidden = false;
    if (state.can_save === false) {
      block(state.save_blocked);
    } else {
      blockedEl.hidden = true;
      blockedEl.textContent = '';
      fields.disabled = false;
    }
  }

  function load() {
    reload.disabled = true;
    clearState();
    setTone('neutral');
    summary.textContent = text.loading;
    setResult('', 'ok');
    return request('GET').then(render, function (err) {
      showUnknown(err.message);
    }).then(function () { reload.disabled = false; });
  }

  // 閲覧制限を OFF にするときは、管理画面共通の確認モーダル（window.sorusConfirm）で確かめてから保存する。
  form.addEventListener('submit', function (e) {
    e.preventDefault();
    if (!current || current.can_save === false) return;
    if (!(current.enabled && !enabled.checked)) {
      save();
      return;
    }
    window.sorusConfirm({ message: text.confirmDisable, danger: true }).then(function (ok) {
      if (ok && current && current.can_save !== false && !fields.disabled) save();
    });
  });

  function save() {
    var body = new URLSearchParams();
    body.set('enabled', enabled.checked ? '1' : '0');
    body.set('user', user.value);
    body.set('password', pass.value);
    body.set('revision', revision.value);
    fields.disabled = true;
    reload.disabled = true;
    submit.textContent = text.saving;
    setResult('', 'ok');
    request('POST', body).then(function (data) {
      render(data.state);
      var status = data.state.status;
      if (status === 'off') {
        setResult(data.message, 'ok');
        toast(data.message, 'success');
      } else if (status === 'checked') {
        // 確かめた URL はすべてパスワードを求めたが、確かめていない URL（CDN のキャッシュなど）がある。成功とは示さない。
        setResult(data.message, 'info');
      } else {
        setResult(data.message, 'error');
        toast(data.message, 'error');
      }
    }, function (err) {
      var data = err.data || {};
      if (data.conflict && data.state) {
        render(data.state);
        setResult(err.message, 'error');
      } else if (err.status === 422 && data.blocked) {
        // この接続では変更できない（何も変わっていない）。入力を直しても保存できないため、フォームを使えなくする。
        block(err.message);
      } else if (err.status === 422) {
        // 入力の誤り。何も変わっていないため、入力した内容を残して直してもらう。
        fields.disabled = false;
        setResult(err.message, 'error');
      } else {
        // 公開サーバーとの通信の失敗など。反映されたか分からないこともあるため、前の状態は出したままにしない。
        showUnknown(err.message);
      }
      toast(err.message, 'error');
    }).then(function () {
      submit.textContent = text.save;
      reload.disabled = false;
    });
  }

  reload.addEventListener('click', load);
  load();
})();
