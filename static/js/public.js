// 公開サイトのインクリメンタルサーチ。data-live-search に結果の差し替え先（CSS セレクタ）を持つ検索窓で、
// 入力が止まったら /search に HX-Request ヘッダー付きで問い合わせ、結果のパーシャルだけを差し替える。
// JavaScript が無効でもフォーム送信で同じ結果ページが表示される（プログレッシブエンハンスメント）。
(function () {
  "use strict";
  var DEBOUNCE_MS = 250;

  document.querySelectorAll("input[data-live-search]").forEach(function (input) {
    var target = document.querySelector(input.getAttribute("data-live-search"));
    if (!target) return;
    var timer = null;
    var controller = null;

    input.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(function () { run(input.value.trim()); }, DEBOUNCE_MS);
    });

    function run(q) {
      if (controller) controller.abort();
      controller = new AbortController();
      var url = "/search?q=" + encodeURIComponent(q);
      target.classList.add("search-results-loading");
      fetch(url, { headers: { "HX-Request": "true" }, signal: controller.signal })
        .then(function (res) {
          if (!res.ok) throw new Error("search failed: " + res.status);
          return res.text();
        })
        .then(function (html) {
          target.innerHTML = html;
          history.replaceState(null, "", q ? url : "/search");
          document.querySelectorAll("input[data-live-search]").forEach(function (other) {
            if (other !== input) other.value = q;
          });
        })
        .catch(function (err) {
          if (err.name !== "AbortError") console.error(err);
        })
        .finally(function () { target.classList.remove("search-results-loading"); });
    }
  });
})();

// 公開サイトのヘッダー（layout.html の .site-header）: 高さの CSS 変数、スマートフォンのメニューの開閉、メニューの
// ドロップダウン（サイト構成の子項目）の開閉。JavaScript が無効でも、メニューと検索窓は今までどおり表示され、
// ドロップダウンは CSS のホバーとフォーカスで開く（public.css）。
(function () {
  "use strict";
  var header = document.querySelector(".site-header");
  if (!header) return;
  var root = document.documentElement;
  var wide = window.matchMedia ? window.matchMedia("(min-width: 768px)") : null;
  function isDesktop() { return wide ? wide.matches : window.innerWidth >= 768; }

  // --site-header-h はヘッダーの実際の高さ（px）。--site-header-sticky-h はヘッダーが画面の上に固定される（position が
  // sticky・fixed の）テーマでだけ同じ値で、そうでなければ 0px。public.css の目次（sticky）の位置と見出しの
  // scroll-margin-top が使い、テーマも使ってよい。ヘッダーの高さが変わるたび（画面の幅・フォントの読み込み・メニューの開閉）に測り直す。
  var lastHeight = "";
  var lastSticky = "";
  function measure() {
    var height = Math.round(header.getBoundingClientRect().height) + "px";
    var position = window.getComputedStyle(header).position;
    var sticky = position === "sticky" || position === "fixed" ? height : "0px";
    if (height !== lastHeight) {
      root.style.setProperty("--site-header-h", height);
      lastHeight = height;
    }
    if (sticky !== lastSticky) {
      root.style.setProperty("--site-header-sticky-h", sticky);
      header.classList.toggle("is-sticky", sticky !== "0px");
      lastSticky = sticky;
    }
  }
  measure();
  if (typeof window.ResizeObserver === "function") new window.ResizeObserver(measure).observe(header);
  window.addEventListener("resize", measure);
  window.addEventListener("load", measure);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(measure);

  var nav = header.querySelector(".category-nav");
  var menu = nav && nav.querySelector(".site-menu");
  if (!menu) return;
  // 以後の開閉はこのスクリプトが受け持つ（public.css は .is-enhanced の無いときだけ、ホバーとフォーカスで開く）。
  nav.classList.add("is-enhanced");

  // 浮かせて出すドロップダウンの背景は、メニューの帯と同じ色にする（白い文字のメニューのテーマで、白い背景に白い文字に
  // ならないように）。テーマが --site-nav-bg を決めていればそれを使い、無ければメニューから外側へたどって最初の背景色。
  var themedBackground = window.getComputedStyle(nav).getPropertyValue("--site-nav-bg").trim() !== "";
  function paintNavBackground() {
    if (themedBackground) return;
    nav.style.removeProperty("--site-nav-bg");
    for (var el = nav; el && el.nodeType === 1; el = el.parentElement) {
      var bg = window.getComputedStyle(el).backgroundColor;
      if (bg && bg !== "transparent" && !/^rgba\([^)]*,\s*0\)$/.test(bg)) {
        nav.style.setProperty("--site-nav-bg", bg);
        return;
      }
    }
  }
  paintNavBackground();
  if (window.matchMedia) {
    var scheme = window.matchMedia("(prefers-color-scheme: dark)");
    if (scheme.addEventListener) scheme.addEventListener("change", paintNavBackground);
  }

  var items = Array.prototype.filter.call(menu.children, function (li) { return li.classList.contains("has-submenu"); });
  function toggleOf(li) {
    for (var i = 0; i < li.children.length; i++) {
      if (li.children[i].classList.contains("submenu-toggle")) return li.children[i];
    }
    return null;
  }
  function isOpen(li) { var b = toggleOf(li); return !!b && b.getAttribute("aria-expanded") === "true"; }
  function setOpen(li, open) {
    var b = toggleOf(li);
    if (!b) return;
    b.setAttribute("aria-expanded", open ? "true" : "false");
    li.classList.toggle("is-open", open);
    if (!open) li.removeAttribute("data-hover-open");
  }
  function closeAll(except) {
    items.forEach(function (li) { if (li !== except) setOpen(li, false); });
  }
  function links(li) { return Array.prototype.slice.call(li.querySelectorAll(".site-submenu a")); }

  items.forEach(function (li) {
    var button = toggleOf(li);
    if (!button) return;
    var leaveTimer = 0;
    button.addEventListener("click", function () {
      // ポインターを載せて開いたものは、押すと開いたまま固定する（ポインターが離れても閉じない）。
      if (li.hasAttribute("data-hover-open")) {
        li.removeAttribute("data-hover-open");
        return;
      }
      var open = !isOpen(li);
      // PC で開くのは1つだけ。スマートフォン（メニューの中のアコーディオン）ではいくつ開いてもよい。
      if (open && isDesktop()) closeAll(li);
      setOpen(li, open);
    });
    // ホバー補助: PC のマウスでは、載せると開き、離れて少したつと閉じる（押して開いたものは閉じない）。
    li.addEventListener("pointerenter", function (e) {
      if (e.pointerType !== "mouse" || !isDesktop()) return;
      clearTimeout(leaveTimer);
      if (isOpen(li)) return;
      closeAll(li);
      setOpen(li, true);
      li.setAttribute("data-hover-open", "");
    });
    li.addEventListener("pointerleave", function (e) {
      if (e.pointerType !== "mouse" || !li.hasAttribute("data-hover-open")) return;
      leaveTimer = setTimeout(function () {
        if (li.hasAttribute("data-hover-open")) setOpen(li, false);
      }, 250);
    });
    li.addEventListener("keydown", function (e) {
      var list = links(li);
      var index = list.indexOf(document.activeElement);
      if (e.key === "Escape" && isOpen(li)) {
        // Esc で閉じて、開くボタンへ戻る（スマートフォンのメニュー自体は、もう一度 Esc で閉じる）。
        setOpen(li, false);
        button.focus();
        e.preventDefault();
        e.stopPropagation();
      } else if (e.key === "ArrowDown" && (e.target === button || index >= 0) && list.length) {
        if (!isOpen(li)) {
          if (isDesktop()) closeAll(li);
          setOpen(li, true);
        }
        list[index < 0 ? 0 : Math.min(index + 1, list.length - 1)].focus();
        e.preventDefault();
      } else if (e.key === "ArrowUp" && index >= 0) {
        if (index === 0) button.focus();
        else list[index - 1].focus();
        e.preventDefault();
      }
    });
    // PC: フォーカスがドロップダウンの外へ移ったら閉じる。
    li.addEventListener("focusout", function (e) {
      if (!isDesktop() || !e.relatedTarget || li.contains(e.relatedTarget)) return;
      setOpen(li, false);
    });
  });

  // PC: ドロップダウンの外を押したら閉じる。
  document.addEventListener("click", function (e) {
    if (!isDesktop()) return;
    items.forEach(function (li) { if (!li.contains(e.target)) setOpen(li, false); });
  });

  // スマートフォン（768px 未満）: サイト名の横のボタンで、検索窓とメニューをまとめて開閉する（閉じているあいだの
  // ヘッダーはサイト名とボタンの1行）。PC の幅ではボタンを隠し、メニューを横に並べる（public.css）。
  var toggle = header.querySelector(".nav-toggle");
  if (!toggle) return;
  if (!nav.id) nav.id = "site-nav";
  toggle.setAttribute("aria-controls", nav.id);
  toggle.hidden = false;
  header.classList.add("is-nav-ready");
  function setNavOpen(open, returnFocus) {
    header.classList.toggle("is-nav-open", open);
    toggle.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) {
      // 表示中のページを含むドロップダウンは、開いた状態で見せる。
      items.forEach(function (li) { if (li.classList.contains("is-current-parent")) setOpen(li, true); });
    } else {
      closeAll();
      if (returnFocus) toggle.focus();
    }
  }
  toggle.addEventListener("click", function () { setNavOpen(!header.classList.contains("is-nav-open")); });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && header.classList.contains("is-nav-open") && !isDesktop()) setNavOpen(false, true);
  });
  // ページ内のリンク（#見出し）を選んだら、メニューを閉じて本文を見せる。
  nav.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("a") && header.classList.contains("is-nav-open")) setNavOpen(false);
  });
  // PC の幅に広げたら、スマートフォンのメニューの開閉の状態を戻す。
  if (wide) {
    var reset = function () { if (header.classList.contains("is-nav-open")) setNavOpen(false); closeAll(); };
    if (wide.addEventListener) wide.addEventListener("change", reset);
    else if (wide.addListener) wide.addListener(reset);
  }
})();

// 本文のコードブロック（.prose pre。Mermaid の図の pre.mermaid は除く）のコピーボタンと言語の表示。pre を div.code-block で包み、右上に言語（```go のように
// 書いたときの class="language-go"）とコピーボタンを置く。押すとコードをクリップボードに写し、ボタンの文字で
// 「コピーしました」と知らせる（読み上げは画面に出ない role="status" で伝える）。JavaScript が無いときはコードだけを出す。
(function () {
  "use strict";
  var blocks = document.querySelectorAll(".prose pre:not(.mermaid)");
  if (!blocks.length) return;
  var en = (document.documentElement.getAttribute("lang") || "ja").toLowerCase().indexOf("en") === 0;
  var LABELS = en
    ? { copy: "Copy", copied: "Copied!", failed: "Copy failed", aria: "Copy code to clipboard" }
    : { copy: "コピー", copied: "コピーしました", failed: "コピーできませんでした", aria: "コードをクリップボードにコピー" };
  // FEEDBACK_MS は「コピーしました」を出しておく時間。
  var FEEDBACK_MS = 2000;
  var LANG_PATTERN = /(?:^|\s)language-([A-Za-z0-9_+#.-]{1,32})(?:\s|$)/;

  var live = document.createElement("span");
  live.className = "visually-hidden";
  live.setAttribute("role", "status");
  live.setAttribute("aria-live", "polite");
  document.body.appendChild(live);

  // legacyCopy はクリップボードの API を使えないとき（http の LAN のアドレスなど）に、選んだ文字をコピーする命令で写す。
  function legacyCopy(text) {
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.top = "-1000px";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    document.body.removeChild(area);
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).catch(function () { return legacyCopy(text); });
    }
    return legacyCopy(text);
  }

  Array.prototype.forEach.call(blocks, function (pre) {
    var parent = pre.parentNode;
    if (!parent || (parent.classList && parent.classList.contains("code-block"))) return;
    var code = pre.querySelector("code");
    var wrap = document.createElement("div");
    wrap.className = "code-block";
    parent.insertBefore(wrap, pre);
    wrap.appendChild(pre);

    var bar = document.createElement("div");
    bar.className = "code-block-toolbar";
    var m = code && LANG_PATTERN.exec(code.className || "");
    if (m) {
      var lang = document.createElement("span");
      lang.className = "code-block-lang";
      lang.textContent = m[1];
      wrap.setAttribute("data-lang", m[1]);
      bar.appendChild(lang);
    }
    var button = document.createElement("button");
    button.type = "button";
    button.className = "code-copy";
    button.textContent = LABELS.copy;
    button.setAttribute("aria-label", LABELS.aria);
    bar.appendChild(button);
    wrap.appendChild(bar);

    var timer = 0;
    function feedback(ok) {
      button.textContent = ok ? LABELS.copied : LABELS.failed;
      button.classList.toggle("is-copied", ok);
      button.classList.toggle("is-failed", !ok);
      live.textContent = button.textContent;
      clearTimeout(timer);
      timer = setTimeout(function () {
        button.textContent = LABELS.copy;
        button.classList.remove("is-copied", "is-failed");
        live.textContent = "";
      }, FEEDBACK_MS);
    }
    button.addEventListener("click", function () {
      var text = (code || pre).textContent.replace(/\n$/, "");
      copyText(text).then(function () { feedback(true); }, function () { feedback(false); }).then(function () {
        // 古い方法のコピーは入力欄を選ぶため、ボタンにフォーカスを戻す。
        if (document.activeElement !== button) button.focus();
      });
    });
  });
})();

// タブ（internal/markdown/tabs.go の div.doc-tabs[data-tabs]）。サーバーは JavaScript が無くても読めるよう、タブの並び
// （div.doc-tab-list）を hidden にし、すべてのタブの中身を名前の行（p.doc-tab-title）付きで縦に並べて出す。ここでは並びの
// hidden を外し、選んでいないタブの中身と名前の行を隠して、押す・左右の矢印キー（Home・End）で切り替えられるようにする。
// URL の # が隠したタブの中（見出しなど）を指すときは、そのタブを選んで見せる。
(function () {
  "use strict";
  var groups = [];
  Array.prototype.forEach.call(document.querySelectorAll(".doc-tabs[data-tabs]"), function (root) {
    var list = null;
    var panels = [];
    Array.prototype.forEach.call(root.children, function (child) {
      if (child.classList.contains("doc-tab-list")) list = list || child;
      else if (child.classList.contains("doc-tab-panel")) panels.push(child);
    });
    if (!list) return;
    var tabs = Array.prototype.filter.call(list.children, function (b) { return b.classList.contains("doc-tab"); });
    var pairs = [];
    tabs.forEach(function (tab) {
      var id = tab.getAttribute("aria-controls");
      for (var i = 0; i < panels.length; i++) {
        if (panels[i].id === id) {
          pairs.push({ tab: tab, panel: panels[i] });
          return;
        }
      }
    });
    // ボタンと中身の組が合わないものは触らない（すべてのタブの中身を縦に並べたままにする）。
    if (!pairs.length || pairs.length !== tabs.length) return;

    function select(index, focus) {
      pairs.forEach(function (p, i) {
        var on = i === index;
        p.tab.setAttribute("aria-selected", on ? "true" : "false");
        p.tab.tabIndex = on ? 0 : -1;
        p.panel.hidden = !on;
      });
      if (focus) pairs[index].tab.focus();
    }

    var initial = 0;
    for (var i = 0; i < pairs.length; i++) {
      if (pairs[i].tab.getAttribute("aria-selected") === "true") {
        initial = i;
        break;
      }
    }
    pairs.forEach(function (p) {
      var title = p.panel.firstElementChild;
      if (title && title.classList.contains("doc-tab-title")) title.hidden = true;
      if (!p.panel.hasAttribute("tabindex")) p.panel.tabIndex = 0;
    });
    select(initial, false);
    list.hidden = false;
    root.classList.add("is-ready");

    pairs.forEach(function (p, index) {
      p.tab.addEventListener("click", function () { select(index, false); });
    });
    list.addEventListener("keydown", function (e) {
      var current = -1;
      for (var i = 0; i < pairs.length; i++) {
        if (pairs[i].tab === document.activeElement) current = i;
      }
      if (current < 0) return;
      var next = -1;
      if (e.key === "ArrowRight") next = (current + 1) % pairs.length;
      else if (e.key === "ArrowLeft") next = (current - 1 + pairs.length) % pairs.length;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = pairs.length - 1;
      if (next < 0) return;
      e.preventDefault();
      select(next, true);
    });
    groups.push({ pairs: pairs, select: select });
  });
  if (!groups.length) return;

  function revealHash() {
    var id = "";
    try { id = decodeURIComponent(location.hash.slice(1)); } catch (e) { return; }
    var target = id && document.getElementById(id);
    if (!target) return;
    var revealed = false;
    // 外側のタブから順に（文書の順に）選ぶため、入れ子のタブの中も開ける。
    groups.forEach(function (g) {
      g.pairs.forEach(function (p, i) {
        if (p.panel.hidden && p.panel.contains(target)) {
          g.select(i, false);
          revealed = true;
        }
      });
    });
    if (revealed) target.scrollIntoView();
  }
  revealHash();
  window.addEventListener("hashchange", revealHash);
})();

// ドキュメント表示の左のサイドバー（partials/doc_nav.html の .doc-sidebar）。サーバーは JavaScript が無くても読めるよう
// 開いた状態（<details open>）で出す。ここでは PC の幅（1024px 以上）では開いたまま、狭い画面では閉じて本文を先に見せる。
// サイドバーの中でスクロールする長い一覧では、表示中のページの項目を見える位置に出す。
(function () {
  "use strict";
  var details = document.querySelector(".doc-sidebar-collapsible");
  if (!details) return;
  var wide = window.matchMedia ? window.matchMedia("(min-width: 1024px)") : null;
  function sync() { details.open = wide ? wide.matches : window.innerWidth >= 1024; }
  sync();
  if (wide) {
    if (wide.addEventListener) wide.addEventListener("change", sync);
    else if (wide.addListener) wide.addListener(sync);
  }
  var aside = details.closest ? details.closest(".doc-sidebar") : null;
  var current = details.querySelector('a[aria-current="page"]');
  if (aside && current && aside.scrollHeight > aside.clientHeight) {
    var top = current.getBoundingClientRect().top - aside.getBoundingClientRect().top;
    if (top + current.offsetHeight > aside.clientHeight) aside.scrollTop = top - aside.clientHeight / 3;
  }
})();

// Cmd+K・Ctrl+K のすばやい検索（layout.html の <dialog id="search-modal">）。どのページからでも開き、入力に合わせて
// 結果を出す。上下の矢印キーで選び、Enter でそのページへ移る（選んでいないときはフォームを送り、/search の結果のページを開く）。
// 公開サーバーでは /search に HX-Request を付けて結果のパーシャルを受け取り、静的エクスポート（data-search-index がある）では
// static-search.js（data-search-script）を読み込んで検索用インデックスをブラウザの中で絞り込む。ヘッダーの検索窓には
// ショートカットの案内（Cmd K・Ctrl K）を添え、押しても開く。JavaScript が無いときは開かず、検索窓のフォーム送信がそのまま動く。
(function () {
  "use strict";
  var dialog = document.getElementById("search-modal");
  if (!dialog || typeof dialog.showModal !== "function") return;
  var form = dialog.querySelector(".search-modal-form");
  var input = dialog.querySelector(".search-modal-input");
  var status = dialog.querySelector(".search-modal-status");
  var list = dialog.querySelector(".search-modal-results");
  var closeButton = dialog.querySelector(".search-modal-close");
  if (!form || !input || !status || !list) return;
  // MAX_RESULTS は一覧に出す結果の数。それより多いときは「すべての検索結果を見る」から /search を開く。
  var MAX_RESULTS = 8;
  var DEBOUNCE_MS = 150;
  // MAX_QUERY_RUNES は検索語の最大文字数（サーバーの検索と同じ）。
  var MAX_QUERY_RUNES = 100;
  var lang = (document.documentElement.getAttribute("lang") || "ja").toLowerCase().split("-")[0];
  var indexURL = dialog.getAttribute("data-search-index");
  var scriptURL = dialog.getAttribute("data-search-script");
  var searchPath = form.getAttribute("action") || "/search";
  var isMac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");
  var timer = 0;
  var seq = 0;
  var controller = null;
  var engine = null;
  var active = -1;

  // msg は dialog の data-msg-<name> の文言の {名前} を vars の値に置き換えて返す。
  function msg(name, vars) {
    var text = dialog.getAttribute("data-msg-" + name) || "";
    Object.keys(vars || {}).forEach(function (k) { text = text.split("{" + k + "}").join(vars[k]); });
    return text;
  }

  // ヘッダーの検索窓: 入力欄を span.search-field で包み、ショートカットの案内を重ねる（文字を入れると public.css が隠す）。
  var headerInputs = document.querySelectorAll(".site-header .search-form input[name=q]");
  Array.prototype.forEach.call(headerInputs, function (field) {
    field.setAttribute("aria-keyshortcuts", isMac ? "Meta+K" : "Control+K");
    var wrap = document.createElement("span");
    wrap.className = "search-field";
    field.parentNode.insertBefore(wrap, field);
    wrap.appendChild(field);
    var hint = document.createElement("span");
    hint.className = "search-shortcut";
    hint.setAttribute("aria-hidden", "true");
    hint.title = dialog.getAttribute("aria-label") || "";
    [isMac ? "\u2318" : "Ctrl", "K"].forEach(function (key) {
      var kbd = document.createElement("kbd");
      kbd.textContent = key;
      hint.appendChild(kbd);
    });
    hint.addEventListener("mousedown", function (e) { e.preventDefault(); });
    hint.addEventListener("click", function () { open(field.value); });
    wrap.appendChild(hint);
  });
  function headerQuery() {
    for (var i = 0; i < headerInputs.length; i++) {
      if (headerInputs[i].value.trim() !== "") return headerInputs[i].value;
    }
    return "";
  }

  function open(initial) {
    if (!dialog.open) dialog.showModal();
    if (initial && initial.trim() !== "") input.value = initial;
    input.focus();
    input.select();
    // 静的エクスポートでは、開いたときに検索の部品とインデックスを読み始め、最初の入力を待たせない。
    if (indexURL) loadEngine().then(function (s) { return s.load(indexURL); }).catch(function () {});
    run(input.value);
  }

  document.addEventListener("keydown", function (e) {
    if (!(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey || (e.key !== "k" && e.key !== "K")) return;
    e.preventDefault();
    if (dialog.open) {
      input.focus();
      input.select();
      return;
    }
    open(headerQuery());
  });
  if (closeButton) closeButton.addEventListener("click", function () { dialog.close(); });
  // 背景（dialog の外側）を押したら閉じる。
  dialog.addEventListener("click", function (e) { if (e.target === dialog) dialog.close(); });

  input.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { run(input.value); }, DEBOUNCE_MS);
  });
  input.addEventListener("keydown", function (e) {
    var options = optionList();
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (!options.length) return;
      var step = e.key === "ArrowDown" ? 1 : -1;
      setActive(active < 0 ? (step > 0 ? 0 : options.length - 1) : (active + step + options.length) % options.length, true);
    } else if (e.key === "Enter") {
      // 日本語の変換を確定する Enter では移らない。
      if (e.isComposing || e.keyCode === 229) return;
      var link = active >= 0 && options[active] ? options[active].querySelector("a") : null;
      if (!link) return; // 選んでいなければフォームを送る（/search の結果のページ）。
      e.preventDefault();
      if (e.metaKey || e.ctrlKey) window.open(link.href, "_blank", "noopener");
      else window.location.href = link.href;
    }
  });
  list.addEventListener("mousemove", function (e) {
    var li = e.target.closest ? e.target.closest(".search-modal-result") : null;
    var index = li ? optionList().indexOf(li) : -1;
    if (index >= 0 && index !== active) setActive(index, false);
  });

  function optionList() { return Array.prototype.slice.call(list.children); }

  function setActive(index, scroll) {
    var options = optionList();
    options.forEach(function (li, i) { li.setAttribute("aria-selected", i === index ? "true" : "false"); });
    active = index;
    if (index >= 0 && options[index]) {
      input.setAttribute("aria-activedescendant", options[index].id);
      if (scroll && options[index].scrollIntoView) options[index].scrollIntoView({ block: "nearest" });
    } else {
      input.removeAttribute("aria-activedescendant");
    }
  }

  function showMessage(text) {
    list.textContent = "";
    setActive(-1, false);
    input.setAttribute("aria-expanded", "false");
    status.classList.remove("is-count");
    status.textContent = text;
  }

  function run(raw) {
    var q = String(raw || "").trim();
    if (q.length > MAX_QUERY_RUNES) q = q.slice(0, MAX_QUERY_RUNES);
    var mine = ++seq;
    if (controller) {
      controller.abort();
      controller = null;
    }
    if (q === "") {
      dialog.classList.remove("is-loading");
      showMessage(msg("idle"));
      return;
    }
    dialog.classList.add("is-loading");
    if (!list.children.length) showMessage(msg("loading"));
    (indexURL ? searchIndex(q) : searchServer(q))
      .then(function (result) {
        if (mine === seq) render(q, result);
      })
      .catch(function (err) {
        if (mine !== seq || (err && err.name === "AbortError")) return;
        showMessage(msg("error"));
      })
      .then(function () {
        if (mine === seq) dialog.classList.remove("is-loading");
      });
  }

  // isSiteLink は検索結果のリンク先がサイト内のパス（/ 始まり。// 始まりの別のホストは除く）か返す。
  function isSiteLink(url) { return typeof url === "string" && /^\/(?!\/)/.test(url); }

  // safeCopy は el の中の文字と <mark>（一致した語の強調）だけを写した断片を返す。ほかの要素・属性は持ち込まない。
  function safeCopy(el) {
    var frag = document.createDocumentFragment();
    (function walk(node, parent) {
      Array.prototype.forEach.call(node.childNodes, function (child) {
        if (child.nodeType === 3) {
          parent.appendChild(document.createTextNode(child.nodeValue));
        } else if (child.nodeType === 1) {
          if (child.tagName === "MARK") {
            var mark = document.createElement("mark");
            walk(child, mark);
            parent.appendChild(mark);
          } else {
            walk(child, parent);
          }
        }
      });
    })(el, frag);
    return frag;
  }

  // searchServer は公開サーバーの /search の結果のパーシャル（partials/search_results.html）を受け取り、結果にする。
  function searchServer(q) {
    controller = typeof AbortController === "function" ? new AbortController() : null;
    var init = { headers: { "HX-Request": "true" }, credentials: "same-origin" };
    if (controller) init.signal = controller.signal;
    return fetch(searchPath + "?q=" + encodeURIComponent(q), init)
      .then(function (res) {
        if (!res.ok) throw new Error("search failed: " + res.status);
        return res.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var hits = [];
        Array.prototype.forEach.call(doc.querySelectorAll(".search-hit"), function (hit) {
          var a = hit.querySelector(".card-title a") || hit.querySelector("a");
          if (!a || !isSiteLink(a.getAttribute("href")) || hits.length >= MAX_RESULTS) return;
          var snippet = hit.querySelector(".snippet");
          var badge = hit.querySelector(".meta .badge");
          hits.push({
            url: a.getAttribute("href"),
            title: safeCopy(a),
            snippet: snippet ? safeCopy(snippet) : null,
            meta: badge ? badge.textContent.trim() : ""
          });
        });
        var count = doc.querySelector(".result-count[data-total]");
        var total = count ? parseInt(count.getAttribute("data-total"), 10) : NaN;
        return { hits: hits, total: isNaN(total) ? hits.length : total };
      });
  }

  function loadEngine() {
    if (window.sorusStaticSearch) return Promise.resolve(window.sorusStaticSearch);
    if (!engine) {
      engine = new Promise(function (resolve, reject) {
        if (!scriptURL) {
          reject(new Error("static search script is not set"));
          return;
        }
        var script = document.createElement("script");
        script.src = scriptURL;
        script.onload = function () {
          if (window.sorusStaticSearch) resolve(window.sorusStaticSearch);
          else reject(new Error("static search did not initialize"));
        };
        script.onerror = function () { reject(new Error("static search script failed to load")); };
        document.head.appendChild(script);
      });
      engine.catch(function () { engine = null; });
    }
    return engine;
  }

  // searchIndex は静的エクスポートの検索用インデックスを static-search.js の決まりで絞り込み、結果にする。
  function searchIndex(q) {
    return loadEngine().then(function (s) {
      return s.load(indexURL).then(function (items) {
        var words = s.terms(q);
        var found = s.find(items, words, lang);
        var hits = [];
        found.forEach(function (it) {
          if (hits.length >= MAX_RESULTS || !isSiteLink(it.url)) return;
          var title = document.createDocumentFragment();
          s.appendHighlighted(title, String(it.title || ""), words);
          var text = s.snippet(it, words);
          var snippet = null;
          if (text) {
            snippet = document.createDocumentFragment();
            s.appendHighlighted(snippet, text, words);
          }
          var meta = it.kind === "page" ? "" : [it.category || "", s.formatDate(it.published)].filter(Boolean).join(" · ");
          hits.push({ url: it.url, title: title, snippet: snippet, meta: meta });
        });
        return { hits: hits, total: found.length };
      });
    });
  }

  function render(q, result) {
    if (!result.hits.length) {
      showMessage(msg("none"));
      return;
    }
    list.textContent = "";
    status.classList.add("is-count");
    status.textContent = msg("count", { n: String(result.total) });
    result.hits.forEach(function (hit, i) {
      var li = document.createElement("li");
      li.className = "search-modal-result";
      li.id = "search-modal-option-" + i;
      li.setAttribute("role", "option");
      var a = document.createElement("a");
      a.href = hit.url;
      a.tabIndex = -1;
      var title = document.createElement("span");
      title.className = "search-modal-result-title";
      title.appendChild(hit.title);
      a.appendChild(title);
      if (hit.meta) {
        var meta = document.createElement("span");
        meta.className = "search-modal-result-meta";
        meta.textContent = hit.meta;
        a.appendChild(meta);
      }
      if (hit.snippet) {
        var snippet = document.createElement("span");
        snippet.className = "search-modal-result-snippet";
        snippet.appendChild(hit.snippet);
        a.appendChild(snippet);
      }
      li.appendChild(a);
      list.appendChild(li);
    });
    var all = document.createElement("li");
    all.className = "search-modal-result search-modal-result-all";
    all.id = "search-modal-option-all";
    all.setAttribute("role", "option");
    var allLink = document.createElement("a");
    allLink.href = searchPath + "?q=" + encodeURIComponent(q);
    allLink.tabIndex = -1;
    allLink.textContent = msg("all", { q: q });
    all.appendChild(allLink);
    list.appendChild(all);
    input.setAttribute("aria-expanded", "true");
    setActive(0, true);
  }
})();
