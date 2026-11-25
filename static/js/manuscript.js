// 「届いた原稿」の画面（templates/admin/manuscripts.html）のスクリプト。
//
// 公開サーバーの原稿を「この PC に受け取る」で /admin/remote/pending/:id/preview から中身と差分を
// 読んで見せ、確かめてから /admin/remote/pending/:id/import で取り込む（同じ記事がこの PC にあれば overwrite=1 を
// 付ける）。#remote-<記事 ID> で開くと、その原稿の中身を最初から見せる（ダッシュボードの一覧から来たとき）。
// 公開サーバーで作成中の下書き（data-draft）は「この PC で続きを書く」でそのまま取り込んで編集画面を開き、この PC に同じ記事が
// あるときだけ差分を見せる。ほかの人が編集中の記事は「引き継ぐ」（data-force）で、確かめてから force=1 を付けて取り込む。
// 変更案は「差分を見て反映」で、行の <template data-proposal-template> を #manuscript-previews に複製し、
// /admin/remote/pending/:id/proposal に accept・discard を送る。
// サーバーから受け取った文は textContent で入れ、HTML として解釈しない（本文のプレビューだけは、サーバーが
// markdown.Render でサニタイズした HTML を入れる）。共通の部品（CSRF・通知・要求・差分の表）は sorus_common.js の window.sorus を使う。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };

  var sorus = window.sorus;
  var el = sorus.el;
  var toast = sorus.toast;
  var post = sorus.postJSON;

  function failMessage(res) {
    if (res.data && res.data.message) return res.data.message;
    return T('js_ms_load_failed');
  }

  // needForce は応答 res が、ほかの人が PC で編集中（409・code: checked_out・need_force）で断られたかを返す。
  function needForce(res) {
    return res.status === 409 && res.data.code === 'checked_out' && !!res.data.need_force;
  }

  // confirmTakeover は編集中の人の編集を引き継ぐか（force=1 で送り直すか）を確かめる。
  function confirmTakeover(res) {
    return sorus.confirmTakeover(failMessage(res));
  }

  // --- 差分 ---

  // renderDiff は今の記事（左）と届いた原稿（右）を並べる。変わった行の前後 2 行より離れた同じ行はまとめる。
  function renderDiff(ex) {
    return sorus.diffTable(ex.rows, {
      headers: [T('js_ms_local_article'), T('js_ms_received')],
      titleRow: ex.title_row,
      titleText: function (s) { return T('js_ms_title_prefix', s); },
      sameText: function (n) { return T('js_ms_same_lines', n); },
      cellClass: 'w-1/2 font-mono text-xs'
    });
  }

  // --- 届いた原稿の受け取り ---

  function primaryButton(text) {
    return el('button', 'btn btn-primary btn-md', text);
  }

  function secondaryButton(text) {
    return el('button', 'btn btn-secondary btn-md', text);
  }

  function showError(card, name, msg) {
    card.textContent = '';
    card.setAttribute('data-manuscript-state', 'error');
    card.className = 'notice notice-error';
    card.appendChild(el('p', 'font-semibold', T('js_ms_named_load_failed', name)));
    card.appendChild(el('p', 'mt-1', msg));
    var close = secondaryButton(T('js_close'));
    close.type = 'button';
    close.className += ' mt-3';
    close.addEventListener('click', function () { card.remove(); });
    card.appendChild(close);
  }

  // renderPreview は届いた原稿の中身 p を card に描く。send(overwrite) は受け取りを送る関数（post の結果を返す）、
  // done(data) は受け取れたときに呼ぶ関数（無くてもよい）。
  function renderPreview(card, p, send, done, draft) {
    card.textContent = '';
    card.setAttribute('data-manuscript-state', 'preview');
    card.className = 'rounded-xl border border-zinc-300 bg-white p-4 shadow-sm';
    card.appendChild(el('p', 'text-xs font-medium text-neutral-500', T('js_ms_from_server')));
    card.appendChild(el('h3', 'mt-1 text-lg font-bold text-neutral-900', p.title));
    var meta = [];
    if (p.author_name) meta.push(T('js_ms_author', p.author_name));
    meta.push(T('js_ms_submitted_at', p.created_at));
    meta.push(T('js_ms_images', p.images));
    if (p.kind === 'page') meta.push(T('js_ms_kind_page'));
    card.appendChild(el('p', 'mt-1 text-xs text-neutral-500', meta.join(T('js_ms_meta_separator'))));
    card.appendChild(el('p', 'mt-2 text-sm text-neutral-700', p.author_note));

    var ex = p.existing;
    if (ex) {
      var box = el('div', 'mt-4 notice ' + (ex.need_confirm ? 'notice-warn' : 'notice-neutral'));
      box.setAttribute('data-manuscript-existing', '');
      var head = el('p', 'text-sm font-semibold text-neutral-900', T('js_ms_existing', ex.status_label, ex.updated_at));
      box.appendChild(head);
      if (ex.reason) {
        var reason = el('p', 'mt-1 text-sm text-amber-900', ex.reason);
        reason.setAttribute('data-manuscript-reason', '');
        box.appendChild(reason);
      }
      if (!ex.changed) box.appendChild(el('p', 'mt-1 text-sm text-neutral-600', T('js_ms_same_content')));
      else box.appendChild(renderDiff(ex));
      var open = el('a', 'mt-2 inline-block text-xs text-neutral-600 underline', T('js_ms_open_local'));
      open.href = ex.edit_url;
      open.target = '_blank';
      open.rel = 'noopener';
      box.appendChild(open);
      card.appendChild(box);
    }

    var details = el('details', 'mt-4');
    if (!ex) details.open = true;
    details.appendChild(el('summary', 'cursor-pointer select-none text-sm font-medium text-neutral-700', T('js_ms_body_preview')));
    var body = el('div', 'prose prose-neutral mt-2 max-h-96 max-w-none overflow-y-auto rounded-lg border border-neutral-200 p-4');
    body.innerHTML = p.html;
    details.appendChild(body);
    card.appendChild(details);

    var actions = el('div', 'mt-4 flex flex-wrap items-center gap-2');
    var overwrite = !!(ex && ex.need_confirm);
    var accept = primaryButton(overwrite ? T('js_ms_overwrite') : (draft ? T('js_ms_continue') : (ex ? T('js_ms_accept_update') : T('js_ms_accept'))));
    accept.type = 'button';
    accept.setAttribute('data-manuscript-accept', overwrite ? 'overwrite' : 'new');
    accept.addEventListener('click', function () { acceptManuscript(card, send, done, overwrite, accept, draft, false); });
    var cancel = secondaryButton(T('js_ms_cancel'));
    cancel.type = 'button';
    cancel.addEventListener('click', function () { card.remove(); });
    actions.appendChild(accept);
    actions.appendChild(cancel);
    card.appendChild(actions);
  }

  // acceptManuscript は届いた原稿を受け取る。下書き（draft）は受け取れたらそのまま編集画面を開く。ほかの人が編集中なら、
  // 確かめてから force=1 で送り直す（引き継ぐ）。
  function acceptManuscript(card, send, done, overwrite, btn, draft, force) {
    btn.disabled = true;
    send(overwrite, force).then(function (res) {
      if (res.ok) {
        if (done) done(res.data);
        toast(res.data.message, 'success');
        if (draft && res.data.edit_url) {
          location.href = res.data.edit_url;
          return;
        }
        card.textContent = '';
        card.setAttribute('data-manuscript-state', 'accepted');
        card.className = 'notice notice-success';
        card.appendChild(el('p', 'font-semibold', res.data.message));
        if (!draft) card.appendChild(el('p', 'mt-1', T('js_ms_check_and_publish')));
        var link = el('a', 'btn btn-primary btn-md mt-3', draft ? T('js_ms_continue') : T('js_ms_open_review'));
        link.href = res.data.edit_url;
        card.appendChild(link);
        return;
      }
      btn.disabled = false;
      if (needForce(res) && !force) {
        confirmTakeover(res).then(function (ok) {
          if (ok) acceptManuscript(card, send, done, overwrite, btn, draft, true);
        });
        return;
      }
      if (res.status === 409 && res.data.need_confirm && !overwrite) {
        // 確認の間に記事が変わったときは、上書きの確認に切り替える。
        btn.textContent = T('js_ms_overwrite');
        btn.setAttribute('data-manuscript-accept', 'overwrite');
        var again = btn.cloneNode(true);
        btn.replaceWith(again);
        again.addEventListener('click', function () { acceptManuscript(card, send, done, true, again, draft, force); });
      }
      toast(failMessage(res), 'error');
    }).catch(function () {
      btn.disabled = false;
      toast(T('js_ms_receive_failed'), 'error');
    });
  }

  function newCard(list, text) {
    var card = el('article', 'rounded-xl border border-neutral-200 bg-white p-4 text-sm text-neutral-600', text);
    card.setAttribute('data-manuscript-card', '');
    list.prepend(card);
    return card;
  }

  // previewRemote は公開サーバーに届いた原稿 id の中身と差分を見せる。受け取れたら、一覧のボタン btn を
  // 「開いて校閲する」（下書きは「この PC で続きを書く」）に変える。行に編集画面へのリンクが既にあれば、btn を外すだけにする。
  function previewRemote(list, id, btn, opts) {
    opts = opts || {};
    var base = '/admin/remote/pending/' + encodeURIComponent(id);
    list.querySelectorAll('[data-remote-card]').forEach(function (c) {
      if (c.getAttribute('data-remote-card') === id) c.remove();
    });
    var card = newCard(list, T('js_ms_loading'));
    card.setAttribute('data-remote-card', id);
    card.scrollIntoView({ block: 'nearest' });
    function send(overwrite, force) {
      var body = new URLSearchParams();
      if (overwrite) body.append('overwrite', '1');
      if (opts.force || force) body.append('force', '1');
      return post(base + '/import', body);
    }
    function done(data) {
      if (!btn || !btn.parentNode) return;
      var row = btn.closest('[data-inbox-item]');
      if (row && row.querySelector('a[data-inbox-edit][href]')) {
        btn.remove();
        return;
      }
      var link = el('a', 'btn btn-primary btn-sm', opts.draft ? T('js_ms_continue') : T('js_ms_open_review'));
      link.href = data.edit_url;
      btn.replaceWith(link);
    }
    post(base + '/preview', new URLSearchParams()).then(function (res) {
      if (res.ok) renderPreview(card, res.data, send, done, opts.draft);
      else showError(card, T('js_ms_server_manuscript'), failMessage(res));
    }).catch(function () {
      showError(card, T('js_ms_server_manuscript'), T('js_ms_load_failed'));
    });
  }

  // continueDraft は公開サーバーの下書き id をこの PC に取り込み、編集画面を開く。この PC に同じ記事があれば
  // （need_confirm）差分を見せる画面に切り替える。
  function continueDraft(list, id, btn, force) {
    btn.disabled = true;
    var body = new URLSearchParams();
    if (force) body.append('force', '1');
    post('/admin/remote/pending/' + encodeURIComponent(id) + '/import', body).then(function (res) {
      if (res.ok) {
        toast(res.data.message, 'success');
        if (res.data.edit_url) location.href = res.data.edit_url;
        return;
      }
      btn.disabled = false;
      if (needForce(res) && !force) {
        confirmTakeover(res).then(function (ok) { if (ok) continueDraft(list, id, btn, true); });
        return;
      }
      if (res.status === 409 && res.data.need_confirm) {
        previewRemote(list, id, btn, { force: force, draft: true });
        return;
      }
      toast(failMessage(res), 'error');
    }).catch(function () {
      btn.disabled = false;
      toast(T('js_ms_receive_failed'), 'error');
    });
  }

  // openProposal は記事 id の変更案の差分（行の <template>）を list の先頭に出し、反映・破棄のボタンを動かす。
  function openProposal(list, id) {
    var tpl = document.querySelector('template[data-proposal-template="' + id + '"]');
    if (!tpl) return;
    list.querySelectorAll('[data-proposal-card]').forEach(function (c) {
      if (c.getAttribute('data-proposal-card') === id) c.remove();
    });
    var frag = tpl.content.cloneNode(true);
    var card = frag.querySelector('[data-proposal-card]');
    list.prepend(frag);
    card.scrollIntoView({ block: 'nearest' });
    card.querySelector('[data-proposal-close]').addEventListener('click', function () { card.remove(); });
    card.querySelectorAll('[data-proposal-resolve]').forEach(function (b) {
      b.addEventListener('click', function () {
        if (b.disabled) return;
        card.querySelectorAll('[data-proposal-resolve]').forEach(function (x) { x.disabled = true; });
        var body = new URLSearchParams();
        body.append('proposal_action', b.getAttribute('data-proposal-resolve'));
        body.append('proposal_id', b.getAttribute('data-proposal-id'));
        post('/admin/remote/pending/' + encodeURIComponent(id) + '/proposal', body).then(function (res) {
          if (!res.ok) {
            card.querySelectorAll('[data-proposal-resolve]').forEach(function (x) { x.disabled = false; });
            toast(failMessage(res), 'error');
            return;
          }
          toast(res.data.message, 'success');
          location.reload();
        }).catch(function () {
          card.querySelectorAll('[data-proposal-resolve]').forEach(function (x) { x.disabled = false; });
          toast(T('js_ms_load_failed'), 'error');
        });
      });
    });
  }

  function initInbox() {
    var list = document.querySelector('[data-manuscript-previews]');
    if (!list) return;
    // 公開サーバーに届いた原稿は、中身と差分を見せてから受け取る。下書きはそのまま取り込んで続きを書く。
    // ほかの人が編集中の記事（data-force）は、確かめてから引き継ぐ。
    document.querySelectorAll('[data-remote-inbox-import]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        if (btn.disabled) return;
        var id = btn.getAttribute('data-remote-inbox-import');
        var draft = btn.hasAttribute('data-draft');
        var force = btn.getAttribute('data-force') === '1';
        var go = function () {
          if (draft) continueDraft(list, id, btn, force);
          else previewRemote(list, id, btn, { force: force });
        };
        if (!force) { go(); return; }
        window.sorusConfirm({ message: btn.getAttribute('data-confirm-message') }).then(function (ok) { if (ok) go(); });
      });
    });
    document.querySelectorAll('[data-proposal-open]').forEach(function (btn) {
      btn.addEventListener('click', function () { openProposal(list, btn.getAttribute('data-proposal-open')); });
    });
    var m = /^#remote-([A-Za-z0-9_-]+)$/.exec(location.hash);
    if (m) previewRemote(list, m[1], document.querySelector('[data-remote-inbox-import="' + m[1] + '"]'));
    document.querySelectorAll('[data-remote-inbox-publish]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        if (btn.disabled) return;
        // 確認は管理画面共通の確認モーダル（window.sorusConfirm）で行う。
        window.sorusConfirm({ message: btn.getAttribute('data-confirm-message') }).then(function (ok) {
          if (!ok || btn.disabled) return;
          publishRemote(list, btn, btn.getAttribute('data-remote-inbox-publish'), false);
        });
      });
    });
  }

  // publishRemote は公開サーバーの原稿 id を承認して公開する。ほかの人が編集中なら確かめてから force=1 で送り直し、
  // この PC で取り込んだ後に公開サーバーで変わっていた（409・code: conflict）ときは、今の内容と差分を見せる。
  function publishRemote(list, btn, id, force) {
    btn.disabled = true;
    var body = new URLSearchParams();
    if (force) body.append('force', '1');
    post('/admin/remote/pending/' + encodeURIComponent(id) + '/publish', body).then(function (res) {
      if (res.ok) {
        toast(res.data.message, 'success');
        btn.textContent = T('js_published');
        return;
      }
      btn.disabled = false;
      if (needForce(res) && !force) {
        confirmTakeover(res).then(function (ok) { if (ok) publishRemote(list, btn, id, true); });
        return;
      }
      toast(failMessage(res), 'error');
      if (res.status === 409 && res.data.code === 'conflict') {
        previewRemote(list, id, document.querySelector('[data-remote-inbox-import="' + id + '"]'), { force: force });
      }
    }).catch(function () { btn.disabled = false; toast(T('js_ms_publish_failed'), 'error'); });
  }

  function init() {
    initInbox();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
