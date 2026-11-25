// 公開サーバーのスクリプト: 連携設定画面の接続テスト・サイト設定の反映、記事エディタの下書き提出、
// 編集者向けの「公開サーバーの校閲待ち記事」一覧（プレビュー・ローカルへの取り込み・承認して本番公開・日時を決めて予約）と、
// 公開サーバーで予約中の記事の「予約を取り消す」。
// ブラウザはローカルの /admin/remote/* だけを呼び、トークンはローカル CMS のサーバー側から公開サーバーへ送られる。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };
  // 管理画面の共通バナー（layout.html）と各画面の両方から読み込まれても、ボタンの処理を二重に付けない（二重に送信しない）。
  if (window.__sorusRemoteSyncLoaded) return;
  window.__sorusRemoteSyncLoaded = true;

  // 共通の部品（CSRF・通知・要求・日時・差分の表・引き継ぎの確認）は sorus_common.js の window.sorus を使う。
  var sorus = window.sorus;
  var el = sorus.el;
  var toast = sorus.toast;
  var fmtDate = sorus.fmtDate;

  // confirmFirst は、確認の文があれば管理画面共通の確認モーダル（window.sorusConfirm）で確かめ、Promise<boolean> を返す。
  function confirmFirst(message) {
    return message ? window.sorusConfirm({ message: message }) : Promise.resolve(true);
  }

  // request は CSRF トークン付きでローカルの API を呼び（sorus.requestJSON・sorus.postJSON）、JSON を返す。
  // 失敗時は応答の message を持つ Error を投げる。
  function request(method, url, body) {
    var call = method === 'POST' ? sorus.postJSON(url, body) : sorus.requestJSON(method, url, body);
    return call.then(function (res) {
      if (!res.ok) {
        var err = new Error(res.data.message || T('js_remote_failed_status', res.status));
        err.data = res.data;
        throw err;
      }
      return res.data;
    }, function () {
      throw new Error(T('js_remote_local_unreachable'));
    });
  }

  // takeover は send(force) で操作を送り、ほかの人が PC で編集中（409・code: checked_out・need_force）なら、確かめてから
  // force=1 で送り直す（引き継ぐ）。引き継がなかったときは cancelled の付いた Error で断る（呼び出し側は知らせない）。
  function takeover(send) {
    return send(false).catch(function (err) {
      var d = err.data || {};
      if (d.code !== 'checked_out' || !d.need_force) throw err;
      return sorus.confirmTakeover(err.message).then(function (ok) {
        if (ok) return send(true);
        var stop = new Error('');
        stop.cancelled = true;
        throw stop;
      });
    });
  }

  // inboxPreviewURL は、届いた原稿 id の中身と差分を見せる「届いた原稿」の画面の URL。
  function inboxPreviewURL(id) { return '/admin/manuscripts#remote-' + encodeURIComponent(id); }

  // goReview は公開サーバーの記事 id が、この PC で取り込んだ後に変わっていた（409・code: conflict）ときに、
  // 「届いた原稿」の画面で今の内容と差分を見せる（受け取り直してから公開・予約する）。同じ画面にいれば読み込み直す。
  function goReview(id, message) {
    toast(message || T('js_remote_failed_status', 409), 'error');
    if (window.location.pathname === '/admin/manuscripts') {
      window.location.hash = 'remote-' + encodeURIComponent(id);
      window.location.reload();
      return;
    }
    window.location.href = inboxPreviewURL(id);
  }

  // --- 連携設定画面: 接続テスト ---
  var testButton = document.getElementById('remote-test-button');
  var testResult = document.getElementById('remote-test-result');
  var permissionLabels = [
    ['submit_draft', T('js_perm_submit_draft')],
    ['fetch_pending', T('js_perm_fetch_pending')],
    ['publish_article', T('js_perm_publish_article')],
    ['sync_settings', T('js_perm_sync_settings')]
  ];

  function showTestResult(data) {
    testResult.textContent = '';
    testResult.classList.remove('hidden', 'border-rose-200', 'bg-rose-50', 'text-rose-700', 'border-emerald-200', 'bg-emerald-50', 'text-emerald-800');
    if (data.ok) testResult.classList.add('border-emerald-200', 'bg-emerald-50', 'text-emerald-800');
    else testResult.classList.add('border-rose-200', 'bg-rose-50', 'text-rose-700');
    testResult.setAttribute('data-ok', data.ok ? 'true' : 'false');
    testResult.appendChild(el('p', 'font-medium', data.message || (data.ok ? T('js_connected') : T('js_connect_failed'))));
    if (data.role) {
      var meta = T('js_remote_test_meta', data.role, data.expires_at || '-');
      if (data.ok && data.latency_ms !== undefined) meta += T('js_remote_test_latency', data.latency_ms);
      testResult.appendChild(el('p', 'mt-1 text-xs', meta));
      var allowed = permissionLabels.filter(function (p) { return data.permissions && data.permissions[p[0]]; })
        .map(function (p) { return p[1]; });
      testResult.appendChild(el('p', 'mt-1 text-xs', T('js_remote_allowed', allowed.length ? allowed.join(T('js_list_separator')) : T('js_none'))));
    }
  }

  if (testButton && testResult) {
    testButton.addEventListener('click', function () {
      var body = new URLSearchParams();
      var urlInput = document.getElementById('remote_url');
      var tokenInput = document.getElementById('remote_token');
      if (urlInput) body.set('remote_url', urlInput.value);
      if (tokenInput) body.set('remote_token', tokenInput.value);
      testButton.disabled = true;
      testButton.textContent = T('js_connecting');
      request('POST', '/admin/remote/test', body).then(showTestResult).catch(function (err) {
        showTestResult({ ok: false, message: err.message });
      }).then(function () {
        testButton.disabled = false;
        testButton.textContent = T('js_connection_test');
      });
    });
  }

  // --- 共通バナー（layout.html）: サイト設定の公開サーバーへの反映（管理者） ---
  // 独自テーマはファイルごと送るため時間がかかることがある。結果（何が変わったか・変わっていないか・分からないか、
  // とその対処）はトーストが消えた後も読めるよう、ボタンの下にも残す。
  var pushButton = document.getElementById('remote-push-settings');
  var pushResult = document.getElementById('remote-push-result');
  function showPushResult(message, isError) {
    if (!pushResult) return;
    pushResult.textContent = message;
    pushResult.className = 'text-sm ' + (isError ? 'font-medium text-rose-700' : 'text-neutral-700');
    pushResult.setAttribute('role', isError ? 'alert' : 'status');
    pushResult.hidden = false;
  }
  if (pushButton) {
    var pushLabel = pushButton.textContent;
    // 確認は管理画面共通の確認モーダル（layout.html の #confirm-modal、window.sorusConfirm）で行い、
    // 「反映する」を押したときだけ反映する。
    pushButton.addEventListener('click', function () {
      if (pushButton.disabled) return;
      var msg = pushButton.getAttribute('data-confirm-message');
      if (!msg) {
        pushSettings();
        return;
      }
      // 実行のボタンには、押したボタンと同じ「公開サーバーへ反映する」（表示言語に合わせた文）を出す。
      window.sorusConfirm({ message: msg, okLabel: pushLabel.trim() }).then(function (ok) {
        if (ok) pushSettings();
      });
    });
  }
  function pushSettings() {
    pushButton.disabled = true;
    pushButton.textContent = pushButton.getAttribute('data-running-label') || pushLabel;
    if (pushResult) pushResult.hidden = true;
    request('POST', '/admin/remote/settings/push').then(function (data) {
      var done = data.message || T('js_pushed');
      toast(done, 'success');
      showPushResult(done, false);
      // 共通バナーの「未反映」の一覧は反映が済んだので消し、結果の文だけを残す。
      sorus.remoteDirty.clear();
    }).catch(function (err) {
      toast(err.message, 'error');
      showPushResult(err.message, true);
    }).then(function () {
      pushButton.disabled = false;
      pushButton.textContent = pushLabel;
    });
  }

  // --- 記事エディタ: 公開サーバーへ提出 ---
  // 公開サーバーの記事が、この PC で取得した後に変わっていたら（409・code: conflict）、差分の画面（#remote-conflict-modal）で
  // 「サーバー版を取り込む」か「この PC の内容で上書き提出」（server_updated_at を base_updated_at に付けて送り直す）を選ぶ。
  // ほかの人が PC で編集中（code: checked_out）なら、確かめてから force=1 で引き継いで提出する。
  function submitArticle(button, id, extra) {
    var label = button.textContent;
    button.disabled = true;
    button.textContent = T('js_submitting');
    var body = new URLSearchParams();
    Object.keys(extra || {}).forEach(function (k) { body.set(k, extra[k]); });
    return request('POST', '/admin/remote/articles/' + encodeURIComponent(id) + '/submit', body).then(function (data) {
      toast(data.message || T('js_submitted_draft'), 'success');
      // 公開サーバーがスラッグを付け直したら、この画面の入力欄も合わせる（次の保存で元に戻さない）。
      var slug = document.querySelector('input[name="slug"]');
      if (data.slug_adopted && slug && data.article && data.article.slug) slug.value = data.article.slug;
    }).catch(function (err) {
      var d = err.data || {};
      if (d.code === 'conflict') { showConflict(button, id, d); return; }
      if (d.code === 'checked_out' && d.need_force) {
        return sorus.confirmTakeover(err.message).then(function (ok) {
          if (ok) {
            button.disabled = false;
            button.textContent = label;
            var next = Object.assign({}, extra || {}, { force: '1' });
            return submitArticle(button, id, next);
          }
        });
      }
      toast(err.message, 'error');
    }).then(function () {
      button.disabled = false;
      button.textContent = label;
    });
  }

  // showConflict は 409 の応答 d（左がこの PC、右が公開サーバー）を差分の画面に描いて開く。
  function showConflict(button, id, d) {
    var modal = document.getElementById('remote-conflict-modal');
    if (!modal) { toast(d.message, 'error'); return; }
    var rowsEl = modal.querySelector('[data-remote-conflict-rows]');
    modal.querySelector('[data-remote-conflict-left]').textContent = modal.getAttribute('data-label-local');
    modal.querySelector('[data-remote-conflict-right]').textContent = modal.getAttribute('data-label-server');
    var meta = [];
    if (d.server_author_name) meta.push(d.server_author_name);
    if (d.server_updated_at) meta.push(fmtDate(d.server_updated_at));
    modal.querySelector('[data-remote-conflict-meta]').textContent = meta.join(' · ');
    sorus.diffTable(d.rows, {
      tbody: rowsEl,
      titleRow: d.title_row,
      sameText: function (n) { return (modal.getAttribute('data-label-same') || '%s').replace('%s', n); }
    });

    var close = function () { if (typeof modal.close === 'function') modal.close(); else modal.removeAttribute('open'); };
    // ボタンは開くたびに付け直す（前の衝突の server_updated_at を使わない）。
    ['[data-remote-conflict-close]', '[data-remote-conflict-pull]', '[data-remote-conflict-overwrite]'].forEach(function (sel) {
      var b = modal.querySelector(sel);
      var fresh = b.cloneNode(true);
      b.replaceWith(fresh);
    });
    modal.querySelector('[data-remote-conflict-close]').addEventListener('click', close);
    modal.querySelector('[data-remote-conflict-overwrite]').addEventListener('click', function () {
      close();
      submitArticle(button, id, { base_updated_at: d.server_updated_at });
    });
    var pull = modal.querySelector('[data-remote-conflict-pull]');
    pull.addEventListener('click', function () {
      window.sorusConfirm({ message: modal.getAttribute('data-pull-confirm') }).then(function (ok) {
        if (!ok) return;
        pull.disabled = true;
        request('POST', '/admin/remote/pending/' + encodeURIComponent(id) + '/pull-latest').then(function (data) {
          toast(data.message || T('js_imported'), 'success');
          close();
          window.location.reload();
        }).catch(function (err) {
          toast(err.message, 'error');
        }).then(function () { pull.disabled = false; });
      });
    });
    if (typeof modal.showModal === 'function') modal.showModal();
    else modal.setAttribute('open', '');
  }

  document.addEventListener('click', function (e) {
    var button = e.target.closest && e.target.closest('[data-remote-submit]');
    if (!button || button.disabled) return;
    var id = button.getAttribute('data-remote-submit');
    if (!id) return;
    confirmFirst(button.getAttribute('data-confirm-message')).then(function (ok) {
      if (!ok || button.disabled) return;
      submitArticle(button, id, {});
    });
  });

  // --- 編集者: 日時を決めて予約・予約を取り消す（「届いた原稿」と、下の校閲待ち記事の一覧で共通） ---
  // 予約・取り消しが済んだら remote-sync:changed を送り、一覧を読み直させる。data-reload の付いた部品
  // （サーバーで描いた「届いた原稿」の画面）では、画面ごと読み込み直す。
  function changed(source) {
    if (source.closest && source.closest('[data-reload]')) {
      window.location.reload();
      return;
    }
    document.dispatchEvent(new CustomEvent('remote-sync:changed'));
  }

  document.addEventListener('submit', function (e) {
    var form = e.target.closest && e.target.closest('form[data-remote-schedule-form]');
    if (!form) return;
    e.preventDefault();
    var id = form.getAttribute('data-remote-schedule-form');
    var input = form.querySelector('input[name="publish_at"]');
    var button = form.querySelector('button[type="submit"]');
    if (!id || !input || (button && button.disabled)) return;
    confirmFirst(form.getAttribute('data-confirm-message')).then(function (ok) {
      if (!ok || (button && button.disabled)) return;
      // 入力欄の日時は日本時間のまま送り、この PC の sorus が UTC にして公開サーバーへ送る。
      var publishAt = input.value;
      if (button) button.disabled = true;
      takeover(function (force) {
        var body = new URLSearchParams();
        body.set('publish_at', publishAt);
        if (force) body.set('force', '1');
        return request('POST', '/admin/remote/pending/' + encodeURIComponent(id) + '/schedule', body);
      }).then(function (data) {
        toast(data.message || T('js_scheduled'), 'success');
        var box = form.closest('details');
        if (box) box.open = false;
        changed(form);
      }).catch(function (err) {
        if (err.cancelled) return;
        if (err.data && err.data.code === 'conflict') { goReview(id, err.message); return; }
        toast(err.message, 'error');
      }).then(function () { if (button) button.disabled = false; });
    });
  });

  document.addEventListener('click', function (e) {
    var button = e.target.closest && e.target.closest('[data-remote-unschedule]');
    if (!button || button.disabled) return;
    var id = button.getAttribute('data-remote-unschedule');
    if (!id) return;
    confirmFirst(button.getAttribute('data-confirm-message')).then(function (ok) {
      if (!ok || button.disabled) return;
      button.disabled = true;
      request('POST', '/admin/remote/scheduled/' + encodeURIComponent(id) + '/unschedule').then(function (data) {
        toast(data.message || T('js_unscheduled'), 'success');
        changed(button);
      }).catch(function (err) {
        toast(err.message, 'error');
      }).then(function () { button.disabled = false; });
    });
  });

  // --- 公開先（公開サーバー）: 公開中の記事・添付ファイルの一括同期（管理者。handlers_remote_bulk.go） ---
  // 送信はサーバーの中で続く。ここでは始める要求を送り、進み具合（GET /admin/remote/bulk-sync）を読んで表示するだけ。
  var bulk = document.getElementById('remote-bulk-sync');
  if (bulk) {
    var bulkStart = document.getElementById('remote-bulk-start');
    var bulkProgress = document.getElementById('remote-bulk-progress');
    var bulkBar = document.getElementById('remote-bulk-bar');
    var bulkText = document.getElementById('remote-bulk-text');
    var bulkResult = document.getElementById('remote-bulk-result');
    var bulkActive = bulk.getAttribute('data-bulk-active') === 'true';
    var bulkTimer = null;

    var bulkResultText = function (st) {
      switch (st.outcome) {
        case 'ok': return T('js_bulk_result_ok', st.articles, st.attachments, st.images, st.media);
        case 'partial': return T('js_bulk_result_partial', st.articles, st.failures.length);
        case 'timeout': return T('js_bulk_result_stopped', st.done, st.total) + ' ' + T('js_bulk_stopped_timeout');
        default: return T('js_bulk_result_stopped', st.done, st.total) + (st.stop_reason ? ' ' + st.stop_reason : '');
      }
    };

    var showBulkResult = function (st) {
      bulkResult.textContent = '';
      var box = el('div', 'notice ' + (st.outcome === 'ok' ? 'notice-success' : 'notice-warn'));
      box.setAttribute('data-bulk-result', st.outcome === 'ok' ? 'ok' : 'partial');
      box.appendChild(el('p', 'font-semibold', bulkResultText(st)));
      var lines = (st.notes || []).map(function (key) { return T(key); });
      if (lines.length) {
        var notes = el('ul', 'mt-2 list-disc space-y-1 pl-5');
        lines.forEach(function (line) { notes.appendChild(el('li', '', line)); });
        box.appendChild(notes);
      }
      if (st.failures && st.failures.length) {
        var list = el('ul', 'mt-2 list-disc space-y-1 pl-5');
        st.failures.forEach(function (f) {
          var li = el('li');
          li.appendChild(el('span', 'font-medium', f.kind === 'gallery' ? T('js_bulk_gallery') : (f.title || T('js_untitled'))));
          li.appendChild(document.createTextNode(': ' + (f.kind === 'attachments' ? T('js_bulk_attachments_failed') + ' ' : '') + f.message));
          list.appendChild(li);
        });
        box.appendChild(list);
      }
      if (st.finished_text) box.appendChild(el('p', 'mt-2 text-xs opacity-75', T('js_bulk_finished_at', st.finished_text)));
      bulkResult.appendChild(box);
      bulkResult.hidden = false;
    };

    var showBulk = function (st) {
      bulkProgress.hidden = !st.running;
      bulkBar.style.width = (st.percent || 0) + '%';
      var bar = bulkBar.parentNode;
      if (bar) bar.setAttribute('aria-valuenow', String(st.percent || 0));
      // 公開サーバーの受付上限で待っているあいだは、止まったように見えないよう待ち時間を添える。
      bulkText.textContent = st.running ? T('js_bulk_running', st.done, st.total) + (st.current ? ' — ' + st.current : '') +
        (st.waiting ? ' ' + T('js_bulk_waiting', st.waiting) : '') : '';
      bulkStart.disabled = !!st.running || !bulkActive;
      if (st.running) {
        bulkResult.hidden = true;
      } else if (st.started) {
        showBulkResult(st);
      }
    };

    var pollBulk = function () {
      clearTimeout(bulkTimer);
      request('GET', '/admin/remote/bulk-sync').then(function (st) {
        showBulk(st);
        if (st.running) {
          bulkTimer = setTimeout(pollBulk, 1500);
        } else if (st.started) {
          toast(bulkResultText(st), st.outcome === 'ok' ? 'success' : 'error');
        }
      }).catch(function (err) {
        bulkText.textContent = T('js_bulk_status_failed') + ' ' + err.message;
        bulkTimer = setTimeout(pollBulk, 5000);
      });
    };

    bulkStart.addEventListener('click', function () {
      window.sorusConfirm({
        title: bulkStart.getAttribute('data-confirm-title') || '',
        message: bulkStart.getAttribute('data-confirm-message') || '',
        okLabel: bulkStart.getAttribute('data-confirm-ok') || ''
      }).then(function (ok) {
        if (!ok || bulkStart.disabled) return;
        bulkStart.disabled = true;
        request('POST', '/admin/remote/bulk-sync').then(function (st) {
          showBulk(st);
          bulkTimer = setTimeout(pollBulk, 1000);
        }).catch(function (err) {
          toast(err.message || T('js_bulk_start_failed'), 'error');
          bulkStart.disabled = !bulkActive;
        });
      });
    });
    if (bulk.getAttribute('data-bulk-running') === 'true') pollBulk();
  }

  // --- 編集者: 公開サーバーの校閲待ち記事 ---
  var panel = document.querySelector('[data-remote-pending]');
  if (!panel) return;

  var list = panel.querySelector('[data-remote-pending-list]');
  var countEl = panel.querySelector('[data-remote-pending-count]');
  var statusEl = panel.querySelector('[data-remote-pending-status]');
  var refreshButton = panel.querySelector('[data-remote-pending-refresh]');
  var modal = document.getElementById('remote-sync-modal');
  var modalTitle = document.getElementById('remote-sync-modal-title');
  var modalMeta = document.getElementById('remote-sync-modal-meta');
  var modalExcerpt = document.getElementById('remote-sync-modal-excerpt');
  var modalBody = document.getElementById('remote-sync-modal-body');
  var modalImport = document.getElementById('remote-sync-modal-import');
  var modalPublish = document.getElementById('remote-sync-modal-publish');
  var modalSchedule = document.getElementById('remote-sync-modal-schedule');
  var scheduleNote = panel.querySelector('[data-remote-schedule-note]');
  var scheduledBox = panel.querySelector('[data-remote-scheduled]');
  var scheduledList = panel.querySelector('[data-remote-scheduled-list]');
  var scheduledCount = panel.querySelector('[data-remote-scheduled-count]');
  var scheduleTemplate = panel.querySelector('template[data-remote-schedule-template]');
  var articles = [];
  var scheduled = [];
  // schedule は「日時を決めて予約」を出すか（available）と、入力欄の初期値・最小値。予約に対応していない公開サーバーでは
  // 選択肢を出さず、note の案内を出す（即時公開に切り替えたりはしない）。
  var schedule = {};
  var current = null;
  var localStatusLabels = { draft: T('js_local_draft'), review: T('js_local_review'), published: T('js_local_published'), scheduled: T('js_local_scheduled'), deleted: T('js_local_deleted') };

  function setStatus(text, link) {
    statusEl.textContent = text || '';
    if (link) {
      statusEl.appendChild(document.createTextNode(' '));
      var a = el('a', 'underline', link);
      a.href = link;
      a.target = '_blank';
      a.rel = 'noopener';
      statusEl.appendChild(a);
    }
    statusEl.hidden = !text;
  }

  function metaText(a) {
    var parts = [];
    if (a.author_name) parts.push(T('js_meta_author', a.author_name));
    if (a.category_name) parts.push(T('js_meta_category', a.category_name));
    if (a.updated_at) parts.push(T('js_meta_updated', a.updated_at));
    return parts.join(' / ');
  }

  // setupPicker は「日時を決めて予約」の入力欄 root を記事 a 用にする。
  function setupPicker(root, a) {
    var form = root.querySelector('form[data-remote-schedule-form]');
    if (!form) return;
    form.setAttribute('data-remote-schedule-form', a.id);
    var confirmText = panel.getAttribute('data-schedule-confirm') || '';
    form.setAttribute('data-confirm-message', confirmText.replace('%s', a.title || T('js_untitled')));
    var input = form.querySelector('input[name="publish_at"]');
    if (input) {
      input.value = schedule.default || '';
      if (schedule.min) input.min = schedule.min;
    }
  }

  function schedulePicker(a) {
    if (!schedule.available || !scheduleTemplate) return null;
    var frag = scheduleTemplate.content.cloneNode(true);
    setupPicker(frag, a);
    return frag;
  }

  function renderScheduled() {
    if (scheduleNote) {
      scheduleNote.textContent = schedule.note || '';
      scheduleNote.hidden = !schedule.note;
    }
    if (!scheduledBox) return;
    scheduledList.textContent = '';
    scheduledCount.textContent = String(scheduled.length);
    scheduledBox.hidden = !scheduled.length;
    scheduled.forEach(function (a) {
      var li = el('li', 'flex flex-wrap items-center justify-between gap-2 py-2.5');
      li.setAttribute('data-remote-scheduled-item', a.id);
      var info = el('div', 'min-w-0');
      var line = el('p', 'flex flex-wrap items-center gap-2 text-sm');
      line.appendChild(el('span', 'status-scheduled rounded-full px-2 py-0.5 text-xs font-medium', a.label));
      line.appendChild(el('span', 'font-medium text-neutral-900', a.title || T('js_untitled')));
      info.appendChild(line);
      var meta = metaText({ author_name: a.author_name, category_name: a.category_name });
      if (meta) info.appendChild(el('p', 'text-xs text-neutral-500', meta));
      var cancel = el('button', 'btn btn-secondary btn-sm',
        panel.getAttribute('data-unschedule-label') || T('js_unschedule'));
      cancel.type = 'button';
      cancel.setAttribute('data-remote-unschedule', a.id);
      cancel.setAttribute('data-confirm-message', a.unschedule_confirm || '');
      li.appendChild(info);
      li.appendChild(cancel);
      scheduledList.appendChild(li);
    });
  }

  function renderList() {
    countEl.textContent = String(articles.length);
    list.textContent = '';
    renderScheduled();
    if (!articles.length) {
      setStatus(T('js_no_pending'));
      return;
    }
    articles.forEach(function (a) {
      var li = el('li', 'flex flex-wrap items-center justify-between gap-2 py-2.5');
      li.setAttribute('data-remote-article', a.id);
      var info = el('div', 'min-w-0');
      var title = el('button', 'block max-w-full truncate text-left text-sm font-medium text-neutral-900 hover:underline', a.title || T('js_untitled'));
      title.type = 'button';
      title.addEventListener('click', function () { openModal(a); });
      info.appendChild(title);
      var meta = el('p', 'text-xs text-neutral-500', metaText(a));
      if (a.local_status) {
        meta.appendChild(document.createTextNode(' '));
        meta.appendChild(el('span', 'rounded-full bg-neutral-100 px-2 py-0.5 text-neutral-600', localStatusLabels[a.local_status] || a.local_status));
      }
      info.appendChild(meta);
      var actions = el('div', 'flex shrink-0 flex-wrap items-start gap-2');
      var view = el('button', 'btn btn-secondary btn-sm', T('js_view'));
      view.type = 'button';
      view.addEventListener('click', function () { openModal(a); });
      var importBtn = el('button', 'btn btn-secondary btn-sm', T('js_import_local'));
      importBtn.type = 'button';
      importBtn.addEventListener('click', function () { importArticle(a, importBtn); });
      var publishBtn = el('button', 'btn btn-primary btn-sm', T('js_approve_publish_live'));
      publishBtn.type = 'button';
      publishBtn.addEventListener('click', function () { publishArticle(a, publishBtn); });
      actions.appendChild(view);
      actions.appendChild(importBtn);
      actions.appendChild(publishBtn);
      var picker = schedulePicker(a);
      if (picker) actions.appendChild(picker);
      li.appendChild(info);
      li.appendChild(actions);
      list.appendChild(li);
    });
    setStatus('');
  }

  function load() {
    refreshButton.disabled = true;
    setStatus(T('js_loading'));
    request('GET', '/admin/remote/pending').then(function (data) {
      articles = data.articles || [];
      scheduled = data.scheduled || [];
      schedule = data.schedule || {};
      renderList();
    }).catch(function (err) {
      countEl.textContent = '-';
      list.textContent = '';
      scheduled = [];
      schedule = {};
      renderScheduled();
      setStatus(err.message);
    }).then(function () { refreshButton.disabled = false; });
  }

  function openModal(a) {
    if (!modal) return;
    current = a;
    modalTitle.textContent = a.title || T('js_untitled');
    modalMeta.textContent = metaText(a);
    modalExcerpt.textContent = a.excerpt || '';
    modalExcerpt.hidden = !a.excerpt;
    // html はローカルの sorus が Markdown から生成し、サニタイズ済みのプレビュー。
    modalBody.innerHTML = a.html || '';
    modalImport.disabled = false;
    modalPublish.disabled = false;
    if (modalSchedule) {
      modalSchedule.hidden = !schedule.available;
      var box = modalSchedule.querySelector('details');
      if (box) box.open = false;
      setupPicker(modalSchedule, a);
    }
    if (typeof modal.showModal === 'function') modal.showModal();
    else modal.setAttribute('open', '');
  }

  function closeModal() {
    current = null;
    if (!modal) return;
    if (typeof modal.close === 'function') modal.close();
    else modal.removeAttribute('open');
  }

  function removeArticle(id) {
    articles = articles.filter(function (a) { return a.id !== id; });
    renderList();
  }

  function importArticle(a, button) {
    // この PC に同じ記事があれば、差分を確かめてから上書きできる「届いた原稿」の画面で受け取る。
    if (a.local_status) {
      window.location.href = inboxPreviewURL(a.id);
      return;
    }
    button.disabled = true;
    takeover(function (force) {
      var body = new URLSearchParams();
      if (force) body.set('force', '1');
      return request('POST', '/admin/remote/pending/' + encodeURIComponent(a.id) + '/import', body);
    }).then(function (data) {
      toast(data.message || T('js_imported'), data.images_failed ? 'error' : 'success');
      var done = function () {
        a.local_status = data.status || 'review';
        closeModal();
        renderList();
      };
      if (!data.edit_url) { done(); return; }
      return window.sorusConfirm({ message: T('js_imported_open_editor') }).then(function (open) {
        if (open) {
          window.location.href = data.edit_url;
          return;
        }
        done();
      });
    }).catch(function (err) {
      if (err.cancelled) return;
      if (err.data && err.data.need_confirm) {
        window.location.href = inboxPreviewURL(a.id);
        return;
      }
      toast(err.message, 'error');
    }).then(function () { button.disabled = false; });
  }

  function publishArticle(a, button) {
    window.sorusConfirm({ message: T('js_publish_remote_confirm', a.title || T('js_untitled')) }).then(function (ok) {
      if (!ok || button.disabled) return;
      button.disabled = true;
      takeover(function (force) {
        var body = new URLSearchParams();
        if (force) body.set('force', '1');
        return request('POST', '/admin/remote/pending/' + encodeURIComponent(a.id) + '/publish', body);
      }).then(function (data) {
        toast(data.message || T('js_published'), 'success');
        closeModal();
        removeArticle(a.id);
        if (data.url) setStatus(T('js_published_colon'), data.url);
      }).catch(function (err) {
        if (err.cancelled) return;
        if (err.data && err.data.code === 'conflict') { goReview(a.id, err.message); return; }
        toast(err.message, 'error');
      }).then(function () { button.disabled = false; });
    });
  }

  refreshButton.addEventListener('click', load);
  document.addEventListener('remote-sync:changed', function () {
    closeModal();
    load();
  });
  if (modal) {
    modal.querySelectorAll('[data-remote-modal-close]').forEach(function (b) { b.addEventListener('click', closeModal); });
    modalImport.addEventListener('click', function () { if (current) importArticle(current, modalImport); });
    modalPublish.addEventListener('click', function () { if (current) publishArticle(current, modalPublish); });
  }
  load();
})();
