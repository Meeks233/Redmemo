// Shared "bring a block's top back into view" helper.
//
// Used by the disclosure folds (expandAnchor.js) when a section is collapsed and
// by the gallery navigator (galleryNav.js) when stepping between images, so a
// user who has scrolled to the bottom of a tall block is not left stranded.
//
// It mirrors scrollIntoView({block:'nearest'}) — it scrolls ONLY when the block
// has floated above the viewport — while honouring the sticky-navbar offset
// (scroll-padding-top on html.fixed_navbar: 50px desktop / 100px mobile, absent
// when the navbar is not fixed) so the block lands just below the bar.
(function () {
  'use strict';

  function headerOffset() {
    var v = getComputedStyle(document.documentElement).scrollPaddingTop;
    var n = parseFloat(v);
    return isNaN(n) ? 0 : n;
  }

  window.RedMemo = window.RedMemo || {};

  // Scroll the block's top to the top of the viewport (below the sticky bar),
  // but only when it sits above that line. Smooth unless the user prefers
  // reduced motion. No-op when the block is already in view or missing.
  //
  // Scroll to an ABSOLUTE document position rather than a relative scrollBy:
  // (scrollY + rect.top) is the block's invariant document coordinate, so this
  // is idempotent. A relative scrollBy issued while a previous smooth scroll is
  // still animating compounds — rect.top is read mid-flight and a second delta
  // stacks on top — which overshot the media box upward toward the post top on a
  // quick second click. scrollTo(absolute) simply re-targets the same spot.
  window.RedMemo.revealTop = function (block) {
    if (!block) return;
    var top = block.getBoundingClientRect().top;
    var off = headerOffset();
    if (top >= off) return;
    var target = window.scrollY + top - off;
    var smooth = !matchMedia('(prefers-reduced-motion: reduce)').matches;
    window.scrollTo({ top: target, left: window.scrollX, behavior: smooth ? 'smooth' : 'auto' });
  };

  // Put a block's geometric centre at the viewport centre, sizing to whatever the
  // block actually measures right now (every gallery image is a different shape).
  // Used by the gallery navigator so paging always lands the current media dead
  // centre regardless of where you had scrolled.
  //
  // Unconditional and absolute on purpose:
  //  - Absolute target (scrollY + centre) is idempotent — re-issuing it while a
  //    previous smooth scroll is still running converges on the same spot instead
  //    of compounding (the old relative scrollBy overshot on a quick 2nd click).
  //  - Always re-centring (rather than a conditional "only if off-screen") leaves
  //    no gap for the browser's native focus-scroll of the clicked nav button to
  //    fight over, which was yanking the view on the second click.
  window.RedMemo.centerInView = function (block) {
    if (!block) return;
    var rect = block.getBoundingClientRect();
    if (!rect.height) return;
    var center = window.scrollY + rect.top + rect.height / 2; // doc-space centre
    var target = center - window.innerHeight / 2;
    if (target < 0) target = 0;
    var smooth = !matchMedia('(prefers-reduced-motion: reduce)').matches;
    window.scrollTo({ top: target, left: window.scrollX, behavior: smooth ? 'smooth' : 'auto' });
  };
})();
