(function () {
  'use strict';

  var SPINNER = '<div class="gallery_loading"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg></div>';

  function lockDimensions(slider, items, link) {
    if (slider.hasAttribute('data-gallery-locked')) return;
    var w = link.offsetWidth;
    var maxH = 0;
    for (var i = 0; i < items.length; i++) {
      if (items[i].w > 0 && items[i].h > 0) {
        var h = w * items[i].h / items[i].w;
        if (h > maxH) maxH = h;
      }
    }
    if (w > 0) slider.style.minWidth = w + 'px';
    if (maxH > 0) slider.style.minHeight = Math.ceil(maxH) + 'px';
    slider.setAttribute('data-gallery-locked', '1');
  }

  function clearPending(link) {
    var loader = link.querySelector('.gallery_loading');
    if (loader) loader.remove();
  }

  function showImg(slider, link, newImg, old, count) {
    var loader = link.querySelector('.gallery_loading');
    if (loader) loader.remove();
    if (old && old.parentNode) old.remove();
    link.insertBefore(newImg, count);
    slider._galleryShown = newImg;
  }

  function navigate(slider, dir) {
    var items;
    try { items = JSON.parse(slider.getAttribute('data-gallery-urls')); } catch (_) { return; }
    if (!items || items.length < 2) return;

    var prevIdx = parseInt(slider.getAttribute('data-gallery-idx') || '0', 10);
    var idx = prevIdx + dir;
    if (idx < 0 || idx >= items.length) return;
    var myIdx = idx;
    slider.setAttribute('data-gallery-idx', idx);

    var link = slider.querySelector('.gallery_preview');
    if (!link) return;

    lockDimensions(slider, items, link);

    var old = slider._galleryShown && slider._galleryShown.parentNode === link ? slider._galleryShown : null;
    if (!old) {
      old = link.querySelector('img') || link.querySelector('svg:not(.gallery_loading svg)');
      if (!old) old = link.querySelector('svg');
    }
    var count = link.querySelector('.gallery_count');
    var prevCountText = count ? count.textContent : null;
    if (count) count.textContent = (idx + 1) + ' / ' + items.length;

    var prev = slider.querySelector('.gallery_prev');
    var next = slider.querySelector('.gallery_next');
    if (prev) prev.classList.toggle('gallery_nav_hidden', idx <= 0);
    if (next) next.classList.toggle('gallery_nav_hidden', idx >= items.length - 1);

    function isStale() {
      return parseInt(slider.getAttribute('data-gallery-idx') || '0', 10) !== myIdx;
    }

    function rollback() {
      if (isStale()) return;
      slider.setAttribute('data-gallery-idx', prevIdx);
      if (count && prevCountText !== null) count.textContent = prevCountText;
      if (prev) prev.classList.toggle('gallery_nav_hidden', prevIdx <= 0);
      if (next) next.classList.toggle('gallery_nav_hidden', prevIdx >= items.length - 1);
      if (old) old.style.display = '';
    }

    var newImg = document.createElement('img');
    newImg.alt = (old && old.alt) || '';
    newImg.style.width = '100%';
    newImg.style.height = 'auto';
    newImg.src = items[idx].u;

    if (newImg.complete && newImg.naturalWidth > 0) {
      clearPending(link);
      showImg(slider, link, newImg, old, count);
      return;
    }

    if (old) old.style.display = 'none';
    clearPending(link);
    link.insertAdjacentHTML('afterbegin', SPINNER);

    newImg.onload = function () {
      if (isStale()) return;
      showImg(slider, link, newImg, old, count);
    };
    newImg.onerror = function () {
      if (isStale()) return;
      clearPending(link);
      rollback();
    };
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('.gallery_nav');
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();

    var slider = btn.closest('.gallery_slider');
    if (!slider) return;

    navigate(slider, btn.classList.contains('gallery_prev') ? -1 : 1);
  });

  // Touch swipe support for mobile (hover-based nav buttons never appear on touch).
  var touchStartX = 0;
  var touchStartY = 0;
  var touchSlider = null;

  document.addEventListener('touchstart', function (e) {
    if (e.touches.length !== 1) { touchSlider = null; return; }
    var slider = e.target.closest('.gallery_slider');
    if (!slider) { touchSlider = null; return; }
    touchSlider = slider;
    touchStartX = e.touches[0].clientX;
    touchStartY = e.touches[0].clientY;
  }, { passive: true });

  document.addEventListener('touchend', function (e) {
    if (!touchSlider) return;
    var slider = touchSlider;
    touchSlider = null;
    var t = e.changedTouches[0];
    if (!t) return;
    var dx = t.clientX - touchStartX;
    var dy = t.clientY - touchStartY;
    // Require a mostly-horizontal swipe past a threshold.
    if (Math.abs(dx) < 40 || Math.abs(dx) <= Math.abs(dy)) return;
    e.preventDefault();
    navigate(slider, dx < 0 ? 1 : -1);
  }, { passive: false });
})();
