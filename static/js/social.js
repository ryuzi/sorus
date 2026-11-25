// SNS 告知文モーダルのスクリプト: data-social-open 付きのボタン（記事エディタ・記事一覧）から
// POST /admin/ai/social-post を呼び、X のスレッド・Instagram のリード文・LINE の配信文を表示して
// ワンクリックでコピーできるようにする。X の文字数は編集に合わせて数え直す。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };

  var modal = document.getElementById('social-modal');
  if (!modal) return;

  var thread = document.getElementById('social-x-thread');
  var instagram = document.getElementById('social-instagram');
  var line = document.getElementById('social-line');
  var hashtags = document.getElementById('social-hashtags');
  var errorText = document.getElementById('social-error-text');
  var templateNotice = document.getElementById('social-template-notice');
  var unpublishedNotice = document.getElementById('social-unpublished-notice');
  var ogFigure = document.getElementById('social-og-figure');
  var ogImage = document.getElementById('social-og-image');
  var provider = document.getElementById('social-provider');
  var regenerate = document.getElementById('social-regenerate');
  var xLimit = 280;
  var lastButton = null;
  var controller = null;

  // xLength は X が数える文字数（internal/social.XLength と同じ規則の目安）。
  // ラテン文字・一部の句読点は1、和文や絵文字は2、URL は長さによらず23。
  var urlPattern = /https?:\/\/[^\s　]+/g;
  function xLength(s) {
    var n = 0;
    var rest = s.replace(urlPattern, function () { n += 23; return ''; });
    for (var i = 0; i < rest.length; i++) {
      var cp = rest.codePointAt(i);
      if (cp > 0xffff) i++;
      if (cp === 0xfe0f || cp === 0x200d) continue;
      n += (cp <= 0x10ff || (cp >= 0x2000 && cp <= 0x200d) || (cp >= 0x2010 && cp <= 0x201f) ||
        (cp >= 0x2032 && cp <= 0x2037)) ? 1 : 2;
    }
    return n;
  }

  function showPane(name) {
    modal.querySelectorAll('[data-social-pane]').forEach(function (el) { el.hidden = el.dataset.socialPane !== name; });
  }

  function openModal() {
    if (modal.open) return;
    if (typeof modal.showModal === 'function') modal.showModal();
    else modal.setAttribute('open', '');
  }

  function closeModal() {
    if (controller) controller.abort();
    controller = null;
    if (typeof modal.close === 'function') modal.close();
    else modal.removeAttribute('open');
  }

  // collect はボタンに応じたフォームを作る。エディタからは編集中（未保存を含む）のタイトル・本文・抜粋・カテゴリを送る。
  function collect(button) {
    var body = new URLSearchParams();
    var id = button.getAttribute('data-article-id');
    if (id) body.set('article_id', id);
    var form = document.getElementById('article-form');
    if (button.hasAttribute('data-social-editor') && form) {
      ['title', 'excerpt', 'category_id'].forEach(function (name) {
        var el = form.elements[name];
        if (el) body.set(name, el.value);
      });
      var text = document.getElementById('content_markdown');
      if (text) body.set('text', text.value);
    }
    return body;
  }

  function generate(button) {
    lastButton = button;
    if (controller) controller.abort();
    controller = typeof AbortController === 'function' ? new AbortController() : null;
    showPane('loading');
    regenerate.disabled = true;
    openModal();
    fetch('/admin/ai/social-post', {
      method: 'POST',
      headers: { 'X-CSRF-Token': window.cmsCSRFToken(), 'Accept': 'application/json' },
      body: collect(button),
      credentials: 'same-origin',
      signal: controller ? controller.signal : undefined
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) throw new Error(data.message || T('js_social_failed_status', res.status));
        return data;
      });
    }).then(render).catch(function (err) {
      if (err.name === 'AbortError') return;
      errorText.textContent = err.message || T('js_social_failed');
      showPane('error');
    }).then(function () { regenerate.disabled = false; });
  }

  function render(data) {
    xLimit = data.x_limit || xLimit;
    templateNotice.hidden = data.source !== 'template' || data.configured;
    unpublishedNotice.hidden = !!data.url;
    ogFigure.hidden = !data.og_image;
    if (data.og_image) ogImage.src = data.og_image;
    provider.textContent = data.source === 'ai' ? 'AI: ' + data.provider : T('js_social_template');

    thread.textContent = '';
    (data.x_thread || []).forEach(function (text, i, all) {
      var li = document.createElement('li');
      li.className = 'social-post rounded-lg border border-neutral-200 p-3';
      var head = document.createElement('div');
      head.className = 'mb-1 flex items-center justify-between gap-2 text-xs text-neutral-500';
      var label = document.createElement('span');
      label.textContent = all.length > 1 ? (i + 1) + ' / ' + all.length : T('js_social_post');
      var right = document.createElement('span');
      right.className = 'flex items-center gap-3';
      var count = document.createElement('span');
      count.className = 'social-count tabular-nums';
      var copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'btn btn-secondary btn-sm';
      copy.textContent = T('js_copy');
      var ta = document.createElement('textarea');
      ta.className = 'social-text w-full rounded-md border border-neutral-300 p-2 text-sm leading-relaxed focus:border-zinc-500 focus:outline-none';
      ta.rows = Math.min(8, Math.max(3, text.split('\n').length + 1));
      ta.value = text;
      ta.setAttribute('aria-label', T('js_social_x_post', i + 1));
      var recount = function () {
        var n = xLength(ta.value);
        count.textContent = n + ' / ' + xLimit;
        count.classList.toggle('is-over', n > xLimit);
      };
      ta.addEventListener('input', recount);
      copy.addEventListener('click', function () { window.cmsCopy(ta.value, T('js_social_x_post_label')); });
      recount();
      right.appendChild(count);
      right.appendChild(copy);
      head.appendChild(label);
      head.appendChild(right);
      li.appendChild(head);
      li.appendChild(ta);
      thread.appendChild(li);
    });

    instagram.value = data.instagram || '';
    line.value = data.line || '';
    hashtags.textContent = '';
    (data.hashtags || []).forEach(function (tag) {
      var chip = document.createElement('span');
      chip.className = 'rounded-full bg-neutral-100 px-2.5 py-0.5 text-xs font-medium text-neutral-700';
      chip.textContent = tag;
      hashtags.appendChild(chip);
    });
    showPane('result');
  }

  document.addEventListener('click', function (e) {
    var open = e.target.closest('[data-social-open]');
    if (open) {
      e.preventDefault();
      generate(open);
      return;
    }
    if (!modal.contains(e.target)) return;
    if (e.target.closest('[data-social-close]')) closeModal();
    var copy = e.target.closest('[data-social-copy]');
    if (copy) {
      var target = modal.querySelector(copy.getAttribute('data-social-copy'));
      if (target) window.cmsCopy(target.value, target === line ? T('js_social_line') : T('js_social_instagram'));
    }
  });

  document.getElementById('social-copy-thread').addEventListener('click', function () {
    var posts = Array.prototype.map.call(thread.querySelectorAll('textarea'), function (ta) { return ta.value; });
    window.cmsCopy(posts.join('\n\n---\n\n'), T('js_social_x_thread'));
  });
  document.getElementById('social-copy-tags').addEventListener('click', function () {
    var tags = Array.prototype.map.call(hashtags.children, function (el) { return el.textContent; });
    window.cmsCopy(tags.join(' '), T('js_social_hashtags'));
  });
  regenerate.addEventListener('click', function () { if (lastButton) generate(lastButton); });
  modal.addEventListener('close', function () {
    if (controller) controller.abort();
    controller = null;
  });
  // 背景（::backdrop）のクリックで閉じる。
  modal.addEventListener('click', function (e) { if (e.target === modal) closeModal(); });
})();
