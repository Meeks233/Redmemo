// Keeps the viewport oriented when a disclosure (show more / show less) toggles.
//
// Two surfaces share this behaviour: the listing-card text expander
// (.post_expand_label toggling a hidden checkbox, see postPreview) and the
// native comment fold (<details class="comment_right">). Both let you collapse a
// tall block, and the painful case is identical: you have scrolled to the bottom
// of a long post/comment, tap the toggle, the block folds up — but the browser
// leaves scrollY untouched, so the content below floats up and you are stranded
// in the whitespace the block used to occupy, with no idea where you are.
//
// This is the well-worn disclosure pattern (WAI-ARIA "return to the trigger on
// hide"; GitHub's PR-review collapse, written up by Ben Nadel):
//
//   - Expand ("show more"): reveal in place. The body grows downward, so doing
//     nothing keeps your reading position — that is the wanted default.
//   - Collapse ("show less"): if the collapsed block's top has floated above the
//     viewport, scroll it back to the top edge so the remnant (and the toggle to
//     undo it) stays on screen. If the block is already in view, leave it.
//
// Progressive enhancement: without JS the toggles still work, they just drift.
//
// The actual scroll is delegated to RedMemo.revealTop (scrollReveal.js): bring
// the collapsed block's top back into view, but only when it has floated above
// the viewport. The block's top does not move on collapse (content above it is
// untouched), so revealTop measures exactly how far above the fold the user was
// stranded.
(function () {
  'use strict';

  function revealOnCollapse(block) {
    if (block && window.RedMemo) window.RedMemo.revealTop(block);
  }

  document.addEventListener('click', function (e) {
    // Real links/buttons handle their own click (e.g. author links inside a
    // comment summary) — never treat those as a fold toggle.
    if (e.target.closest('a, button')) return;

    // Listing-card text expander.
    var label = e.target.closest('.post_expand_label');
    if (label) {
      var wrap = label.closest('.post_body_wrap');
      var checkbox = wrap && wrap.querySelector('.post_expand_toggle');
      var post = label.closest('.post');
      // The label's "for" flips the checkbox as the click's default action;
      // read the resulting state next frame. checked === expanded.
      requestAnimationFrame(function () {
        if (!checkbox || checkbox.checked) return; // expand → leave in place
        if (post) revealOnCollapse(post);          // collapse → reveal the post
      });
      return;
    }

    // Native comment fold.
    var summary = e.target.closest('.comment_right > summary');
    if (summary) {
      var details = summary.parentElement;
      requestAnimationFrame(function () {
        if (details.open) return;        // expand → leave in place
        revealOnCollapse(details);       // collapse → reveal the comment
      });
    }
  });
})();
