// 公開サイト共通の画像拡大表示。
// :::gallery の .gallery-item、トップの .cover-gallery、本文 .prose の画像、data-lightbox-group を付けた画像を
// クリック（または Enter / Space）すると、レイアウト共通の <dialog id="cms-lightbox"> で開く。
// - 同じギャラリー（グループ）の画像を左右ボタン・←→キー・横スワイプで続けてめくり、「3 / 14」の位置を読み上げる。
// - ピンチ・ダブルタップ（ダブルクリック）・Ctrl+ホイールで拡大し、拡大中はドラッグで画像を動かす（めくりより優先）。
// - Esc・×・背景タップ・下スワイプ・ブラウザの「戻る」で閉じる。開くと履歴に #lightbox を積み、閉じると元に戻す。
// - 等倍の画像をタップすると、ボタンとキャプションを隠す／出す。
// View Transitions API に対応したブラウザではサムネイルから拡大画像へ変形させ、未対応なら即座に切り替える。
// JavaScript が無効でもサムネイル画像とキャプションはそのまま読める（プログレッシブエンハンスメント）。
(function () {
  "use strict";
  var TRANSITION_NAME = "cms-lightbox-image";
  var DECODE_WAIT_MS = 300;
  var HISTORY_HASH = "#lightbox";
  var MAX_SCALE = 4;
  var DOUBLE_TAP_SCALE = 2.5;
  var TAP_SLOP = 10; // これ以下の移動はタップとみなす（px）
  var DOUBLE_TAP_MS = 280;
  var SWIPE_DISTANCE = 60; // めくりが決まる横移動（px）
  var CLOSE_DISTANCE = 110; // 閉じるが決まる下移動（px）
  var FLICK_VELOCITY = 0.5; // 距離が足りなくても、これより速く払えば決まる（px/ms）

  var dialog = document.getElementById("cms-lightbox");
  if (!dialog || typeof dialog.showModal !== "function") return;
  var image = dialog.querySelector(".cms-lightbox-image");
  var stage = dialog.querySelector(".cms-lightbox-stage") || image.parentNode;
  var caption = dialog.querySelector(".cms-lightbox-caption");
  var counter = dialog.querySelector(".cms-lightbox-counter");
  var closeButton = dialog.querySelector(".cms-lightbox-close");
  var prevButton = dialog.querySelector(".cms-lightbox-prev");
  var nextButton = dialog.querySelector(".cms-lightbox-next");
  var reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

  var items = []; // 開いているグループの画像 { thumb, trigger, caption }
  var group = null; // 開いている画像の .photo-gallery（「もっと見る」の続きを読む対象）。無ければ null
  var loadingMore = false; // 最後の写真から次へ進むために続きを読んでいる間
  var index = 0;
  var activeThumb = null; // 拡大表示中の元サムネイル（閉じるときの変形先）
  var closingThumb = null; // 閉じる遷移の終わりに名前を外すサムネイル
  var returnFocus = null; // 閉じたあとにフォーカスを戻す要素
  var busy = false; // 遷移中の連打で開閉が重ならないようにする。
  var pendingClose = false; // 遷移中に「戻る」が押されたら、遷移の後で閉じる。
  var historyPushed = false; // 開くときに #lightbox を積んだか
  var ignorePop = false; // 自分で history.back() したときの popstate を無視する。
  var ignorePopTimer = 0;

  // ---- 拡大表示できる画像の判定とグループ ----

  var GROUP_SELECTOR = ".photo-gallery, .cover-gallery";

  function inLightbox(el) { return !!el.closest("#cms-lightbox"); }

  // 拡大表示の対象になる画像か。リンクで囲まれた画像はリンクの移動を優先する（data-full があれば拡大する）。
  function isEligible(img) {
    if (!img || img.tagName !== "IMG" || inLightbox(img)) return false;
    // 「もっと見る」までまだ見せていない写真（hidden）は対象にしない。
    if (img.closest(".gallery-item[hidden]")) return false;
    if (img.classList.contains("emoji")) return false;
    var hasFull = img.hasAttribute("data-full");
    if (!hasFull && img.closest("a[href]")) return false;
    if (hasFull) return true;
    return !!img.closest(".gallery-item, .cover-gallery, [data-lightbox-group], .prose");
  }

  // 拡大表示中にフォーカスを戻す要素（.gallery-item、画像を囲むリンク、画像そのもの）。
  function triggerFor(img) {
    return img.closest(".gallery-item") || img.closest("a[href]") || img.closest(".cover-gallery figure[tabindex]") || img;
  }

  function captionFor(img) {
    var holder = img.closest(".gallery-item");
    if (!holder) {
      var fig = img.closest("figure");
      if (fig && !fig.classList.contains("photo-gallery") && !fig.classList.contains("cover-gallery")) holder = fig;
    }
    if (!holder) return "";
    var cap = holder.querySelector("figcaption");
    return cap && !inLightbox(cap) ? cap.textContent.trim() : "";
  }

  function eligibleIn(root) {
    var list = root.tagName === "IMG" ? [root] : Array.prototype.slice.call(root.querySelectorAll("img"));
    return list.filter(isEligible);
  }

  // 画像が属するグループの画像を文書の順で集める。
  function collectGroup(img) {
    var list;
    var named = img.closest("[data-lightbox-group]");
    var gallery = img.closest(GROUP_SELECTOR);
    var prose = img.closest(".prose");
    if (named && named.getAttribute("data-lightbox-group") !== "") {
      var name = named.getAttribute("data-lightbox-group");
      list = [];
      Array.prototype.forEach.call(document.querySelectorAll("[data-lightbox-group]"), function (el) {
        if (el.getAttribute("data-lightbox-group") !== name) return;
        eligibleIn(el).forEach(function (m) { if (list.indexOf(m) < 0) list.push(m); });
      });
    } else if (gallery) {
      list = eligibleIn(gallery);
    } else if (prose) {
      // 本文の中のギャラリーはそれぞれ別のグループなので、本文の画像群からは外す。
      list = eligibleIn(prose).filter(function (m) { return !m.closest(GROUP_SELECTOR); });
    } else {
      list = [img];
    }
    if (list.indexOf(img) < 0) list = [img];
    return list.map(function (m) { return { thumb: m, trigger: triggerFor(m), caption: captionFor(m) }; });
  }

  // クリック・キー操作の対象から、拡大する画像を見つける。
  function findImage(target) {
    if (!target || !target.closest || inLightbox(target)) return null;
    var item = target.closest(".gallery-item");
    if (item) return item.querySelector("img");
    var img = target.closest("img");
    if (img) return isEligible(img) ? img : null;
    var fig = target.closest(".cover-gallery figure");
    if (fig && !target.closest("a[href]")) {
      var inner = fig.querySelector("img");
      return isEligible(inner) ? inner : null;
    }
    return null;
  }

  // キーボードでも開けるよう、.gallery-item 以外の対象画像にフォーカスと役割を付ける。
  function enhance() {
    var label = dialog.getAttribute("data-trigger-label") || "";
    Array.prototype.forEach.call(document.querySelectorAll(".prose img, .cover-gallery img, [data-lightbox-group] img, img[data-lightbox-group], img[data-full]"), function (img) {
      if (!isEligible(img)) return;
      var el = triggerFor(img);
      el.classList.add("cms-lightbox-trigger");
      if (el.classList.contains("gallery-item") || el.tagName === "A" || el.hasAttribute("tabindex")) return;
      el.setAttribute("tabindex", "0");
      el.setAttribute("role", "button");
      var name = img.alt || captionFor(img);
      if (label && !el.hasAttribute("aria-label")) el.setAttribute("aria-label", name ? label + ": " + name : label);
    });
  }

  // ---- View Transitions ----

  // update を View Transition の中で実行する。busy は DOM の更新が終わった時点で解除し、
  // done はアニメーションが終わったら（未対応なら直後に）呼ぶ。
  function withTransition(update, done) {
    done = done || function () {};
    if (typeof document.startViewTransition !== "function" || reducedMotion.matches) {
      update();
      done();
      afterBusy();
      return;
    }
    busy = true;
    var transition = document.startViewTransition(update);
    var release = function () { busy = false; afterBusy(); };
    transition.updateCallbackDone.then(release, release);
    transition.finished.then(done, done);
  }

  function afterBusy() {
    if (pendingClose && dialog.open) {
      pendingClose = false;
      close({ fromHistory: true });
    }
  }

  // 拡大画像のデコードを短時間だけ待ち、遷移後の画像が空のまま変形しないようにする。
  function waitForImage() {
    if (typeof image.decode !== "function") return Promise.resolve();
    return Promise.race([
      image.decode().catch(function () {}),
      new Promise(function (resolve) { setTimeout(resolve, DECODE_WAIT_MS); })
    ]);
  }

  function fullURL(thumb) {
    return thumb.getAttribute("data-full") || thumb.currentSrc || thumb.src;
  }

  // ---- 表示の更新 ----

  function render() {
    var entry = items[index];
    image.src = fullURL(entry.thumb);
    image.alt = entry.thumb.alt;
    caption.textContent = entry.caption;
    caption.hidden = entry.caption === "";
    // 「もっと見る」の続きがあるギャラリーは、まだ読んでいない写真も含めた総数で数える。
    var total = Math.max(items.length, moreTotal());
    var many = total > 1;
    counter.hidden = !many;
    counter.textContent = many ? (index + 1) + " / " + total : "";
    prevButton.hidden = !many;
    nextButton.hidden = !many;
    setDisabled(prevButton, index === 0);
    setDisabled(nextButton, index === items.length - 1 && !canLoadMore());
    preload(index - 1);
    preload(index + 1);
  }

  // 端で押せなくなったボタンにフォーカスがあれば、反対側（なければ ×）へ移す。
  function setDisabled(button, disabled) {
    if (disabled && document.activeElement === button) {
      var other = button === prevButton ? nextButton : prevButton;
      (other.hidden || other.disabled ? closeButton : other).focus();
    }
    button.disabled = disabled;
  }

  // ---- 「もっと見る」の続き（justified_gallery.js の window.sorusGalleryMore）----

  function moreAPI() { return window.sorusGalleryMore || null; }
  function canLoadMore() { var api = moreAPI(); return !!(group && api && api.hasMore(group)); }
  function moreTotal() { var api = moreAPI(); return group && api ? api.total(group) : 0; }

  // 最後の写真から次へ進むとき、ギャラリーの続きを読んでから進む。読めなければ元の位置に戻す。
  function loadMoreThenGo(dir, fromX) {
    if (loadingMore) return;
    loadingMore = true;
    setDisabled(nextButton, true);
    moreAPI().load(group).then(function () {
      loadingMore = false;
      if (!dialog.open) return;
      var current = items[index].thumb;
      items = collectGroup(current);
      index = Math.max(0, items.map(function (it) { return it.thumb; }).indexOf(current));
      if (index + dir < items.length) go(dir, fromX);
      else { render(); snapBack(); }
    }, function () {
      loadingMore = false;
      if (dialog.open) render();
      snapBack();
    });
  }

  function preload(i) {
    if (i < 0 || i >= items.length) return;
    var pre = new Image();
    pre.src = fullURL(items[i].thumb);
  }

  function setUIHidden(hidden) {
    dialog.classList.toggle("is-ui-hidden", hidden);
  }

  // ---- 開く・めくる・閉じる ----

  function open(img) {
    if (!img || busy || dialog.open || ignorePop) return;
    items = collectGroup(img);
    group = img.closest(".photo-gallery");
    index = 0;
    for (var i = 0; i < items.length; i++) if (items[i].thumb === img) index = i;
    returnFocus = items[index].trigger;

    // 前回の閉じる遷移が終わる前に開いた場合に、名前が重複しないよう外しておく。
    if (closingThumb) closingThumb.style.viewTransitionName = "";
    closingThumb = null;
    activeThumb = img;
    resetZoom(false);
    setUIHidden(false);
    img.style.viewTransitionName = TRANSITION_NAME;
    withTransition(function () {
      img.style.viewTransitionName = "";
      image.style.viewTransitionName = TRANSITION_NAME;
      render();
      dialog.showModal();
      closeButton.focus({ preventScroll: true });
      return waitForImage();
    });
    pushHistory();
  }

  // dir: -1 で前、+1 で次。fromX はスワイプで画像が既に動いている量。
  function go(dir, fromX) {
    var next = index + dir;
    if (!busy && dialog.open && dir > 0 && next >= items.length && canLoadMore()) {
      loadMoreThenGo(dir, fromX);
      return;
    }
    if (busy || !dialog.open || next < 0 || next >= items.length) {
      snapBack();
      return;
    }
    index = next;
    activeThumb = items[index].thumb;
    returnFocus = items[index].trigger;
    resetZoom(false);
    var animate = !reducedMotion.matches && typeof image.animate === "function";
    var width = stage.clientWidth || window.innerWidth;
    var startX = typeof fromX === "number" ? fromX : 0;
    if (!animate) {
      render();
      return;
    }
    // いまの画像を払った向きへ流してから、次の画像を反対側から入れる。
    var out = image.animate(
      [{ transform: "translateX(" + startX + "px)", opacity: 1 }, { transform: "translateX(" + (-dir * width * 0.35) + "px)", opacity: 0 }],
      { duration: 140, easing: "ease-in" }
    );
    busy = true;
    out.onfinish = function () {
      render();
      waitForImage().then(function () {
        busy = false;
        image.animate(
          [{ transform: "translateX(" + (dir * width * 0.25) + "px)", opacity: 0 }, { transform: "translateX(0)", opacity: 1 }],
          { duration: 200, easing: "cubic-bezier(.2, 0, 0, 1)" }
        );
        afterBusy();
      });
    };
  }

  // opts.fromHistory: 「戻る」で閉じたとき（履歴はもう戻っている）。
  function close(opts) {
    opts = opts || {};
    if (!dialog.open) return;
    if (busy) {
      if (opts.fromHistory) pendingClose = true;
      return;
    }
    if (!opts.fromHistory) popHistory();
    historyPushed = false;
    var thumb = activeThumb;
    var focusTarget = returnFocus;
    activeThumb = null;
    closingThumb = thumb;
    endDrag();
    // めくった先のサムネイルが画面外なら、変形先が見えるようにページ側をそっと寄せる。
    if (thumb && typeof thumb.scrollIntoView === "function") {
      var r = thumb.getBoundingClientRect();
      if (r.bottom < 0 || r.top > window.innerHeight) thumb.scrollIntoView({ block: "center" });
    }
    // 拡大・下スワイプ中の見た目のまま遷移を始め、等倍へ戻すのは遷移の中で行う。
    withTransition(function () {
      resetZoom(false);
      image.style.viewTransitionName = "";
      dialog.close();
      if (thumb) thumb.style.viewTransitionName = TRANSITION_NAME;
      // 遷移中はフォーカスの自動復帰が効かないことがあるため、開いた要素へ明示的に戻す。
      if (focusTarget && typeof focusTarget.focus === "function") focusTarget.focus({ preventScroll: true });
    }, function () {
      if (thumb && closingThumb === thumb) {
        thumb.style.viewTransitionName = "";
        closingThumb = null;
      }
    });
  }

  // ---- 履歴（ブラウザの「戻る」で閉じる） ----

  function pushHistory() {
    if (!window.history || typeof history.pushState !== "function") return;
    try {
      history.pushState({ cmsLightbox: true }, "", location.pathname + location.search + HISTORY_HASH);
      historyPushed = true;
    } catch (e) {
      historyPushed = false;
    }
  }

  // ×・Esc などで閉じたら、積んだ #lightbox を取り除いて元の URL に戻す。
  function popHistory() {
    if (!historyPushed || !(history.state && history.state.cmsLightbox)) return;
    historyPushed = false;
    ignorePop = true;
    clearTimeout(ignorePopTimer);
    // popstate が届かない環境でも、次に開けなくならないようにする。
    ignorePopTimer = setTimeout(function () { ignorePop = false; }, 1000);
    history.back();
  }

  window.addEventListener("popstate", function () {
    if (ignorePop) {
      ignorePop = false;
      clearTimeout(ignorePopTimer);
      return;
    }
    if (dialog.open && historyPushed) {
      historyPushed = false;
      close({ fromHistory: true });
    }
  });

  // #lightbox のまま再読み込みされたら、拡大表示は開かないので URL だけ元に戻す。
  if (location.hash === HISTORY_HASH && history.state && history.state.cmsLightbox) {
    try { history.replaceState(null, "", location.pathname + location.search); } catch (e) { /* 無視 */ }
  }

  // ---- 拡大（ピンチ・ダブルタップ・Ctrl+ホイール）とパン ----

  var scale = 1;
  var tx = 0;
  var ty = 0;

  function applyTransform() {
    image.style.transform = scale === 1 && tx === 0 && ty === 0 ? "" : "translate(" + tx + "px, " + ty + "px) scale(" + scale + ")";
    dialog.classList.toggle("is-zoomed", scale > 1);
  }

  // 拡大した画像の端が画面の内側へ入り込みすぎないように移動量を抑える。
  function clampPan() {
    var maxX = Math.max(0, (image.offsetWidth * scale - stage.clientWidth) / 2);
    var maxY = Math.max(0, (image.offsetHeight * scale - stage.clientHeight) / 2);
    tx = Math.min(maxX, Math.max(-maxX, tx));
    ty = Math.min(maxY, Math.max(-maxY, ty));
  }

  // 変形していないときの画像の中心（画面座標）。
  function imageCenter() {
    var r = image.getBoundingClientRect();
    return { x: r.left + r.width / 2 - tx, y: r.top + r.height / 2 - ty };
  }

  // 画面上の点 (px, py) を動かさずに倍率を next にする。
  function zoomAt(next, px, py, animate) {
    next = Math.min(MAX_SCALE, Math.max(1, next));
    var c = imageCenter();
    var fx = px - c.x;
    var fy = py - c.y;
    tx = fx - (fx - tx) * next / scale;
    ty = fy - (fy - ty) * next / scale;
    scale = next;
    if (scale === 1) { tx = 0; ty = 0; }
    clampPan();
    setAnimated(animate);
    applyTransform();
    if (scale > 1) setUIHidden(true);
  }

  function resetZoom(animate) {
    scale = 1;
    tx = 0;
    ty = 0;
    setAnimated(animate);
    applyTransform();
  }

  function setAnimated(animate) {
    image.classList.toggle("is-animating", !!animate && !reducedMotion.matches);
  }

  // 等倍に戻したら、拡大のときに隠した操作 UI を出し直す。
  function zoomOut() {
    resetZoom(true);
    setUIHidden(false);
  }

  function toggleZoom(px, py) {
    if (scale > 1) zoomOut();
    else zoomAt(DOUBLE_TAP_SCALE, px, py, true);
  }

  stage.addEventListener("wheel", function (e) {
    if (!dialog.open || !(e.ctrlKey || e.metaKey)) return;
    e.preventDefault();
    zoomAt(scale * Math.exp(-e.deltaY / 200), e.clientX, e.clientY, false);
  }, { passive: false });

  // ---- ポインター操作（タップ・スワイプ・ピンチ・パン・下スワイプで閉じる） ----

  var pointers = {}; // pointerId -> { x, y }
  var gesture = null; // 進行中のジェスチャー
  var lastTap = null; // ダブルタップ判定用 { time, x, y }
  var tapTimer = 0;

  function pointerCount() { return Object.keys(pointers).length; }

  function pinchInfo() {
    var ids = Object.keys(pointers);
    var a = pointers[ids[0]];
    var b = pointers[ids[1]];
    return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, dist: Math.hypot(a.x - b.x, a.y - b.y) || 1 };
  }

  function startPinch() {
    var p = pinchInfo();
    var c = imageCenter();
    gesture = { type: "pinch", scale: scale, tx: tx, ty: ty, cx: c.x, cy: c.y, mid: p, moved: true };
    setAnimated(false);
  }

  function endDrag() {
    dialog.classList.remove("is-dragging");
    dialog.style.removeProperty("--cms-lightbox-dim");
  }

  function snapBack() {
    endDrag();
    setAnimated(true);
    applyTransform();
  }

  dialog.addEventListener("pointerdown", function (e) {
    if (!dialog.open || busy) return;
    if (e.pointerType === "mouse" && e.button !== 0) return;
    if (e.target.closest("button, a, .cms-lightbox-caption")) return;
    pointers[e.pointerId] = { x: e.clientX, y: e.clientY };
    if (typeof dialog.setPointerCapture === "function") {
      try { dialog.setPointerCapture(e.pointerId); } catch (err) { /* 無視 */ }
    }
    if (pointerCount() === 2) {
      startPinch();
      return;
    }
    if (pointerCount() > 2) return;
    gesture = {
      type: "pending",
      startX: e.clientX,
      startY: e.clientY,
      lastX: e.clientX,
      lastY: e.clientY,
      startTime: e.timeStamp,
      tx: tx,
      ty: ty,
      onImage: e.target === image,
      moved: false
    };
  });

  dialog.addEventListener("pointermove", function (e) {
    var p = pointers[e.pointerId];
    if (!p || !gesture) return;
    p.x = e.clientX;
    p.y = e.clientY;
    if (gesture.type === "pinch") {
      if (pointerCount() < 2) return;
      var now = pinchInfo();
      var next = Math.min(MAX_SCALE, Math.max(1, gesture.scale * now.dist / gesture.mid.dist));
      // 指の間の点に乗っていた画像の位置が、指と一緒に動くようにする。
      var lx = (gesture.mid.x - gesture.cx - gesture.tx) / gesture.scale;
      var ly = (gesture.mid.y - gesture.cy - gesture.ty) / gesture.scale;
      scale = next;
      tx = now.x - gesture.cx - lx * scale;
      ty = now.y - gesture.cy - ly * scale;
      clampPan();
      applyTransform();
      return;
    }
    var dx = e.clientX - gesture.startX;
    var dy = e.clientY - gesture.startY;
    gesture.velocityX = (e.clientX - gesture.lastX) / Math.max(1, e.timeStamp - (gesture.lastTime || gesture.startTime));
    gesture.velocityY = (e.clientY - gesture.lastY) / Math.max(1, e.timeStamp - (gesture.lastTime || gesture.startTime));
    gesture.lastX = e.clientX;
    gesture.lastY = e.clientY;
    gesture.lastTime = e.timeStamp;
    if (gesture.type === "pending") {
      if (Math.hypot(dx, dy) < TAP_SLOP) return;
      gesture.moved = true;
      if (scale > 1) gesture.type = "pan";
      else if (Math.abs(dx) > Math.abs(dy)) gesture.type = "swipe";
      else if (dy > 0) gesture.type = "dismiss";
      else gesture.type = "none";
      setAnimated(false);
      if (gesture.type === "swipe" || gesture.type === "dismiss") dialog.classList.add("is-dragging");
    }
    if (gesture.type === "pan") {
      tx = gesture.tx + dx;
      ty = gesture.ty + dy;
      clampPan();
      applyTransform();
    } else if (gesture.type === "swipe") {
      // 端の画像ではゴムのように重く動かし、めくれないことを伝える（続きを読める最後の画像は端ではない）。
      var atEdge = (dx > 0 && index === 0) || (dx < 0 && index === items.length - 1 && !canLoadMore());
      image.style.transform = "translateX(" + (atEdge ? dx / 3 : dx) + "px)";
    } else if (gesture.type === "dismiss") {
      var progress = Math.min(1, Math.max(0, dy) / (window.innerHeight * 0.6));
      image.style.transform = "translate(" + dx * 0.5 + "px, " + Math.max(0, dy) + "px) scale(" + (1 - progress * 0.35) + ")";
      dialog.style.setProperty("--cms-lightbox-dim", String(1 - progress * 0.8));
    }
  });

  function finishPointer(e) {
    if (!pointers[e.pointerId]) return;
    delete pointers[e.pointerId];
    if (!gesture) return;
    if (gesture.type === "pinch") {
      if (pointerCount() > 0) {
        // 1本指が残ったら、そこからパンを続ける。
        var rest = pointers[Object.keys(pointers)[0]];
        if (scale < 1.05) zoomOut();
        gesture = { type: scale > 1 ? "pan" : "none", startX: rest.x, startY: rest.y, lastX: rest.x, lastY: rest.y, startTime: e.timeStamp, tx: tx, ty: ty, moved: true };
        return;
      }
      if (scale < 1.05) zoomOut();
      gesture = null;
      return;
    }
    if (pointerCount() > 0) return;
    var g = gesture;
    gesture = null;
    var cancelled = e.type === "pointercancel";
    var dx = e.clientX - g.startX;
    var dy = e.clientY - g.startY;
    if (!g.moved) {
      if (!cancelled) handleTap(e, g);
      return;
    }
    if (g.type === "swipe") {
      var flick = Math.abs(g.velocityX || 0) > FLICK_VELOCITY;
      var dir = dx < 0 ? 1 : -1;
      var target = index + dir;
      // 読み込み済みの最後の画像から次へめくったときは、go がギャラリーの続きを読んでから進む（loadMoreThenGo）。
      if (!cancelled && (Math.abs(dx) > SWIPE_DISTANCE || flick) && target >= 0 && (target < items.length || (dir > 0 && canLoadMore()))) {
        endDrag();
        image.style.transform = "";
        go(dir, dx);
      } else {
        snapBack();
      }
    } else if (g.type === "dismiss") {
      if (!cancelled && (dy > CLOSE_DISTANCE || (g.velocityY || 0) > FLICK_VELOCITY)) {
        // 指を離した位置からそのまま閉じる（View Transition はサムネイルへ戻る）。
        endDrag();
        close();
      } else {
        snapBack();
      }
    } else if (g.type === "none") {
      snapBack();
    }
  }

  dialog.addEventListener("pointerup", finishPointer);
  dialog.addEventListener("pointercancel", finishPointer);

  // タップ：画像は1回で操作 UI の表示切り替え、2回で拡大／縮小。背景は閉じる。
  function handleTap(e, g) {
    var now = e.timeStamp;
    var isDouble = lastTap && now - lastTap.time < DOUBLE_TAP_MS && Math.hypot(e.clientX - lastTap.x, e.clientY - lastTap.y) < 30;
    if (g.onImage) {
      if (isDouble) {
        clearTimeout(tapTimer);
        lastTap = null;
        toggleZoom(e.clientX, e.clientY);
        return;
      }
      lastTap = { time: now, x: e.clientX, y: e.clientY };
      clearTimeout(tapTimer);
      tapTimer = setTimeout(function () {
        lastTap = null;
        if (dialog.open && scale === 1) setUIHidden(!dialog.classList.contains("is-ui-hidden"));
      }, DOUBLE_TAP_MS);
      return;
    }
    lastTap = null;
    // 拡大中の背景タップは、まず等倍に戻す。
    if (scale > 1) {
      zoomOut();
      return;
    }
    close();
  }

  // ダブルクリックでのテキスト選択や、マウスでの画像ドラッグを起こさない。
  dialog.addEventListener("dblclick", function (e) { e.preventDefault(); });
  image.addEventListener("dragstart", function (e) { e.preventDefault(); });

  // ---- キーボードとボタン ----

  function focusables() {
    return Array.prototype.filter.call(dialog.querySelectorAll("button, [href], [tabindex]:not([tabindex='-1'])"), function (el) {
      return !el.disabled && !el.hidden && el.getClientRects().length > 0;
    });
  }

  dialog.addEventListener("keydown", function (e) {
    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
      e.preventDefault();
      go(e.key === "ArrowRight" ? 1 : -1);
      return;
    }
    if (e.key === "Tab") {
      // フォーカスを拡大表示の中だけで巡回させる（ブラウザの UI へも抜けない）。
      var list = focusables();
      if (list.length === 0) return;
      var first = list[0];
      var last = list[list.length - 1];
      var current = list.indexOf(document.activeElement);
      if (e.shiftKey && (current <= 0)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && (current === -1 || current === list.length - 1)) {
        e.preventDefault();
        first.focus();
      }
    }
  });

  // 隠したボタンへ Tab で来たら、操作 UI を出し直す。
  dialog.addEventListener("focusin", function (e) {
    if (e.target.closest && e.target.closest("button")) setUIHidden(false);
  });

  closeButton.addEventListener("click", function () { close(); });
  prevButton.addEventListener("click", function () { go(-1); });
  nextButton.addEventListener("click", function () { go(1); });

  // ESC キー。ブラウザ既定の即時クローズの代わりに、開くときと同じ遷移で閉じる。
  dialog.addEventListener("cancel", function (e) {
    e.preventDefault();
    close();
  });

  // ブラウザが cancel を取り消させずに閉じた場合（Esc の連打、Android の戻る操作など）も、状態と履歴を戻しておく。
  dialog.addEventListener("close", function () {
    if (dialog.open) return;
    popHistory();
    historyPushed = false;
    activeThumb = null;
    pointers = {};
    gesture = null;
    clearTimeout(tapTimer);
    image.style.viewTransitionName = "";
    resetZoom(false);
    endDrag();
    setUIHidden(false);
  });

  // ---- 開くきっかけ ----

  document.addEventListener("click", function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var img = findImage(e.target);
    if (!img) return;
    // data-full 付きの画像がリンクの中にあるときは、移動せずに拡大表示する。
    if (e.target.closest("a[href]")) e.preventDefault();
    open(img);
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    var t = e.target;
    if (!t.closest || inLightbox(t)) return;
    var host = t.closest(".gallery-item, .cms-lightbox-trigger");
    if (!host || host !== t) return;
    // リンクの Enter はリンクの既定動作（クリック）に任せる。
    if (host.tagName === "A") return;
    var img = host.tagName === "IMG" ? host : host.querySelector("img");
    if (!isEligible(img) && !host.classList.contains("gallery-item")) return;
    e.preventDefault();
    open(img);
  });

  enhance();
})();
