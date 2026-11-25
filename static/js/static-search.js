// 静的エクスポートしたサイトのサイト内検索。data-static-search に結果の表示先（CSS セレクタ）を持つ検索窓で、
// 書き出し時に作った検索用インデックス（表示先の data-index。既定 /search-index.json）を1回だけ読み、
// ブラウザの中で絞り込んで表示する。サーバーが無くても動くよう、外部のライブラリやビルドは使わない。
// 照合は大文字・小文字、全角・半角、ひらがな・カタカナの違いを無視した部分一致で、空白で区切った語をすべて含むものを出す。
// URL の ?q= を読み、ヘッダーの検索窓から送られた語でも結果を表示する。
(function () {
  "use strict";
  var DEBOUNCE_MS = 200;
  // SNIPPET_RUNES は結果に出す本文の抜き出しの最大文字数。
  var SNIPPET_RUNES = 120;
  // MAX_QUERY_RUNES は検索語の最大文字数（動的なサーバーの検索と同じ）。
  var MAX_QUERY_RUNES = 100;

  var form = document.querySelector("form[data-static-search]");
  if (!form) return;
  var target = document.querySelector(form.getAttribute("data-static-search"));
  var input = form.querySelector("input[name=q]");
  if (!target || !input) return;
  var indexURL = target.getAttribute("data-index") || "/search-index.json";
  var pageLang = (document.documentElement.getAttribute("lang") || "ja").toLowerCase().split("-")[0];
  var loading = null;
  var timer = null;
  var seq = 0;

  // normalize は照合用に文字の違いをならす。NFKC で全角英数・半角カナをそろえ、小文字にし、カタカナをひらがなにする。
  function normalize(s) {
    s = String(s || "");
    if (s.normalize) s = s.normalize("NFKC");
    s = s.toLowerCase();
    return s.replace(/[ァ-ヶ]/g, function (ch) {
      return String.fromCharCode(ch.charCodeAt(0) - 0x60);
    });
  }

  // terms は検索語を空白（全角を含む）で区切り、ならした語の配列にする。
  function terms(q) {
    return normalize(q).split(/\s+/).filter(function (t) { return t !== ""; });
  }

  // load は検索用インデックスを読み、各項目に照合用の文字列を付けて返す。2回目以降は同じ結果を使う。
  function load() {
    if (!loading) {
      loading = fetch(indexURL, { credentials: "same-origin" })
        .then(function (res) {
          if (!res.ok) throw new Error("search index: " + res.status);
          return res.json();
        })
        .then(function (data) {
          var items = Array.isArray(data.articles) ? data.articles : [];
          items.forEach(function (it) {
            it._title = normalize(it.title);
            it._text = normalize(it.text);
            it._category = normalize(it.category);
          });
          return items;
        });
      loading.catch(function () { loading = null; });
    }
    return loading;
  }

  // find は words をすべて含む項目を返す。タイトルに一致した項目を先に、それ以外はインデックスの順（新しい順）に並べる。
  // ページの言語（pageLang）と一致する記事のみに絞り込む。
  function find(items, words) {
    var inTitle = [];
    var inBody = [];
    items.forEach(function (it) {
      var itemLang = (it.lang || "ja").toLowerCase().split("-")[0];
      if (itemLang !== pageLang) return;
      var titleHits = 0;
      for (var i = 0; i < words.length; i++) {
        var w = words[i];
        var t = it._title.indexOf(w) >= 0;
        if (!t && it._text.indexOf(w) < 0 && it._category.indexOf(w) < 0) return;
        if (t) titleHits++;
      }
      (titleHits > 0 ? inTitle : inBody).push(it);
    });
    return inTitle.concat(inBody);
  }

  // appendHighlighted は text を parent に追加し、words に一致する箇所を <mark> で囲む。
  // ならした文字列と元の文字列の長さが違うとき（半角カナの濁点など）は、位置がずれるため囲まない。
  function appendHighlighted(parent, text, words) {
    var norm = normalize(text);
    if (norm.length !== text.length || words.length === 0) {
      parent.appendChild(document.createTextNode(text));
      return;
    }
    var marks = [];
    words.forEach(function (w) {
      var from = 0;
      var i;
      while ((i = norm.indexOf(w, from)) >= 0) {
        marks.push([i, i + w.length]);
        from = i + w.length;
      }
    });
    marks.sort(function (a, b) { return a[0] - b[0]; });
    var pos = 0;
    marks.forEach(function (m) {
      if (m[0] < pos) return;
      if (m[0] > pos) parent.appendChild(document.createTextNode(text.slice(pos, m[0])));
      var mark = document.createElement("mark");
      mark.textContent = text.slice(m[0], m[1]);
      parent.appendChild(mark);
      pos = m[1];
    });
    if (pos < text.length) parent.appendChild(document.createTextNode(text.slice(pos)));
  }

  // snippet は本文のうち最初に一致した語の前後を抜き出す。本文に一致が無ければ抜粋か本文の先頭を返す。
  function snippet(it, words) {
    var text = String(it.text || "");
    var first = -1;
    if (it._text.length === text.length) {
      words.forEach(function (w) {
        var i = it._text.indexOf(w);
        if (i >= 0 && (first < 0 || i < first)) first = i;
      });
    }
    if (first < 0) {
      var head = it.excerpt || text;
      return head.length > SNIPPET_RUNES ? head.slice(0, SNIPPET_RUNES) + "…" : head;
    }
    var start = Math.max(0, first - Math.floor(SNIPPET_RUNES / 3));
    var end = Math.min(text.length, start + SNIPPET_RUNES);
    return (start > 0 ? "…" : "") + text.slice(start, end) + (end < text.length ? "…" : "");
  }

  // formatDate は "2026-10-27T09:00:00+09:00" を "2026年10月27日"（英語ページは "Oct 27, 2026"）にする。
  function formatDate(iso) {
    var m = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso || "");
    if (!m) return "";
    if (pageLang === "en") {
      var months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
      var mi = parseInt(m[2], 10) - 1;
      var mName = (mi >= 0 && mi < 12) ? months[mi] : m[2];
      return mName + " " + parseInt(m[3], 10) + ", " + m[1];
    }
    return m[1] + "年" + Number(m[2]) + "月" + Number(m[3]) + "日";
  }

  function el(tag, className, text) {
    var e = document.createElement(tag);
    if (className) e.className = className;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function showMessage(text) {
    target.textContent = "";
    target.appendChild(el("p", "empty", text));
  }

  function render(q, words, hits) {
    target.textContent = "";
    var countMsg = pageLang === "en" ? (hits.length + " results for \u201c" + q + "\u201d") : ("「" + q + "」の検索結果: " + hits.length + "件");
    target.appendChild(el("p", "result-count", countMsg)).setAttribute("role", "status");
    if (hits.length === 0) {
      var emptyMsg = pageLang === "en" ? "No matching articles found. Try searching with different keywords." : "一致する記事は見つかりませんでした。別の言葉で検索してみてください。";
      target.appendChild(el("p", "empty", emptyMsg));
      return;
    }
    var list = el("ol", "search-hits");
    hits.forEach(function (it) {
      var li = el("li", "search-hit");
      var h = el("h2", "card-title");
      var a = el("a");
      a.href = it.url;
      appendHighlighted(a, String(it.title || ""), words);
      h.appendChild(a);
      li.appendChild(h);
      var sn = snippet(it, words);
      if (sn) {
        var p = el("p", "snippet");
        appendHighlighted(p, sn, words);
        li.appendChild(p);
      }
      if (it.kind !== "page") {
        var meta = el("p", "meta");
        if (it.category) meta.appendChild(el("span", "badge", it.category));
        var date = formatDate(it.published);
        if (date) {
          if (it.category) meta.appendChild(document.createTextNode(" "));
          var time = el("time", "", date);
          time.setAttribute("datetime", it.published);
          meta.appendChild(time);
        }
        if (meta.childNodes.length > 0) li.appendChild(meta);
      }
      list.appendChild(li);
    });
    target.appendChild(list);
  }

  // run は q で検索して結果を表示し、URL の ?q= を合わせる（再読み込み・共有で同じ結果になる）。
  function run(q) {
    q = q.trim();
    if (q.length > MAX_QUERY_RUNES) q = q.slice(0, MAX_QUERY_RUNES);
    var mine = ++seq;
    history.replaceState(null, "", q ? "?q=" + encodeURIComponent(q) : location.pathname);
    document.querySelectorAll("form[role=search] input[name=q]").forEach(function (other) {
      if (other !== input) other.value = q;
    });
    var words = terms(q);
    if (words.length === 0) {
      showMessage(pageLang === "en" ? "Enter keywords to search articles." : "キーワードを入力して記事を検索できます。");
      return;
    }
    target.classList.add("search-results-loading");
    load()
      .then(function (items) {
        if (mine === seq) render(q, words, find(items, words));
      })
      .catch(function () {
        if (mine === seq) showMessage(pageLang === "en" ? "Search could not be initialized. Please reload the page." : "検索の準備ができませんでした。ページを読み込み直してから、もう一度お試しください。");
      })
      .then(function () {
        if (mine === seq) target.classList.remove("search-results-loading");
      });
  }

  input.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { run(input.value); }, DEBOUNCE_MS);
  });
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    clearTimeout(timer);
    run(input.value);
  });

  var initial = new URLSearchParams(location.search).get("q") || "";
  input.value = initial;
  if (initial.trim() !== "") run(initial);
})();
