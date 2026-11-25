// 行揃え（justified）のフォトギャラリー。
// .photo-gallery[data-gallery-layout="justified"] の写真を、切り抜かずに元の縦横比のまま、行ごとに高さをそろえて
// ギャラリーの幅いっぱいに並べる。縦横比は <img> の width・height 属性（無ければ読み込んだ画像の寸法）から求め、
// ギャラリーの幅が変わるたび（画面の回転・ウィンドウの大きさの変更）と、寸法の分からなかった写真を読み込んだときに並べ直す。
// 最後の行は引き伸ばさず、目安の高さのまま左に寄せる（写真が1〜2枚だけ巨大にならない）。ただし最後の行が幅の半分にも
// 届かない（8枚が4枚・3枚・1枚に分かれるなど、端の1枚がぽつんと残る）ときは、行の高さを変えて行数を1つ減らし、すべての
// 行を幅いっぱいにそろえて配り直す（balancedRows。高さは目安の MIN_ROW_SCALE〜MAX_ROW_SCALE 倍に収め、写真を1行に
// 潰しすぎない。収まらなければ同じ行数のまま配り直し、それも収まらなければ元の並べ方のまま）。
// 行の目安の高さと写真の間隔は public.css の --gallery-row-height・--gallery-gap で、テーマが上書きできる。
// スクリプトが動かないときは、public.css が同じ高さの行に写真を左から詰めて表示する（右端はそろわない）。
(function () {
  'use strict';

  var SELECTOR = '.photo-gallery[data-gallery-layout="justified"]';
  // 並べ終えたギャラリーに付けるクラス。public.css はこれが付いてから、写真を枠いっぱいに広げる。
  var READY_CLASS = 'is-justified';
  // 寸法がまだ分からない写真の仮の縦横比（読み込んだら並べ直す）。
  var FALLBACK_RATIO = 3 / 2;
  // 行の高さの上限（目安の高さに対する倍率）。縦長の写真1枚と極端な横長の写真が並んだときなどに、行が高くなりすぎないようにする。
  var MAX_ROW_SCALE = 1.6;
  // 配り直すときの行の高さの下限（目安の高さに対する倍率）。行数を減らしても写真が小さく潰れすぎないようにする。
  var MIN_ROW_SCALE = 0.6;
  // 最後の行の幅がギャラリーの幅のこの割合に届かないとき、端の写真が取り残されたとみなして配り直す。
  var ORPHAN_ROW_RATIO = 0.5;

  var galleries = [];
  var pending = [];
  var frame = 0;

  function num(value) {
    var n = parseFloat(value);
    return isFinite(n) && n > 0 ? n : 0;
  }

  function itemsOf(gallery) {
    // まだ見せていない写真（「もっと見る」まで hidden）は並べない。
    return Array.prototype.filter.call(gallery.children, function (el) { return el.classList.contains('gallery-item') && !el.hidden; });
  }

  // ratioOf は写真の縦横比（幅 / 高さ）を返す。分からなければ 0。
  function ratioOf(img) {
    if (!img) return 0;
    var w = num(img.getAttribute('width'));
    var h = num(img.getAttribute('height'));
    if (w && h) return w / h;
    if (img.naturalWidth && img.naturalHeight) return img.naturalWidth / img.naturalHeight;
    return 0;
  }

  // contentWidth はギャラリーの中身の幅（px、小数を切り捨て）。行の幅の合計がこれを超えると、写真が次の行へ落ちる。
  function contentWidth(gallery, style) {
    var w = gallery.getBoundingClientRect().width -
      num(style.paddingLeft) - num(style.paddingRight) - num(style.borderLeftWidth) - num(style.borderRightWidth);
    return Math.floor(w);
  }

  // rowsFor は縦横比の並び ratios を、幅 width・間隔 gap・目安の高さ target で行に分け、
  // 各行の { start, end（含まない）, height, full（幅いっぱいに広げるか） } を返す。
  // 写真を足すと行が目安より低くなるとき、足した場合と足さない場合のうち、高さが目安に近い方で行を閉じる。
  function rowsFor(ratios, width, gap, target) {
    var rows = [];
    var maxHeight = target * MAX_ROW_SCALE;
    var start = 0;
    var sum = 0;
    function fill(end, total) { return (width - gap * (end - start - 1)) / total; }
    function close(end, total) {
      var h = fill(end, total);
      rows.push({ start: start, end: end, height: Math.min(h, maxHeight), full: h <= maxHeight });
      start = end;
      sum = 0;
    }
    for (var i = 0; i < ratios.length; i++) {
      var r = ratios[i];
      if (i === start) {
        sum = r;
        continue;
      }
      var withIt = fill(i + 1, sum + r);
      if (withIt >= target) {
        sum += r;
        continue;
      }
      var without = fill(i, sum);
      if (Math.abs(Math.log(withIt / target)) <= Math.abs(Math.log(without / target))) {
        close(i + 1, sum + r);
      } else {
        close(i, sum);
        sum = r;
      }
    }
    if (start < ratios.length) {
      // 最後の行: 幅に収まるなら目安の高さのまま（引き伸ばさない）。目安の高さで幅を超える（極端な横長など）ときだけ縮める。
      var h = fill(ratios.length, sum);
      rows.push({ start: start, end: ratios.length, height: Math.min(h, target), full: h <= target });
    }
    return rebalance(rows, ratios, width, gap, target);
  }

  // rowWidth は行 row を高さ row.height で並べたときの幅（間隔を含む）。
  function rowWidth(row, ratios, gap) {
    var sum = 0;
    for (var i = row.start; i < row.end; i++) sum += ratios[i];
    return sum * row.height + gap * (row.end - row.start - 1);
  }

  // rebalance は、最後の行が幅の ORPHAN_ROW_RATIO に届かないとき（端の写真が取り残されたとき）に、行数を1つ減らして
  // すべての行を幅いっぱいにそろえた並べ方を返す。高さの範囲に収まらなければ同じ行数で配り直す。全体で収まらなければ
  // 最後の3行・2行だけを同じように配り直し（それより前の行は元のまま）、どれも収まらなければ rows のまま。
  function rebalance(rows, ratios, width, gap, target) {
    var n = rows.length;
    var last = rows[n - 1];
    if (n < 2 || last.full || rowWidth(last, ratios, gap) >= width * ORPHAN_ROW_RATIO) return rows;
    var tails = [n, 3, 2];
    for (var t = 0; t < tails.length; t++) {
      var m = tails[t];
      if (m > n || (t > 0 && m >= n)) continue;
      var offset = rows[n - m].start;
      var part = ratios.slice(offset);
      var redone = balancedRows(part, width, gap, target, m - 1) || balancedRows(part, width, gap, target, m);
      if (redone) {
        return rows.slice(0, n - m).concat(redone.map(function (row) {
          return { start: row.start + offset, end: row.end + offset, height: row.height, full: true };
        }));
      }
    }
    return rows;
  }

  // balancedRows は縦横比の並び ratios をちょうど count 行に分け、すべての行を幅いっぱい（full）にした並べ方のうち、
  // 各行の高さが目安 target に最も近いもの（高さの比の対数の二乗和が最小）を返す。どの行の高さも target の
  // MIN_ROW_SCALE〜MAX_ROW_SCALE 倍に収まる分け方が無ければ null（幅いっぱいでも下限より低い1枚だけの行は認める）。
  // 写真の順番は変えない。
  function balancedRows(ratios, width, gap, target, count) {
    var n = ratios.length;
    if (count < 1 || count > n) return null;
    var minHeight = target * MIN_ROW_SCALE;
    var maxHeight = target * MAX_ROW_SCALE;
    var prefix = [0];
    for (var p = 0; p < n; p++) prefix.push(prefix[p] + ratios[p]);
    function heightOf(i, j) { return (width - gap * (j - i - 1)) / (prefix[j] - prefix[i]); }
    // cost[r][j]: 先頭の j 枚を r 行に分けたときの最小の費用。from[r][j]: そのときの最後の行の始まり。
    var cost = [];
    var from = [];
    for (var r = 0; r <= count; r++) {
      cost.push(new Array(n + 1).fill(Infinity));
      from.push(new Array(n + 1).fill(-1));
    }
    cost[0][0] = 0;
    for (r = 1; r <= count; r++) {
      for (var j = r; j <= n - (count - r); j++) {
        // 最後の行（i 枚目から j 枚目の手前まで）を後ろから1枚ずつ広げる。写真を足すほど行は低くなるため、下限を割ったら打ち切る。
        // ただし1枚だけの行（横長のパノラマなど、幅いっぱいでも下限より低い写真）はそれ以上高くできないため、そのまま認める。
        for (var i = j - 1; i >= r - 1; i--) {
          var h = heightOf(i, j);
          if (h < minHeight && i < j - 1) break;
          var before = cost[r - 1][i];
          if (h > maxHeight || before === Infinity) continue;
          var d = Math.log(h / target);
          var c = before + d * d;
          if (c < cost[r][j]) {
            cost[r][j] = c;
            from[r][j] = i;
          }
        }
      }
    }
    if (cost[count][n] === Infinity) return null;
    var out = [];
    for (var end = n, k = count; k > 0; k--) {
      var begin = from[k][end];
      out.unshift({ start: begin, end: end, height: heightOf(begin, end), full: true });
      end = begin;
    }
    return out;
  }

  function layout(gallery) {
    var items = itemsOf(gallery);
    if (items.length === 0) return;
    var style = window.getComputedStyle(gallery);
    var width = contentWidth(gallery, style);
    // 非表示（display: none の中など）のときは幅が 0 になる。表示されて幅が変わったら ResizeObserver が呼び直す。
    if (width <= 0) return;
    var gap = num(style.columnGap);
    var target = num(style.getPropertyValue('--gallery-row-height')) || Math.max(140, Math.min(260, width / 3));
    var ratios = items.map(function (item) { return ratioOf(item.querySelector('img')) || FALLBACK_RATIO; });
    rowsFor(ratios, width, gap, target).forEach(function (row) {
      var used = 0;
      for (var i = row.start; i < row.end; i++) {
        // 幅は切り捨てて行の合計が中身の幅を超えないようにし、余り（数 px）は幅いっぱいの行の最後の写真が flex-grow で埋める。
        var w = Math.max(1, Math.floor(ratios[i] * row.height));
        used += w;
        var s = items[i].style;
        s.width = w + 'px';
        s.height = row.height.toFixed(2) + 'px';
        s.flexGrow = row.full && i === row.end - 1 ? '1' : '';
      }
      // 丸めで1行に収まらないときの保険: 最後の写真を縮める。
      var over = used + gap * (row.end - row.start - 1) - width;
      if (over > 0) {
        var last = items[row.end - 1].style;
        last.width = Math.max(1, parseFloat(last.width) - Math.ceil(over)) + 'px';
      }
    });
    gallery.classList.add(READY_CLASS);
    gallery.setAttribute('data-justified-width', String(width));
  }

  // schedule は次の描画の前に gallery を並べ直す（同じフレームの複数の依頼はまとめる）。
  function schedule(gallery) {
    if (pending.indexOf(gallery) < 0) pending.push(gallery);
    if (frame) return;
    frame = window.requestAnimationFrame(function () {
      frame = 0;
      var list = pending;
      pending = [];
      list.forEach(layout);
    });
  }

  function watch(gallery) {
    if (galleries.indexOf(gallery) >= 0) return;
    galleries.push(gallery);
    // 寸法の属性が無い写真は、読み込んで縦横比が分かったら並べ直す（load は伝播しないため捕捉で受ける）。
    gallery.addEventListener('load', function (e) {
      var img = e.target;
      if (img && img.tagName === 'IMG' && !(num(img.getAttribute('width')) && num(img.getAttribute('height')))) schedule(gallery);
    }, true);
    if (resizeObserver) resizeObserver.observe(gallery);
    layout(gallery);
  }

  // ギャラリーの幅が変わったときだけ並べ直す（並べ直しで高さが変わっても、幅が同じなら何もしない）。
  var resizeObserver = typeof window.ResizeObserver === 'function' ? new window.ResizeObserver(function (entries) {
    entries.forEach(function (entry) {
      var gallery = entry.target;
      var style = window.getComputedStyle(gallery);
      if (String(contentWidth(gallery, style)) !== gallery.getAttribute('data-justified-width')) schedule(gallery);
    });
  }) : null;
  if (!resizeObserver) {
    window.addEventListener('resize', function () { galleries.forEach(schedule); });
  }

  function init(root) {
    Array.prototype.forEach.call((root || document).querySelectorAll(SELECTOR), watch);
  }

  // 後から差し込んだ本文などのギャラリーにも使えるよう、並べ直しの入口を公開する。
  window.sorusJustifiedGallery = { init: init, layout: layout, rows: rowsFor, balancedRows: balancedRows };

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { init(); });
  else init();
})();

// 「もっと見る」。limit を書かないタグのギャラリーは最初の一部だけが本文にあり、
// figure の data-gallery-total に総数、data-gallery-more に続きの API（/api/gallery?...。offset はここで足す）がある。
// 直後の p.gallery-more のボタンを押すと続きを読んで figure の末尾に足し、行揃えを並べ直す。data-gallery-more が無い
// （静的な書き出し）ときは、hidden で出力済みの写真を1回に PAGE_SIZE 枚ずつ見せる。
// 拡大表示（lightbox.js）は window.sorusGalleryMore で、最後の写真から次へ進むときに続きを読む。
//
// 年別の絞り込み（:::gallery の year_filter="true"）。figure の直前の div.gallery-year-filter のピル（「すべて」「2025年」…）を
// 押すと、その年の写真だけにする。続きの API で読むギャラリー（data-gallery-more がある）は /api/gallery?...&year=<年> で
// 最初から読み直し、「もっと見る」もその年の続きを読む。本文にすべての写真がある（静的な書き出しなど）ときは、写真の
// data-gallery-years で見分けて合わない写真を hidden にする（data-gallery-out を付け、「もっと見る」の数にも入れない）。
// 拡大表示は hidden の写真を対象にしないため、前後送りも絞り込んだ写真だけになる。
(function () {
  'use strict';

  var PAGE_SIZE = 24;
  var EN = (document.documentElement.getAttribute('lang') || '').toLowerCase().indexOf('en') === 0;
  var loading = typeof WeakMap === 'function' ? new WeakMap() : null;
  // ギャラリーごとの年別の絞り込みの状態 { base: 年の無い続きの API（無ければ ''）, paged: 静的な「もっと見る」か, gen: 選び直した回数 }。
  var filters = typeof WeakMap === 'function' ? new WeakMap() : null;

  function allItemsOf(gallery) {
    return Array.prototype.filter.call(gallery.children, function (el) { return el.classList.contains('gallery-item'); });
  }
  // itemsOf は今の絞り込みに合う写真（data-gallery-out の付いていないもの）。
  function itemsOf(gallery) {
    return allItemsOf(gallery).filter(function (el) { return !el.hasAttribute('data-gallery-out'); });
  }
  function shownCount(gallery) { return itemsOf(gallery).filter(function (el) { return !el.hidden; }).length; }
  function totalOf(gallery) {
    var n = parseInt(gallery.getAttribute('data-gallery-total') || '', 10);
    return isFinite(n) && n > 0 ? n : itemsOf(gallery).length;
  }
  function hasMore(gallery) { return !!gallery && shownCount(gallery) < totalOf(gallery); }
  function barOf(gallery) {
    var el = gallery.nextElementSibling;
    return el && el.classList.contains('gallery-more') ? el : null;
  }
  function galleryOf(bar) {
    var el = bar.previousElementSibling;
    return el && el.classList.contains('photo-gallery') ? el : null;
  }

  function label(remaining) { return EN ? 'Show more (' + remaining + ' left)' : 'もっと見る（残り ' + remaining + ' 件）'; }
  function countLabel(total) { return EN ? total + ' photos' : '全 ' + total + ' 枚'; }

  // update は総枚数とボタンの文言を今の状態にする。すべて見せたらボタンを外す。年別の絞り込みで残りができたときは、
  // 行（p.gallery-more）とボタンを作り直す。
  function update(gallery, failed) {
    var bar = barOf(gallery);
    var total = totalOf(gallery);
    var remaining = Math.max(0, total - shownCount(gallery));
    if (!bar) {
      if (remaining === 0) return;
      bar = document.createElement('p');
      bar.className = 'gallery-more';
      var span = document.createElement('span');
      span.className = 'gallery-count';
      span.setAttribute('aria-live', 'polite');
      bar.appendChild(span);
      gallery.insertAdjacentElement('afterend', bar);
    }
    var count = bar.querySelector('.gallery-count');
    if (count) count.textContent = countLabel(total);
    var button = bar.querySelector('.gallery-more-button');
    if (!button) {
      if (remaining === 0) return;
      button = document.createElement('button');
      button.type = 'button';
      button.className = 'gallery-more-button';
      bar.appendChild(button);
    }
    if (remaining === 0) {
      // ボタンにフォーカスがあったら、足した写真の最後へ移す（フォーカスを見失わせない）。
      if (document.activeElement === button) {
        var items = itemsOf(gallery);
        if (items.length) items[items.length - 1].focus();
      }
      button.remove();
      return;
    }
    button.disabled = false;
    button.removeAttribute('aria-busy');
    button.textContent = failed ? (EN ? 'Could not load. Try again' : '読み込めませんでした。もう一度試す') : label(remaining);
  }

  function relayout(gallery) {
    var j = window.sorusJustifiedGallery;
    if (j && gallery.getAttribute('data-gallery-layout') === 'justified') j.layout(gallery);
  }

  // load は gallery の続きを1回分見せる（読み込む）。終わったら解決する Promise を返す（読み込み中なら同じ Promise）。
  function load(gallery) {
    if (!gallery || !hasMore(gallery)) return Promise.resolve(false);
    if (loading && loading.has(gallery)) return loading.get(gallery);
    var hidden = itemsOf(gallery).filter(function (el) { return el.hidden; });
    var promise;
    if (hidden.length) {
      hidden.slice(0, PAGE_SIZE).forEach(function (el) { el.hidden = false; });
      promise = Promise.resolve(true);
    } else {
      var url = gallery.getAttribute('data-gallery-more');
      if (!url || typeof window.fetch !== 'function') return Promise.resolve(false);
      var gen = genOf(gallery);
      var bar = barOf(gallery);
      var button = bar && bar.querySelector('.gallery-more-button');
      if (button) {
        button.disabled = true;
        button.setAttribute('aria-busy', 'true');
      }
      promise = window.fetch(url + '&offset=' + shownCount(gallery), { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
        .then(function (res) {
          if (!res.ok) throw new Error('gallery: ' + res.status);
          return res.json();
        })
        .then(function (data) {
          // 読み込みの間に年を選び直したら、前の年の続きは捨てる。
          if (gen !== genOf(gallery)) return false;
          if (data && typeof data.html === 'string' && data.count > 0) gallery.insertAdjacentHTML('beforeend', data.html);
          // 写真が減って総数より先が無いときも、ボタンが残り続けないよう総数を合わせる。
          var total = data && data.count > 0 && typeof data.total === 'number' ? data.total : shownCount(gallery);
          gallery.setAttribute('data-gallery-total', String(Math.max(total, shownCount(gallery))));
          return true;
        });
    }
    promise = promise.then(function (ok) {
      if (loading) loading.delete(gallery);
      relayout(gallery);
      update(gallery);
      gallery.dispatchEvent(new CustomEvent('sorus:gallery-more', { bubbles: true }));
      return ok;
    }, function () {
      if (loading) loading.delete(gallery);
      update(gallery, true);
      return false;
    });
    if (loading) loading.set(gallery, promise);
    return promise;
  }

  // ---- 年別の絞り込み ----

  function genOf(gallery) {
    var st = filters && filters.get(gallery);
    return st ? st.gen : 0;
  }
  function filterOf(gallery) {
    var el = gallery.previousElementSibling;
    return el && el.classList.contains('gallery-year-filter') ? el : null;
  }
  function filterGallery(filter) {
    var el = filter.nextElementSibling;
    return el && el.classList.contains('photo-gallery') ? el : null;
  }
  // filterState は gallery の絞り込みの状態を、最初に選んだときの形（続きの API・hidden の有無）から作る。
  function filterState(gallery) {
    var st = filters && filters.get(gallery);
    if (st) return st;
    st = {
      base: gallery.getAttribute('data-gallery-more') || '',
      paged: allItemsOf(gallery).some(function (el) { return el.hidden; }),
      gen: 0
    };
    if (filters) filters.set(gallery, st);
    return st;
  }
  // withYear は続きの API の URL の year を year にする（空なら外す）。offset は load が足す。
  function withYear(base, year) {
    var q = base.indexOf('?');
    var params = new URLSearchParams(q >= 0 ? base.slice(q + 1) : '');
    if (year) params.set('year', year);
    else params.delete('year');
    return (q >= 0 ? base.slice(0, q) : base) + '?' + params.toString();
  }
  function markPills(filter, year) {
    Array.prototype.forEach.call(filter.querySelectorAll('.gallery-year-pill'), function (pill) {
      var on = (pill.getAttribute('data-gallery-year') || '') === year;
      pill.classList.toggle('is-active', on);
      pill.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }
  function activeYear(filter) {
    var pill = filter.querySelector('.gallery-year-pill.is-active');
    return pill ? pill.getAttribute('data-gallery-year') || '' : '';
  }
  function filtered(gallery) {
    if (loading) loading.delete(gallery);
    relayout(gallery);
    update(gallery);
    gallery.dispatchEvent(new CustomEvent('sorus:gallery-filter', { bubbles: true }));
  }

  // filterLocal は本文にある写真を data-gallery-years で絞り込む。静的な「もっと見る」のギャラリーは最初の PAGE_SIZE 枚だけ見せる。
  function filterLocal(gallery, st, year) {
    var n = 0;
    allItemsOf(gallery).forEach(function (el) {
      var years = (el.getAttribute('data-gallery-years') || '').split(' ');
      if (!year || years.indexOf(year) >= 0) {
        el.removeAttribute('data-gallery-out');
        el.hidden = st.paged && n >= PAGE_SIZE;
        n++;
      } else {
        el.setAttribute('data-gallery-out', '');
        el.hidden = true;
      }
    });
    gallery.setAttribute('data-gallery-total', String(n));
    filtered(gallery);
    return Promise.resolve(true);
  }

  // filterRemote は続きの API からその年の写真を最初から読み直す。
  function filterRemote(gallery, st, year, gen) {
    var url = withYear(st.base, year);
    gallery.setAttribute('aria-busy', 'true');
    return window.fetch(url + '&offset=0', { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
      .then(function (res) {
        if (!res.ok) throw new Error('gallery: ' + res.status);
        return res.json();
      })
      .then(function (data) {
        if (gen !== st.gen) return false;
        gallery.removeAttribute('aria-busy');
        allItemsOf(gallery).forEach(function (el) { el.remove(); });
        if (data && typeof data.html === 'string' && data.count > 0) gallery.insertAdjacentHTML('beforeend', data.html);
        var total = data && typeof data.total === 'number' ? data.total : 0;
        gallery.setAttribute('data-gallery-total', String(Math.max(total, shownCount(gallery))));
        gallery.setAttribute('data-gallery-more', url);
        filtered(gallery);
        return true;
      });
  }

  // selectYear は filter のギャラリーを年 year（空なら すべて）に絞り込む。
  function selectYear(filter, year) {
    var gallery = filterGallery(filter);
    if (!gallery) return Promise.resolve(false);
    var before = activeYear(filter);
    var st = filterState(gallery);
    st.gen++;
    var gen = st.gen;
    markPills(filter, year);
    if (!st.base || typeof window.fetch !== 'function' || typeof URLSearchParams !== 'function') return filterLocal(gallery, st, year);
    return filterRemote(gallery, st, year, gen).then(null, function () {
      if (gen !== st.gen) return false;
      // 読めなければ、選ぶ前の年のピルに戻す（写真は前のまま）。
      gallery.removeAttribute('aria-busy');
      markPills(filter, before);
      return false;
    });
  }

  function initFilter(filter) {
    if (!EN) return;
    filter.setAttribute('aria-label', 'Filter by year');
    var all = filter.querySelector('.gallery-year-pill[data-gallery-year=""]');
    if (all) all.textContent = 'All';
  }

  function init() {
    Array.prototype.forEach.call(document.querySelectorAll('.gallery-year-filter'), initFilter);
    Array.prototype.forEach.call(document.querySelectorAll('.gallery-more'), function (bar) {
      var gallery = galleryOf(bar);
      if (!gallery) return;
      var count = bar.querySelector('.gallery-count');
      if (count) count.setAttribute('aria-live', 'polite');
      update(gallery);
    });
  }

  document.addEventListener('click', function (e) {
    var pill = e.target && e.target.closest ? e.target.closest('.gallery-year-pill') : null;
    var filter = pill && pill.closest('.gallery-year-filter');
    if (filter) {
      var year = pill.getAttribute('data-gallery-year') || '';
      if (year !== activeYear(filter) || pill.getAttribute('aria-pressed') !== 'true') selectYear(filter, year);
      return;
    }
    var button = e.target && e.target.closest ? e.target.closest('.gallery-more-button') : null;
    if (!button) return;
    var bar = button.closest('.gallery-more');
    var gallery = bar && galleryOf(bar);
    if (gallery) load(gallery);
  });

  window.sorusGalleryMore = { load: load, hasMore: hasMore, total: totalOf };
  // 年別の絞り込みの入口（後から差し込んだ本文や、テストから年を選ぶ）。
  window.sorusGalleryMore.selectYear = selectYear;
  window.sorusGalleryMore.filterOf = filterOf;

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
