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
 *
 * Beyond the explicit triggers, this script also installs a single delegated
 * click handler that intercepts ANY anchor whose resolved host is reddit.com /
 * redd.it — so every off-site Reddit link on the page (post bodies, comments,
 * sidebars, …) goes through the same second confirmation without each call site
 * having to opt in. The host check uses the browser's URL parser so a
 * look-alike host ("reddit.com.evil.test") is never mistaken for Reddit.
 */
(function() {
    var modal = document.getElementById('reddit-modal');
    if (!modal) return;

    var titleEl   = modal.querySelector('h2');
    var urlEl     = modal.querySelector('.modal-url');
    var confirmEl = modal.querySelector('.modal-confirm');

    var defaultTitle   = titleEl   ? titleEl.textContent   : '';
    var defaultConfirm = confirmEl ? confirmEl.textContent : '';

    // isRedditHost mirrors the backend isRedditSiteHost/redd.it allowlist: the
    // reddit.com site plus the redd.it short-link/media domains. Comparison is
    // case-insensitive and exact-suffix so subdomains match but look-alikes
    // ("notreddit.com", "reddit.com.evil.test") do not.
    function isRedditHost(host) {
        host = (host || '').toLowerCase();
        return host === 'reddit.com' || host.slice(-11) === '.reddit.com' ||
               host === 'redd.it'    || host.slice(-8)  === '.redd.it';
    }

    // resolveReddit returns the absolute reddit URL an anchor points at, or ''
    // when it is not an off-site Reddit link (relative/local, same-origin, or a
    // foreign host). Same-origin links — including RedMemo's own /r/ paths — are
    // intentionally excluded: they stay on this site and need no warning.
    function resolveReddit(a) {
        var raw = a.getAttribute('href');
        if (!raw) return '';
        var u;
        try { u = new URL(a.href, window.location.href); } catch (_) { return ''; }
        if (u.hostname === window.location.hostname) return '';
        if (!isRedditHost(u.hostname)) return '';
        return u.href;
    }

    function open(href, opts) {
        opts = opts || {};
        // Defend the modal's confirm action: only ever arm it with a validated
        // Reddit URL, never an attacker-supplied data-modal-title/href payload.
        if (href) {
            try {
                if (!isRedditHost(new URL(href, window.location.href).hostname)) href = '';
            } catch (_) { href = ''; }
        }
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

    // Global safety net: catch every off-site Reddit link that did NOT opt in
    // via data-leave-modal. Runs in the bubble phase after the explicit
    // per-trigger listeners above, so when one of those already handled the
    // click (and called preventDefault) we bow out via e.defaultPrevented and
    // never double-open. The modal's own buttons (data-modal-close) are skipped.
    document.addEventListener('click', function(e) {
        if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        var a = e.target.closest ? e.target.closest('a[href]') : null;
        if (!a || a.hasAttribute('data-modal-close')) return;
        var href = resolveReddit(a);
        if (!href) return;
        e.preventDefault();
        open(href, {
            title:   a.getAttribute('data-modal-title'),
            confirm: a.getAttribute('data-modal-confirm'),
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
