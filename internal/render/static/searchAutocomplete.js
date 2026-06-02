// Inline ghost-text autocomplete for the navbar search box. Drives a small
// KV grammar so users don't have to remember the full vocabulary: type one
// letter, the rest of the key appears as muted ghost text; Tab or → accepts
// the suggestion. After a key is committed (e.g. `rating:`), value enums for
// that key start guiding too (rating: → nsfw / sfw).
(function () {
    var KEYS = [
        'sub', 'rating', 'type', 'author', 'flair', 'sort',
        'mode', 'date', 'after', 'before',
        'score', 'ups', 'comments', 'cached'
    ];
    // Per-key value enums. Only the keys with a closed set live here; free-text
    // keys (sub, author, flair, score numbers, dates) get no value suggestion.
    var VALUES = {
        rating: ['nsfw', 'sfw'],
        type: ['image', 'video', 'gif'],
        sort: ['hot', 'new', 'top', 'rising', 'controversial', 'relevance'],
        mode: ['raw', 'full'],
        date: ['today', 'week', 'month', 'year']
    };

    var input = document.getElementById('search');
    var field = document.getElementById('search_field');
    if (!input || !field) return;

    // The ghost overlay sits behind the input. We render the typed prefix in
    // transparent so spacing matches exactly, then the suggested suffix in a
    // muted color so it shows through the (transparent-background) input.
    var ghost = document.createElement('div');
    ghost.id = 'search_ghost';
    ghost.setAttribute('aria-hidden', 'true');
    var prefixSpan = document.createElement('span');
    prefixSpan.className = 'sg-prefix';
    var suffixSpan = document.createElement('span');
    suffixSpan.className = 'sg-suffix';
    ghost.appendChild(prefixSpan);
    ghost.appendChild(suffixSpan);
    field.appendChild(ghost);

    // Suggestion state: the suffix string that will be inserted if the user
    // accepts (Tab / →). Empty when no suggestion applies.
    var pendingSuffix = '';

    // Find the longest-prefix completion of `partial` from `pool`, returning
    // the suffix (the chars that would be appended). Empty when nothing
    // matches or `partial` is already a full word.
    function completeFrom(pool, partial) {
        if (!partial) return '';
        var low = partial.toLowerCase();
        for (var i = 0; i < pool.length; i++) {
            var w = pool[i];
            if (w.length > low.length && w.indexOf(low) === 0) {
                return w.slice(low.length);
            }
        }
        return '';
    }

    // The current "token" is everything after the last whitespace up to the
    // cursor. We only suggest when the caret is at the end of the input — a
    // mid-edit suggestion would otherwise insert into the wrong spot.
    function computeSuggestion() {
        var v = input.value;
        var caret = input.selectionStart;
        if (caret !== v.length) return '';
        // Find token start.
        var i = v.length;
        while (i > 0 && v.charAt(i - 1) !== ' ') i--;
        var token = v.slice(i);
        if (!token) return '';

        // Value completion: token has a key prefix matching `<key>:`.
        var colon = token.indexOf(':');
        if (colon > 0) {
            var key = token.slice(0, colon).toLowerCase();
            var val = token.slice(colon + 1);
            var pool = VALUES[key];
            if (!pool) return '';
            return completeFrom(pool, val);
        }

        // Key completion: token is a key-prefix, append `:` so the user can
        // keep typing values immediately.
        var suf = completeFrom(KEYS, token);
        return suf ? suf + ':' : '';
    }

    function render() {
        prefixSpan.textContent = input.value;
        suffixSpan.textContent = pendingSuffix;
    }

    function refresh() {
        pendingSuffix = computeSuggestion();
        render();
    }

    function accept() {
        if (!pendingSuffix) return false;
        var pos = input.value.length;
        input.value = input.value + pendingSuffix;
        // Place caret at end so the next keystroke continues the new token.
        try { input.setSelectionRange(pos + pendingSuffix.length, pos + pendingSuffix.length); } catch (e) {}
        // Recompute — accepting a key (`rating:`) immediately exposes the
        // value enum (nsfw / sfw) as the next ghost.
        refresh();
        return true;
    }

    input.addEventListener('input', refresh);
    input.addEventListener('keyup', function (e) {
        // Arrow keys / caret moves change the caret position without firing
        // `input`; recompute so suggestions only show when the caret is at
        // the very end.
        if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
            e.key === 'ArrowUp' || e.key === 'ArrowDown' ||
            e.key === 'Home' || e.key === 'End') {
            refresh();
        }
    });
    input.addEventListener('keydown', function (e) {
        if (e.key === 'Tab' && !e.shiftKey && pendingSuffix) {
            // Tab is the accept key. Block focus-leave so the user stays on
            // the input and can keep building the query.
            e.preventDefault();
            accept();
            return;
        }
        if (e.key === 'ArrowRight' && pendingSuffix) {
            // Only accept on → when the caret is at the end (otherwise → is
            // a normal caret move).
            if (input.selectionStart === input.value.length) {
                e.preventDefault();
                accept();
            }
            return;
        }
        if (e.key === 'Escape' && pendingSuffix) {
            // Escape hides the current ghost — useful when the suggestion
            // is in the way of a free-text query.
            pendingSuffix = '';
            render();
        }
    });
    input.addEventListener('blur', function () {
        // Hide the ghost when focus leaves so it doesn't show a stale guess
        // over an unrelated state.
        pendingSuffix = '';
        render();
    });
    input.addEventListener('focus', refresh);

    refresh();
})();
