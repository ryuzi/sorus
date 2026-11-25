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
