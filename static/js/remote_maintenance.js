// 「サイト運用」画面（settings_operations.html）の「公開サーバーのメンテナンス表示」。
// 画面を開くと公開サーバーの状態を読み込み（GET /admin/remote/maintenance）、読み込めたときだけ切り替えのボタンを出す。
// ボタンは公開サーバーが返した状態の反対（始める・終える）を、状態を指定して送る（POST。反転ではない）。
// 独自 HTML の選択はこの PC にだけ保存する（POST /admin/remote/maintenance/page）。保存だけでは公開サーバーは変わらない。
// 始めるときはサーバー側がこの PC の保存済み画面をいっしょに送る。表示中の画面の更新は「画面を反映」。
// 画面の優先順位は ① この PC で選んだ独自 HTML ② 適用中のテーマに同梱した maintenance.html ③ 標準画面。テーマ同梱の
// 画面を使う・独自 HTML を優先しているときは、その説明（local_page.source_note）を出す。「現在のメンテナンス画面を
// ダウンロード」は通常のリンク（GET /admin/remote/maintenance/page/download）で、この順で決まる画面を返す。
// 読み込めない・切り替えの結果が分からない（outcome_unknown など）ときは、前の状態を出したままにせず「分からない」に戻し、
// 「状態を確かめる」で公開サーバーに問い合わせ直してもらう。自動では問い合わせ直さず、やり直さない。
// 公開サーバーが指定と違う状態を返したとき（応答の state）は、その状態を表示する。
// サーバーから受け取った文は textContent で入れ、HTML として解釈しない。文言は画面の data-* 属性（表示言語）から読む。
(function () {
  'use strict';
  var root = document.querySelector('[data-remote-maintenance]');
  if (!root) return;
  var text = root.dataset;
  var statusBox = document.getElementById('remote-maintenance-status');
  var summary = document.getElementById('remote-maintenance-summary');
  var details = document.getElementById('remote-maintenance-details');
  var stateEl = document.getElementById('remote-maintenance-state');
  var remotePageEl = document.getElementById('remote-maintenance-remote-page');
  var pageNote = document.getElementById('remote-maintenance-page-note');
  var changedEl = document.getElementById('remote-maintenance-changed');
  var checkedEl = document.getElementById('remote-maintenance-checked');
  var resultEl = document.getElementById('remote-maintenance-result');
  var reload = document.getElementById('remote-maintenance-reload');
  var toggle = document.getElementById('remote-maintenance-toggle');
  var applyBtn = document.getElementById('remote-maintenance-apply');
  var localLabel = document.getElementById('remote-maintenance-local-label');
  var localUpdated = document.getElementById('remote-maintenance-local-updated');
  var localForm = document.getElementById('remote-maintenance-local-form');
  var localFile = document.getElementById('remote-maintenance-file');
  var localSave = document.getElementById('remote-maintenance-local-save');
  var localClear = document.getElementById('remote-maintenance-local-clear');
  var localSource = document.getElementById('remote-maintenance-local-source');
  // 配色は管理画面共通の通知（admin.css の .notice-*）に任せ、ここでは意味のクラスだけを付ける。
  var baseClass = statusBox.className.replace(/(^|\s)notice(-[a-z]+)?(?=\s|$)/g, ' ').trim();
  var tones = {
    warn: 'notice notice-warn',
    error: 'notice notice-error',
    neutral: 'notice notice-neutral'
  };
  var resultTones = {
    ok: 'notice notice-success',
    info: 'notice notice-info',
    error: 'notice notice-error'
  };
  // current は公開サーバーから読み込んだ状態。分からないときは null で、切り替えのボタンを出さない。
  var current = null;
  var localKind = text.localKind || 'default';
  // localSourceKind はこの PC の画面の実際の出どころ（custom・theme・default）。localTheme は同梱しているテーマの名前。
  var localSourceKind = text.localSource || 'default';
  var localTheme = text.localTheme || '';
  var busy = false;

  function csrf() {
    if (window.cmsCSRFToken) return window.cmsCSRFToken();
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.content : '';
  }

  function toast(msg, kind) {
    if (window.cmsToast && msg) window.cmsToast(msg, kind);
  }

  function request(method, path, body) {
    var headers = { 'Accept': 'application/json' };
    if (method !== 'GET') headers['X-CSRF-Token'] = csrf();
    if (body && !(body instanceof FormData)) {
      headers['Content-Type'] = 'application/x-www-form-urlencoded';
    }
    return fetch(path, {
      method: method, headers: headers, body: body, credentials: 'same-origin', cache: 'no-store'
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          var err = new Error(data.message || text.errorGeneric);
          err.status = res.status;
          err.data = data;
          throw err;
        }
        return data;
      });
    }, function () {
      // この PC の sorus に届かなかった。切り替えを送ったかどうかも分からないため、結果は分からないとして扱う。
      var err = new Error(text.errorLocal);
      err.status = 0;
      err.data = {};
      throw err;
    });
  }

  function setTone(tone) {
    statusBox.className = baseClass + ' ' + (tones[tone] || tones.neutral);
  }

  // setResult は操作の結果を出す。kind は ok・info（CDN など確かめていない範囲がある）・error。
  function setResult(msg, kind) {
    resultEl.textContent = msg || '';
    resultEl.hidden = !msg;
    resultEl.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    resultEl.className = 'mt-3 ' + (resultTones[kind] || resultTones.error);
  }

  function renderLocal(page) {
    if (!page) return;
    localKind = page.kind || 'default';
    localSourceKind = page.source || (localKind === 'custom' ? 'custom' : 'default');
    localTheme = page.theme_name || '';
    localLabel.textContent = page.label || '';
    localUpdated.textContent = page.updated_label || '';
    localUpdated.hidden = !page.updated_label;
    if (localSource) {
      localSource.textContent = page.source_note || '';
      localSource.hidden = !page.source_note;
    }
    // 独自 HTML を選んでいるときだけ「標準（テーマ）の画面に戻す」を出す。
    localClear.hidden = localKind !== 'custom';
    localClear.textContent = localTheme ? text.clearTheme : text.clearDefault;
  }

  function applyLocal(page) {
    renderLocal(page);
    if (!current) return;
    current.local_page = page;
    if (current.page && page) {
      // 訪問者に見える内容（種類と SHA-256）。ファイル名が違っても中身が同じなら一致。サーバーと同じ判定。
      current.page.matches_local = current.page.kind === page.kind && (current.page.sha256 || '') === (page.sha256 || '');
    }
    render(current);
  }

  // clearState は前に読み込んだ状態を画面から消し、切り替えのボタンを隠す。
  function clearState() {
    current = null;
    details.hidden = true;
    stateEl.textContent = '';
    remotePageEl.textContent = '';
    pageNote.textContent = '';
    pageNote.hidden = true;
    changedEl.textContent = '';
    changedEl.hidden = true;
    checkedEl.textContent = '';
    toggle.hidden = true;
    toggle.disabled = true;
    applyBtn.hidden = true;
    applyBtn.disabled = true;
  }

  // showUnknown は状態が分からないことを表示する（outcome_unknown・古い公開サーバー・通信の失敗など）。
  function showUnknown(msg) {
    clearState();
    setTone('error');
    summary.textContent = text.unknown;
    setResult(msg, 'error');
  }

  function render(state) {
    current = state;
    if (state.local_page) renderLocal(state.local_page);
    setTone(state.tone);
    summary.textContent = state.summary;
    stateEl.textContent = state.state_label;
    if (state.page) {
      remotePageEl.textContent = state.page.label || '';
    } else {
      remotePageEl.textContent = '';
    }
    var note = '';
    if (state.enabled && state.page && !state.page.matches_local) {
      note = text.pageMismatch;
    }
    pageNote.textContent = note;
    pageNote.hidden = !note;
    changedEl.textContent = state.changed_label || '';
    changedEl.hidden = !state.changed_label;
    checkedEl.textContent = state.checked_label || '';
    details.hidden = false;
    toggle.textContent = state.enabled ? text.stop : text.start;
    toggle.hidden = false;
    toggle.disabled = false;
    var canApply = state.enabled && !!state.page;
    applyBtn.hidden = !canApply;
    applyBtn.disabled = !canApply;
    applyBtn.textContent = text.apply;
  }

  function load() {
    busy = true;
    reload.disabled = true;
    clearState();
    setTone('neutral');
    summary.textContent = text.loading;
    setResult('', 'ok');
    return request('GET', '/admin/remote/maintenance').then(render, function (err) {
      if (err.data && err.data.local_page) renderLocal(err.data.local_page);
      showUnknown(err.message);
    }).then(function () {
      busy = false;
      reload.disabled = false;
    });
  }

  // 確認は管理画面共通の確認モーダル（window.sorusConfirm）で行う。確かめている間に状態が変わったら何もしない。
  toggle.addEventListener('click', function () {
    if (!current || busy) return;
    var start = !current.enabled;
    if (!start) {
      setMaintenance(false);
      return;
    }
    var confirmMsg = localKind === 'custom' ? text.confirmStart
      : (localSourceKind === 'theme' ? text.confirmStartTheme : text.confirmStartDefault);
    window.sorusConfirm({ message: confirmMsg }).then(function (ok) {
      if (ok && current && !busy && !current.enabled) setMaintenance(true);
    });
  });

  function setMaintenance(start) {
    var body = new URLSearchParams();
    body.set('enabled', start ? '1' : '0');
    busy = true;
    toggle.disabled = true;
    applyBtn.disabled = true;
    reload.disabled = true;
    toggle.textContent = text.saving;
    setResult('', 'ok');
    request('POST', '/admin/remote/maintenance', body).then(function (data) {
      render(data.state);
      // 始めたときは、CDN に保存済みのページなど確かめていない範囲があるため、成功の色にしない。
      setResult(data.message, data.state.enabled && data.changed ? 'info' : 'ok');
      toast(data.message, 'success');
    }, function (err) {
      var data = err.data || {};
      if (data.local_page) renderLocal(data.local_page);
      if (data.state) {
        // 公開サーバーが指定と違う状態を返した。その状態を表示する（切り替えたとは示さない）。
        render(data.state);
        setResult(err.message, 'error');
      } else {
        // 通信の失敗・古い公開サーバー・outcome（反映されたか）が分からないなど。前の状態は出したままにしない。
        showUnknown(err.message);
      }
      toast(err.message, 'error');
    }).then(function () {
      busy = false;
      reload.disabled = false;
    });
  }

  applyBtn.addEventListener('click', function () {
    if (!current || busy || !current.enabled) return;
    window.sorusConfirm({ message: text.confirmApply }).then(function (ok) {
      if (ok && current && !busy && current.enabled) applyMaintenance();
    });
  });

  function applyMaintenance() {
    busy = true;
    toggle.disabled = true;
    applyBtn.disabled = true;
    reload.disabled = true;
    applyBtn.textContent = text.applying;
    setResult('', 'ok');
    request('POST', '/admin/remote/maintenance/apply').then(function (data) {
      render(data.state);
      setResult(data.message, 'ok');
      toast(data.message, 'success');
    }, function (err) {
      var data = err.data || {};
      if (data.local_page) renderLocal(data.local_page);
      if (data.state) {
        render(data.state);
        setResult(err.message, 'error');
      } else {
        showUnknown(err.message);
      }
      toast(err.message, 'error');
    }).then(function () {
      busy = false;
      reload.disabled = false;
    });
  }

  localForm.addEventListener('submit', function (ev) {
    ev.preventDefault();
    if (busy) return;
    if (!localFile.files || !localFile.files.length) {
      setResult(text.localChoose, 'error');
      return;
    }
    var body = new FormData();
    body.append('file', localFile.files[0]);
    busy = true;
    localSave.disabled = true;
    localClear.disabled = true;
    localSave.textContent = text.localSaving;
    setResult('', 'ok');
    request('POST', '/admin/remote/maintenance/page', body).then(function (data) {
      applyLocal(data.local_page);
      localFile.value = '';
      setResult(data.message, 'ok');
      toast(data.message, 'success');
    }, function (err) {
      setResult(err.message, 'error');
      toast(err.message, 'error');
    }).then(function () {
      busy = false;
      localSave.disabled = false;
      localClear.disabled = false;
      localSave.textContent = text.localSave;
    });
  });

  localClear.addEventListener('click', function () {
    if (busy) return;
    window.sorusConfirm({ message: localTheme ? text.confirmClearTheme : text.confirmClear }).then(function (ok) {
      if (ok && !busy) clearLocalPage();
    });
  });

  function clearLocalPage() {
    var body = new URLSearchParams();
    body.set('clear', '1');
    busy = true;
    localSave.disabled = true;
    localClear.disabled = true;
    setResult('', 'ok');
    request('POST', '/admin/remote/maintenance/page', body).then(function (data) {
      applyLocal(data.local_page);
      localFile.value = '';
      setResult(data.message, 'ok');
      toast(data.message, 'success');
    }, function (err) {
      setResult(err.message, 'error');
      toast(err.message, 'error');
    }).then(function () {
      busy = false;
      localSave.disabled = false;
      localClear.disabled = false;
    });
  }

  reload.addEventListener('click', function () {
    if (!busy) load();
  });
  load();
})();
