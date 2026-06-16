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
    // Floor by the height already on screen. Galleries whose items carry no
    // intrinsic dimensions yield maxH == 0, which previously left the box
    // unlocked so every navigation resized it — and any swap to a shorter image
    // collapsed content above the fold, jerking the page upward when the
    // viewport sat in the lower half of the image. Anchoring to the current
    // render guarantees the box can never shrink below what is shown now.
    var cur = link.offsetHeight;
    if (cur > maxH) maxH = cur;
    if (w > 0) slider.style.minWidth = w + 'px';
    if (maxH > 0) slider.style.minHeight = Math.ceil(maxH) + 'px';
    slider.setAttribute('data-gallery-locked', '1');
  }

  // Raise the locked floor to the tallest image shown so far. With no intrinsic
  // dimensions the initial lock only knows the first image's height; bumping on
  // each load keeps later (taller) images from ever shrinking the box on a
  // subsequent navigation.
  function bumpLock(slider, el) {
    var h = el.offsetHeight;
    if (h <= 0) return;
    var cur = parseFloat(slider.style.minHeight) || 0;
    if (h > cur) slider.style.minHeight = Math.ceil(h) + 'px';
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
    bumpLock(slider, newImg);
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

    var item = items[idx];
    var newImg = document.createElement('img');
    newImg.alt = (old && old.alt) || '';
    newImg.style.width = '100%';
    newImg.style.height = 'auto';
    // Intrinsic width/height attributes hand the browser the aspect ratio, so
    // the <img> reserves its final height the instant it is inserted — before
    // the pixels arrive. This is the standard layout-shift (CLS) fix: reserve
    // space via aspect ratio, then let CSS (width:100%/height:auto) size it.
    if (item.w > 0 && item.h > 0) { newImg.width = item.w; newImg.height = item.h; }
    newImg.src = item.u;

    if (newImg.complete && newImg.naturalWidth > 0) {
      clearPending(link);
      showImg(slider, link, newImg, old, count);
      return;
    }

    // Reserve the incoming image's space up-front so the box is already at its
    // final height while the spinner shows: no collapse to the tiny spinner svg,
    // and no downward lurch when the image paints (the old "shrink then extend"
    // bug). Known dimensions give the exact height; otherwise we floor at the
    // outgoing image's height so it at least never collapses.
    var reserve = 0;
    if (item.w > 0 && item.h > 0 && link.offsetWidth > 0) {
      reserve = link.offsetWidth * item.h / item.w;
    } else if (old) {
      reserve = old.offsetHeight;
    }
    if (reserve > 0) {
      var curMin = parseFloat(slider.style.minHeight) || 0;
      if (reserve > curMin) slider.style.minHeight = Math.ceil(reserve) + 'px';
    }

    if (old) old.style.display = 'none';
    clearPending(link);
    link.insertAdjacentHTML('afterbegin', SPINNER);
    // Hold the spinner at the reserved height so the placeholder occupies the
    // same box the image will, instead of collapsing to the 24px svg.
    var loader = link.querySelector('.gallery_loading');
    if (loader && reserve > 0) loader.style.height = Math.ceil(reserve) + 'px';

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

    // Turn the "stuck below a tall image" annoyance into a feature: every step
    // re-centres the current media (the .gallery_slider box, which vertically
    // centres the image) in the viewport. Centring on the actual media — not a
    // top-reveal — is deterministic and idempotent, so the first and second
    // clicks behave identically, and it keeps the nav buttons (pinned at the
    // slider's vertical centre) dead-centre on screen, leaving no gap for the
    // browser's native focus-scroll to fight over.
    if (window.RedMemo) window.RedMemo.centerInView(slider);
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
