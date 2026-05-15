/* Reddit off-site warning modal — global handler.
 *
 * Each page that wants to ask "are you sure you want to leave?" before
 * sending the user to reddit.com renders the shared markup (via the
 * `visit_reddit_confirmation` template) once, plus one or more trigger
 * elements. Triggers opt in by setting `data-leave-modal` (any value);
 * close elements opt in by setting `data-modal-close`.
 *
 * Trigger anchors carry the real reddit URL as their href so that:
 *   (a) users with JavaScript disabled get the natural navigation, and
 *   (b) this script lifts the href into the modal's confirm button and
 *       intercepts the click, avoiding inline onclick="..." everywhere.
 *
 * The trigger may carry data-modal-title / data-modal-confirm to override
 * the modal heading and confirm-button text per-action (e.g. "Access
 * Reddit directly" vs "Yes, take me to Reddit").
 */
(function() {
    var modal = document.getElementById('reddit-modal');
    if (!modal) return;

    var titleEl   = modal.querySelector('h2');
    var urlEl     = modal.querySelector('.modal-url');
    var confirmEl = modal.querySelector('.modal-confirm');

    var defaultTitle   = titleEl   ? titleEl.textContent   : '';
    var defaultConfirm = confirmEl ? confirmEl.textContent : '';

    function open(href, opts) {
        opts = opts || {};
        if (href && urlEl)     urlEl.textContent     = href;
        if (href && confirmEl) confirmEl.setAttribute('href', href);
        if (titleEl)   titleEl.textContent   = opts.title   || defaultTitle;
        if (confirmEl) confirmEl.textContent = opts.confirm || defaultConfirm;
        modal.classList.add('visible');
    }

    function close() {
        modal.classList.remove('visible');
    }

    document.querySelectorAll('[data-leave-modal]').forEach(function(el) {
        el.addEventListener('click', function(e) {
            e.preventDefault();
            open(el.getAttribute('href'), {
                title:   el.getAttribute('data-modal-title'),
                confirm: el.getAttribute('data-modal-confirm'),
            });
        });
    });

    document.querySelectorAll('[data-modal-close]').forEach(function(el) {
        el.addEventListener('click', function(e) {
            // Confirm button (the <a> going to reddit) closes the modal but
            // still navigates — don't preventDefault on it.
            if (!el.classList.contains('modal-confirm')) {
                e.preventDefault();
            }
            close();
        });
    });

    // Backdrop click and Escape key both close.
    modal.addEventListener('click', function(e) {
        if (e.target === modal) close();
    });
    document.addEventListener('keydown', function(e) {
        if (e.key === 'Escape' && modal.classList.contains('visible')) close();
    });
})();
