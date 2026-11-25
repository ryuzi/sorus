// 「公開先」画面（publish.html）とダッシュボード（publish_panel.html を読み込む）で使うスクリプト。
// 「サイトに反映」を押すと反映を始め、終わるまで /admin/publish/status を1秒ごとに読んで進み具合と結果を描く。
// 公開先の画面では、選んだ公開先の入力欄だけを見せ、「接続テスト」の結果を出す。サーバーから受け取った文は
// textContent で入れ、HTML として解釈しない。共通部品（window.sorus。sorus_common.js）を使う。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };
  var sorus = window.sorus;
  var el = sorus.el;
  var toast = sorus.toast;

  function list(items, cls) {
    var ul = el('ul', cls);
    items.forEach(function (t) { ul.appendChild(el('li', '', t)); });
    return ul;
  }

  function isWebURL(u) { return /^https?:\/\//.test(u || ''); }

  // renderOutcome は1回の反映の結果 o を box に描く。公開中のサイトを置き換えるかを尋ねているときは
  // 「置き換える」を出す（押すと「サイトに反映」と同じく反映を始める）。
  function renderOutcome(box, o) {
    var tone = o.ok ? 'notice-success' : o.replace_site ? 'notice-warn' : 'notice-error';
    var wrap = el('div', 'notice ' + tone);
    wrap.setAttribute('data-publish-result', o.ok ? 'ok' : o.replace_site ? 'confirm' : 'error');
    wrap.setAttribute('role', 'status');
    wrap.appendChild(el('p', 'font-semibold', o.message));
    if (o.details && o.details.length) wrap.appendChild(list(o.details, 'mt-2 list-disc space-y-1 pl-5'));
    if (/^\/admin\/publish#[a-z-]+$/.test(o.fix_site_url || '')) {
      var fp = el('p', 'mt-3');
      var fa = el('a', 'inline-block rounded-lg border border-rose-300 bg-white px-3 py-1.5 font-medium text-rose-900 hover:bg-rose-100', T('js_pub_enter_site_url'));
      fa.href = o.fix_site_url;
      fa.setAttribute('data-publish-fix-site-url', '');
      fp.appendChild(fa);
      wrap.appendChild(fp);
    }
    if (o.replace_site) {
      var replace = el('form', 'mt-3');
      replace.method = 'post';
      replace.action = '/admin/publish/replace-site';
      replace.setAttribute('data-publish-reflect-form', '');
      var btn = el('button', 'rounded-lg bg-amber-800 px-4 py-2 text-sm font-semibold text-white hover:bg-amber-900', T('js_pub_replace'));
      btn.type = 'submit';
      replace.appendChild(btn);
      wrap.appendChild(replace);
    }
    // 置き換えの確認待ちや失敗では、まだ書き出し終えていないため、書き出し済みのような言い方にしない。
    if (o.folder) wrap.appendChild(el('p', 'mt-2 break-all', o.ok ? T('js_pub_folder_done', o.folder) : T('js_pub_folder_pending', o.folder)));
    if (o.ok && isWebURL(o.site_url)) {
      var p = el('p', 'mt-3');
      var a = el('a', 'inline-block rounded-lg border border-emerald-300 bg-white px-3 py-1.5 font-medium text-emerald-900 hover:bg-emerald-100', T('js_pub_open_site'));
      a.href = o.site_url;
      a.target = '_blank';
      a.rel = 'noopener';
      p.appendChild(a);
      p.appendChild(el('span', 'ml-2 break-all text-xs', o.site_url));
      wrap.appendChild(p);
    }
    if (o.host_key_changed && location.pathname !== '/admin/publish') {
      var fix = el('a', 'mt-2 inline-block font-medium underline', T('js_pub_check_destination'));
      fix.href = '/admin/publish';
      wrap.appendChild(fix);
    }
    if (o.error) {
      var d = el('details', 'mt-2');
      d.appendChild(el('summary', 'cursor-pointer text-xs', T('js_details')));
      d.appendChild(el('pre', 'mt-1 whitespace-pre-wrap break-all text-xs', o.error));
      wrap.appendChild(d);
    }
    if (o.finished_text) wrap.appendChild(el('p', 'mt-2 text-xs opacity-75', T('js_pub_finished', o.finished_text)));
    box.appendChild(wrap);
    if (o.warnings && o.warnings.length) {
      var w = el('div', 'mt-3 notice notice-warn');
      w.setAttribute('data-publish-warnings', '');
      w.appendChild(el('p', 'font-semibold', T('js_notices')));
      w.appendChild(list(o.warnings, 'mt-1 list-disc space-y-1 pl-5'));
      box.appendChild(w);
    }
  }

  // render は状態 st を、ページ内のすべての状態の欄とボタンに反映する。
  function render(st) {
    document.querySelectorAll('[data-publish-status]').forEach(function (box) {
      box.textContent = '';
      if (st.running) {
        box.appendChild(el('p', 'text-sm font-medium text-neutral-800', st.message));
        var bar = el('progress', 'mt-2 h-2 w-full');
        bar.max = 100;
        if (st.total > 0) bar.value = st.percent;
        box.appendChild(bar);
      } else if (st.last) {
        renderOutcome(box, st.last);
      }
    });
    document.querySelectorAll('[data-publish-reflect]').forEach(function (btn) {
      btn.disabled = !!st.running;
      btn.textContent = st.running ? T('js_pub_running') : btn.getAttribute('data-label');
    });
  }

  var polling = false;
  function poll() {
    if (polling) return;
    polling = true;
    (function tick() {
      sorus.requestJSON('GET', '/admin/publish/status')
        .then(function (res) {
          if (!res.ok) throw new Error(String(res.status));
          var st = res.data;
          render(st);
          if (st.running) {
            setTimeout(tick, 1000);
            return;
          }
          polling = false;
          if (st.last) toast(st.last.message, st.last.ok ? 'success' : 'error');
        })
        .catch(function () {
          polling = false;
          toast(T('js_reflect_status_load_failed'), 'error');
        });
    })();
  }

  function reflect(form) {
    sorus.postJSON(form.action)
      .then(function (res) {
        if (!res.ok) {
          toast(res.data.message || T('js_pub_start_failed'), 'error');
          return;
        }
        var banner = document.querySelector('[data-publish-overdue]');
        if (banner) banner.hidden = true;
        render(res.data);
        poll();
      })
      .catch(function () { toast(T('js_pub_start_failed_offline'), 'error'); });
  }

  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (form.matches && form.matches('[data-publish-reflect-form]')) {
      e.preventDefault();
      reflect(form);
    }
  });

  document.addEventListener('click', function (e) {
    var later = e.target.closest && e.target.closest('[data-publish-later]');
    if (later) {
      var banner = later.closest('[data-publish-overdue]');
      if (banner) banner.hidden = true;
    }
    var purge = e.target.closest && e.target.closest('[data-cdn-purge-test]');
    if (purge) {
      var form = purge.form;
      var result = document.querySelector('[data-cdn-purge-result]');
      purge.disabled = true;
      result.textContent = T('js_pub_purging');
      sorus.postJSON('/admin/api/publish/test-cdn-purge', new FormData(form)).then(function (reply) {
        var res = reply.data;
        result.textContent = res.message || T('js_pub_result_unknown');
        result.className = 'mt-2 notice ' + (res.ok ? 'notice-success' : 'notice-error');
      }).catch(function () {
        result.textContent = T('js_pub_test_failed');
        result.className = 'mt-2 notice notice-error';
      }).finally(function () { purge.disabled = false; });
    }
  });

  // --- 公開先の画面: 選んだ公開先の欄だけを見せる・接続テスト ---

  function syncKind() {
    var checked = document.querySelector('input[name="kind"]:checked');
    document.querySelectorAll('[data-kind-panel]').forEach(function (p) {
      p.hidden = !checked || p.getAttribute('data-kind-panel') !== checked.value;
    });
  }

  function syncProtocol() {
    var sel = document.querySelector('select[name="protocol"]');
    if (!sel) return;
    var sftp = sel.value === 'sftp';
    document.querySelectorAll('[data-sftp-only]').forEach(function (p) { p.hidden = !sftp; });
    document.querySelectorAll('[data-ftp-only]').forEach(function (p) { p.hidden = sftp; });
    var port = document.querySelector('input[name="port"]');
    if (port) port.placeholder = sftp ? '22' : '21';
  }

  function test(btn) {
    var form = btn.form;
    var out = document.querySelector('[data-publish-test-result]');
    btn.disabled = true;
    out.textContent = T('js_pub_connecting');
    out.className = 'mt-3 text-sm text-neutral-600';
    sorus.postJSON('/admin/publish/test', new FormData(form))
      .then(function (reply) {
        var res = reply.data;
        if (res.host_key_changed) {
          location.reload();
          return;
        }
        out.textContent = '';
        out.className = 'mt-3 notice ' + (res.ok ? 'notice-success' : 'notice-error');
        out.appendChild(el('p', 'font-semibold', res.message || T('js_pub_connection_test_failed')));
        if (res.details && res.details.length) out.appendChild(list(res.details, 'mt-1 list-disc space-y-1 pl-5'));
        if (res.error) {
          var d = el('details', 'mt-2');
          d.appendChild(el('summary', 'cursor-pointer text-xs', T('js_details')));
          d.appendChild(el('pre', 'mt-1 whitespace-pre-wrap break-all text-xs', res.error));
          out.appendChild(d);
        }
      })
      .catch(function () { out.textContent = T('js_pub_connection_test_failed'); })
      .then(function () { btn.disabled = false; });
  }

  // focusFromHash は、アドレスの # が入力欄（例: #publish-folder-site-url）を指していれば、その欄に移って選ぶ。
  // 反映の結果の「「公開サイトのURL」を入力する」から来たときに使う。
  function focusFromHash() {
    var id = location.hash.slice(1);
    var input = id && document.getElementById(id);
    if (input && input.tagName === 'INPUT') {
      input.scrollIntoView({ block: 'center' });
      input.focus();
    }
  }

  function init(root) {
    root.querySelectorAll('[data-publish-reflect]').forEach(function (btn) {
      if (!btn.getAttribute('data-label')) btn.setAttribute('data-label', btn.textContent);
    });
    if (root.querySelector('[data-publish-running="true"]')) poll();
  }

  document.addEventListener('DOMContentLoaded', function () {
    document.querySelectorAll('input[name="kind"]').forEach(function (r) { r.addEventListener('change', syncKind); });
    var protocol = document.querySelector('select[name="protocol"]');
    if (protocol) protocol.addEventListener('change', syncProtocol);
    syncKind();
    syncProtocol();
    focusFromHash();
    window.addEventListener('hashchange', focusFromHash);
    var testBtn = document.querySelector('[data-publish-test]');
    if (testBtn) testBtn.addEventListener('click', function () { test(testBtn); });
    init(document);

    // ダッシュボード: 「サイトに反映」の部分を読み込む。
    document.querySelectorAll('[data-publish-panel]').forEach(function (box) {
      fetch(box.getAttribute('data-publish-panel'), { credentials: 'same-origin' })
        .then(function (r) { return r.ok ? r.text() : ''; })
        .then(function (html) {
          box.innerHTML = html;
          init(box);
        })
        .catch(function () {});
    });
  });
})();
