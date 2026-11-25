// 管理画面共通のスクリプト: トースト通知、確認・入力のモーダル（sorusConfirm・sorusPrompt、data-confirm 付きフォーム）、
// data-copy-target 付きボタンのクリップボードへのコピー。
(function () {
  'use strict';
  // sorusT は管理画面の文言を、layout.html が埋め込む辞書（window.SORUS_I18N、partials/js_i18n.html）から引く。
  // 文言の %s は、引数で前から順に置き換える。辞書に無いキーはキー名をそのまま返す。
  window.sorusT = function (key) {
    var dict = window.SORUS_I18N || {};
    var text = Object.prototype.hasOwnProperty.call(dict, key) ? String(dict[key]) : key;
    var args = Array.prototype.slice.call(arguments, 1);
    return text.replace(/%s/g, function () { return args.length ? String(args.shift()) : ''; });
  };
  var T = window.sorusT;

  // cmsToast は画面右下にトースト通知を表示する。kind は 'success' または 'error'。
  window.cmsToast = function (message, kind, action) {
    var container = document.getElementById('toast-container');
    if (!container || !message) return;
    var el = document.createElement('div');
    el.className = 'toast is-hidden pointer-events-auto rounded-lg px-4 py-3 text-sm font-medium text-white shadow-lg ' +
      (kind === 'error' ? 'bg-rose-600' : 'bg-emerald-600');
    el.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    var text = document.createElement('span');
    text.textContent = message;
    el.appendChild(text);
    if (action && action.label) {
      var control;
      if (action.href) {
        control = document.createElement('a');
        control.href = action.href;
        control.target = '_blank';
        control.rel = 'noopener noreferrer';
      } else {
        control = document.createElement('button');
        control.type = 'button';
        control.addEventListener('click', action.onClick);
      }
      control.className = 'ml-3 inline-block rounded-md bg-white/15 px-2.5 py-1 font-semibold underline';
      control.textContent = action.label;
      el.appendChild(control);
    }
    container.appendChild(el);
    requestAnimationFrame(function () { el.classList.remove('is-hidden'); });
    setTimeout(function () {
      el.classList.add('is-hidden');
      setTimeout(function () { el.remove(); }, 400);
    }, action ? 10000 : (kind === 'error' ? 6000 : 3000));
  };

  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.content : '';
  }

  // 保存画面から始めた公開反映を見届け、成功時は公開サイトへのリンクを出す。
  // 小さなサイトの反映は1秒の確認間隔より早く終わることがあるため、「反映中」を見たかではなく、
  // 最後に終わった反映の終了日時が見始めたときから変わったかで完了を判断する。
  function watchPublish(start) {
    var baseline = null;
    var startedAt = Date.now();
    function finishedAt(status) { return (status.last && status.last.finished_at) || ''; }
    function getStatus() {
      return fetch('/admin/publish/status', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
        .then(function (r) { if (!r.ok) throw new Error('status'); return r.json(); });
    }
    function tick() {
      getStatus()
        .then(function (status) {
          if (baseline === null) baseline = finishedAt(status);
          if (!status.running && finishedAt(status) !== baseline) {
            var outcome = status.last || {};
            var action = /^https?:\/\//.test(outcome.site_url || '')
              ? { label: T('js_open_site'), href: outcome.site_url } : null;
            window.cmsToast(outcome.message || T('js_reflect_done'), outcome.ok ? 'success' : 'error', action);
            return;
          }
          if (Date.now() - startedAt > 600000) {
            window.cmsToast(T('js_reflect_status_unknown'), 'error');
            return;
          }
          setTimeout(tick, 1000);
        })
        .catch(function () {
          window.cmsToast(T('js_reflect_status_load_failed'), 'error');
        });
    }
    if (start) {
      // 反映を始める前の終了日時を基準にしてから始める。
      getStatus().then(function (status) {
        baseline = finishedAt(status);
        return fetch('/admin/publish/run', {
          method: 'POST', credentials: 'same-origin',
          headers: { Accept: 'application/json', 'X-CSRF-Token': csrfToken() }
        });
      }).then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.message || T('js_reflect_start_failed'));
          setTimeout(tick, 1000);
        });
      }).catch(function (err) { window.cmsToast(err.message || T('js_reflect_start_failed'), 'error'); });
    } else {
      // 保存後の反映は debounce（10秒）の後に始まるため、最初の確認で基準を取る。
      tick();
    }
  }

  // cmsCSRFToken はレイアウトのメタタグから CSRF トークンを返す。
  window.cmsCSRFToken = function () {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.content : '';
  };

  // cmsCopy は text をクリップボードにコピーし、結果をトーストで知らせる。
  // navigator.clipboard が使えない環境（HTTP 配信など）では選択範囲のコピーで代替する。
  window.cmsCopy = function (text, label) {
    function done() { window.cmsToast(T('js_copied', label || T('js_copy_default_label')), 'success'); }
    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      // モーダル（dialog）の中からでもコピーできるよう、開いている dialog の中に置く。
      (document.querySelector('dialog[open]') || document.body).appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      ta.remove();
      if (ok) done();
      else window.cmsToast(T('js_copy_failed'), 'error');
    }
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, fallback);
    } else {
      fallback();
    }
  };

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('[data-copy-target]');
    if (!btn) return;
    var target = document.querySelector(btn.getAttribute('data-copy-target'));
    if (target) window.cmsCopy('value' in target ? target.value : target.textContent);
  });

  // --- 確認・入力のモーダル -----------------------------------------------------
  // 管理画面ではブラウザ標準の確認・入力ダイアログを使わず、layout.html の #confirm-modal（ai-modal）で確かめる。
  // window.sorusConfirm({title, message, okLabel, danger}) は Promise<boolean> を返す（Esc・キャンセルは false）。
  // window.sorusPrompt({title, message, label, value, placeholder, okLabel, danger, suggestions, suggestionsLabel}) は
  // 入力欄付きで開き、Promise<string|null> を返す（キャンセルは null。入力欄が空のうちは実行できない）。
  // suggestions（文字列の配列）は、押すと入力欄にカンマ区切りで足す（もう一度押すと外す）候補のボタンにする。
  // 開いている間に次を開くと、前のものはキャンセルとして終わる。モーダルが無い画面では実行しない（false / null）。
  var modalPending = null;
  var modalPrompt = false;
  function modalPart(name) { return document.getElementById('confirm-modal-' + name); }
  function splitList(value) {
    return String(value || '').split(/[,，、]/).map(function (v) { return v.trim(); }).filter(Boolean);
  }
  function syncPromptState() {
    var input = modalPart('input');
    var ok = modalPart('ok');
    if (!input || !ok) return;
    ok.disabled = modalPrompt && !input.value.trim();
    var chosen = splitList(input.value);
    var box = modalPart('suggestions');
    if (!box) return;
    Array.prototype.forEach.call(box.querySelectorAll('button[data-value]'), function (b) {
      b.setAttribute('aria-pressed', chosen.indexOf(b.getAttribute('data-value')) >= 0 ? 'true' : 'false');
    });
  }
  function renderSuggestions(list, label) {
    var box = modalPart('suggestions');
    var heading = modalPart('suggestions-label');
    if (!box) return;
    box.textContent = '';
    var values = [];
    (list || []).forEach(function (v) {
      v = String(v || '').trim();
      if (v && values.indexOf(v) < 0) values.push(v);
    });
    values.forEach(function (v) {
      var b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('data-value', v);
      b.className = 'confirm-modal-chip';
      b.textContent = v;
      box.appendChild(b);
    });
    box.hidden = values.length === 0;
    if (heading) {
      heading.textContent = label || heading.getAttribute('data-default') || '';
      heading.hidden = values.length === 0;
    }
  }
  function openModal(opts, withInput) {
    opts = opts || {};
    var dlg = document.getElementById('confirm-modal');
    if (!dlg || typeof dlg.showModal !== 'function') return Promise.resolve(withInput ? null : false);
    if (modalPending) { var prev = modalPending; modalPending = null; prev(false); }
    var okBtn = modalPart('ok');
    var input = modalPart('input');
    var field = modalPart('field');
    var message = modalPart('message');
    modalPart('title').textContent = opts.title || dlg.getAttribute('data-default-title') || '';
    message.textContent = opts.message || '';
    message.hidden = !opts.message;
    okBtn.textContent = opts.okLabel || dlg.getAttribute('data-default-ok') || 'OK';
    okBtn.className = 'btn btn-md ' + (opts.danger ? 'btn-danger' : 'btn-primary');
    modalPrompt = !!withInput;
    if (field) field.hidden = !withInput;
    if (withInput) {
      modalPart('input-label').textContent = opts.label || '';
      input.value = opts.value || '';
      input.placeholder = opts.placeholder || '';
      renderSuggestions(opts.suggestions, opts.suggestionsLabel);
    }
    syncPromptState();
    dlg.returnValue = '';
    return new Promise(function (resolve) {
      modalPending = function (ok) {
        if (!withInput) { resolve(ok); return; }
        resolve(ok && input.value.trim() ? input.value : null);
      };
      if (!dlg.open) dlg.showModal();
      (withInput ? input : modalPart('cancel')).focus();
    });
  }
  window.sorusConfirm = function (opts) { return openModal(opts, false); };
  window.sorusPrompt = function (opts) { return openModal(opts, true); };
  (function () {
    var dlg = document.getElementById('confirm-modal');
    if (!dlg) return;
    dlg.addEventListener('close', function () {
      var done = modalPending;
      modalPending = null;
      if (done) done(dlg.returnValue === 'ok');
    });
    var input = modalPart('input');
    if (input) {
      input.addEventListener('input', syncPromptState);
      // フォームの Enter は先頭の「キャンセル」を押したことになるため、入力欄の Enter は「実行」にする（日本語の変換中は除く）。
      input.addEventListener('keydown', function (e) {
        if (e.key !== 'Enter' || e.isComposing || e.keyCode === 229) return;
        e.preventDefault();
        if (input.value.trim()) dlg.close('ok');
      });
    }
    var box = modalPart('suggestions');
    if (box && input) box.addEventListener('click', function (e) {
      var b = e.target.closest && e.target.closest('button[data-value]');
      if (!b) return;
      var v = b.getAttribute('data-value');
      var values = splitList(input.value);
      var at = values.indexOf(v);
      if (at >= 0) values.splice(at, 1); else values.push(v);
      input.value = values.join(', ');
      syncPromptState();
      input.focus();
    });
  })();

  // sorusResubmit は、確かめ終えたフォームを押したボタン（submitter）のまま送り直す。送り直す間だけ data-confirm の確認を
  // 飛ばし、ほかの submit の処理（fetch で送るものなど）にはふつうに渡す。画面ごとに別の確認を挟むときも、
  // 確かめたあとにこれで送り直す。
  window.sorusResubmit = function (form, submitter) {
    if (!form) return;
    form.sorusConfirmed = true;
    try {
      if (typeof form.requestSubmit === 'function') {
        if (submitter && submitter.form === form) form.requestSubmit(submitter);
        else form.requestSubmit();
      } else if (form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))) {
        if (submitter && submitter.name && submitter.form === form) {
          var extra = document.createElement('input');
          extra.type = 'hidden';
          extra.name = submitter.name;
          extra.value = submitter.value;
          form.appendChild(extra);
        }
        form.submit();
      }
    } finally {
      form.sorusConfirmed = false;
    }
  };

  // data-confirm="<確認の文>" を付けたフォーム（または送信のボタン。そのボタンで送るときだけ確かめる）は、送る前に確認の
  // モーダルで確かめる。data-confirm-title・data-confirm-ok で見出しと実行のボタンの文を変え、data-confirm-danger があるか、
  // 押したボタンが赤（btn-danger）なら実行のボタンも赤にする。確かめるまでは送信を止め、ほかの submit の処理にも渡さない
  // （キャンセルなら何もしない）。
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form || !form.getAttribute || form.sorusConfirmed) return;
    var submitter = e.submitter || null;
    var source = submitter && submitter.hasAttribute && submitter.hasAttribute('data-confirm') ? submitter : form;
    var msg = source.getAttribute('data-confirm');
    if (!msg) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    window.sorusConfirm({
      title: source.getAttribute('data-confirm-title'),
      message: msg,
      okLabel: source.getAttribute('data-confirm-ok'),
      danger: source.hasAttribute('data-confirm-danger') || !!(submitter && submitter.classList && submitter.classList.contains('btn-danger'))
    }).then(function (ok) {
      if (ok) window.sorusResubmit(form, submitter);
    });
  }, true);

  // デスクトップモード（body の data-open-external）では、管理画面はアプリ風の独立したウィンドウで開くため、外部リンクと
  // 新しいタブで開くリンクは、そのウィンドウのブラウザのタブではなく OS の既定のブラウザで開く（サーバーに頼む）。
  // 修飾キー付き・中クリック、ダウンロード、ページ内リンク、javascript: はブラウザに任せる。同じオリジンの /admin の
  // ページはログインの Cookie が要るため、既定のブラウザには渡さない。頼めなかったときだけ新しいタブで開く。
  // 左上のサイトの切り替え（data-no-external-intercept）と、同じ PC の別のポートで動くほかのサイトの管理画面（ホスト名が
  // 同じで /admin 配下）も、既定のブラウザ（Chrome など）に逃がさず、このアプリのウィンドウの中で開く。既定のブラウザで
  // 開くと、そこでもログインが要るうえ、そのブラウザが開いている画面の接続を保ち続けて sorus が終わらなくなる。
  function externalLink(e) {
    var endpoint = document.body && document.body.dataset.openExternal;
    if (!endpoint || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return null;
    var a = e.target.closest && e.target.closest('a[href]');
    if (!a || a.hasAttribute('download') || a.hasAttribute('data-no-external-intercept')) return null;
    var href = a.getAttribute('href') || '';
    if (href.charAt(0) === '#' || /^\s*javascript:/i.test(href)) return null;
    if (!/^https?:$/.test(a.protocol)) return null;
    var sameOrigin = a.origin === window.location.origin;
    if (sameOrigin && a.target !== '_blank') return null;
    // 同じオリジン、または同じホスト名で別のポート（同じ PC のほかのサイト）の /admin のページ。
    if (a.hostname === window.location.hostname && /^\/admin(\/|$)/.test(a.pathname)) return null;
    return { endpoint: endpoint, a: a };
  }

  // 「公開デザインで確認 ↗」（a[data-desktop-preview]。templates/admin/article_edit.html）は、デスクトップモードでは
  // 下書きでも OS の既定のブラウザ（Chrome など）で開く（ux_security_docs_final_polish_spec.md §1.2）。管理画面の
  // /admin/articles/{id}/preview はログインの Cookie が要るため、data-desktop-preview の URL へ POST し、サーバーが
  // 使い捨てのプレビュー用 URL を作って既定のブラウザに開かせる（開けたのは 204 のときだけ）。頼めなかったときだけ、
  // このウィンドウの新しいタブで href（保存済みの内容のプレビュー）を開く。修飾キー付き・中クリックはブラウザに任せる。
  document.addEventListener('click', function (e) {
    if (!document.body || !document.body.dataset.openExternal) return;
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest && e.target.closest('a[data-desktop-preview]');
    if (!a) return;
    var endpoint = a.getAttribute('data-desktop-preview');
    if (!endpoint) return;
    e.preventDefault();
    var href = a.href;
    fetch(endpoint, {
      method: 'POST', credentials: 'same-origin',
      headers: { Accept: 'application/json', 'X-CSRF-Token': csrfToken() }
    }).then(function (r) {
      // ログインが切れていると、ログイン画面へ送られて 200 になる。開けたのは 204 のときだけ。
      if (r.status !== 204) throw new Error('preview-open');
    }).catch(function () {
      window.open(href, '_blank', 'noopener');
    });
  });

  document.addEventListener('click', function (e) {
    var link = externalLink(e);
    if (!link) return;
    e.preventDefault();
    var href = link.a.href;
    fetch(link.endpoint, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ url: href })
    }).then(function (r) {
      // ログインが切れていると、ログイン画面へ送られて 200 になる。開けたのは 204 のときだけ。
      if (r.status !== 204) throw new Error('open-external');
    }).catch(function () {
      window.open(href, '_blank', 'noopener');
    });
  });

  // 統一ヘルプポップオーバー（.help-tip）。「?」ボタンのクリックで説明の吹き出しを開閉する。
  // 同時に開くのは1つだけ。Esc・外側のクリックで閉じ、吹き出しは画面の端からはみ出さない位置に置く。
  var openTip = null;
  var tipSeq = 0;
  var TIP_MARGIN = 8;

  function tipParts(tip) {
    return { btn: tip.querySelector('.help-tip-btn'), pop: tip.querySelector('.help-tip-pop') };
  }

  // placeTip は吹き出しをボタンの下（下に入らなければ上）に置き、左右は画面内に収める。
  function placeTip(tip) {
    var p = tipParts(tip);
    if (!p.btn || !p.pop) return;
    var vw = document.documentElement.clientWidth || window.innerWidth;
    var vh = window.innerHeight;
    var r = p.btn.getBoundingClientRect();
    p.pop.style.maxWidth = Math.max(160, Math.min(352, vw - TIP_MARGIN * 2)) + 'px';
    var w = p.pop.offsetWidth;
    var h = p.pop.offsetHeight;
    var left = Math.min(Math.max(TIP_MARGIN, r.left), vw - w - TIP_MARGIN);
    var top = r.bottom + 6;
    if (top + h > vh - TIP_MARGIN && r.top - 6 - h >= TIP_MARGIN) top = r.top - 6 - h;
    p.pop.style.left = Math.max(TIP_MARGIN, left) + 'px';
    p.pop.style.top = Math.max(TIP_MARGIN, top) + 'px';
  }

  function closeTip(returnFocus) {
    if (!openTip) return;
    var p = tipParts(openTip);
    if (p.pop) p.pop.hidden = true;
    if (p.btn) {
      p.btn.setAttribute('aria-expanded', 'false');
      if (returnFocus) p.btn.focus();
    }
    openTip = null;
  }

  function openHelpTip(tip) {
    var p = tipParts(tip);
    if (!p.btn || !p.pop) return;
    closeTip(false);
    if (!p.pop.id) p.pop.id = 'help-tip-' + (++tipSeq);
    p.btn.setAttribute('aria-controls', p.pop.id);
    p.pop.hidden = false;
    p.btn.setAttribute('aria-expanded', 'true');
    openTip = tip;
    placeTip(tip);
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('.help-tip-btn');
    if (btn) {
      // ラベルや <summary> の中に置いても、チェックボックスの切り替えや折りたたみの開閉を起こさない。
      e.preventDefault();
      e.stopPropagation();
      var tip = btn.closest('.help-tip');
      if (tip === openTip) closeTip(false);
      else if (tip) openHelpTip(tip);
      return;
    }
    if (openTip && !(e.target.closest && e.target.closest('.help-tip-pop'))) closeTip(false);
  }, true);

  document.addEventListener('keydown', function (e) {
    if (openTip && (e.key === 'Escape' || e.key === 'Esc')) {
      e.preventDefault();
      closeTip(true);
    }
  });

  window.addEventListener('resize', function () { if (openTip) placeTip(openTip); });
  window.addEventListener('scroll', function () { if (openTip) placeTip(openTip); }, true);



  document.addEventListener('DOMContentLoaded', function () {
    var container = document.getElementById('toast-container');
    if (!container) return;
    var flash = container.getAttribute('data-flash');
    var flashKey = container.getAttribute('data-flash-key');
    // ログイン画面はフォーム内に通知（#login-flash）やエラーを表示するため、トーストは重ねない。
    if (flash && !document.getElementById('login-flash') &&
        !(container.getAttribute('data-flash-error') === 'true' && document.getElementById('password'))) {
      if (flashKey === 'settings_saved_manual' || flashKey === 'structure_saved_manual') {
        window.cmsToast(T('js_settings_saved'), 'success', { label: T('js_reflect_now'), onClick: function () { watchPublish(true); } });
      } else if (flashKey === 'settings_saved_reflecting' || flashKey === 'structure_saved_reflecting') {
        window.cmsToast(T('js_settings_saved_auto'), 'success');
        watchPublish(false);
      } else {
        window.cmsToast(flash, container.getAttribute('data-flash-error') === 'true' ? 'error' : 'success');
      }
    }
  });
})();
