// ダッシュボードのスクリプト: PV 推移グラフの棒にホバー・フォーカスすると、日付・PV・訪問者数のツールチップを出す。
(function () {
  'use strict';
  var T = window.sorusT || function (key) { return key; };

  var svg = document.querySelector('.pv-chart');
  var tooltip = document.getElementById('pv-tooltip');
  if (!svg || !tooltip) return;
  var width = svg.viewBox.baseVal.width;
  var height = svg.viewBox.baseVal.height;

  function show(bar) {
    tooltip.textContent = '';
    var date = document.createElement('strong');
    date.textContent = bar.dataset.date;
    var views = document.createElement('span');
    views.textContent = 'PV ' + bar.dataset.views;
    var visitors = document.createElement('span');
    visitors.textContent = T('js_visitors', bar.dataset.visitors);
    tooltip.appendChild(date);
    tooltip.appendChild(views);
    tooltip.appendChild(visitors);
    tooltip.hidden = false;
    // viewBox の座標を表示中の大きさに換算し、棒の上端の少し上に置く（左右端ではみ出さないよう寄せる）。
    var rect = svg.getBoundingClientRect();
    var x = parseFloat(bar.dataset.x) * rect.width / width;
    var y = parseFloat(bar.dataset.y) * rect.height / height;
    var half = tooltip.offsetWidth / 2;
    tooltip.style.left = Math.min(Math.max(x, half), rect.width - half) + 'px';
    tooltip.style.top = Math.max(y - tooltip.offsetHeight - 8, 0) + 'px';
    svg.querySelectorAll('.chart-bar.is-active').forEach(function (el) { el.classList.remove('is-active'); });
    bar.classList.add('is-active');
  }

  function hide() {
    tooltip.hidden = true;
    svg.querySelectorAll('.chart-bar.is-active').forEach(function (el) { el.classList.remove('is-active'); });
  }

  svg.querySelectorAll('.chart-bar').forEach(function (bar) {
    bar.addEventListener('mouseenter', function () { show(bar); });
    bar.addEventListener('focus', function () { show(bar); });
    bar.addEventListener('blur', hide);
  });
  svg.addEventListener('mouseleave', hide);
})();
