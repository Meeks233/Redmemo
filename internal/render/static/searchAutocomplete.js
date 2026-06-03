// Inline ghost-text autocomplete for query inputs that speak RedMemo's e621-style
// grammar. Drives a small KV vocab so users don't have to memorize keys: type
// one letter, the rest of the key appears as ghost text; Tab or → accepts.
// After a key is committed (e.g. `rating:`), value enums for that key start
// guiding too (rating: → nsfw / sfw).
//
// Two render modes share the same suggestion engine:
//   - "overlay": the navbar single-line <input>, where #search_field provides a
//     positioned wrapper for an absolutely-placed muted ghost <div>.
//     (CSS: #search_ghost in style.css.)
//   - "selection": multi-line / auto-growing <textarea>, where wrapping makes
//     an overlay impractical. The suggestion is appended into the field's own
//     value and held as a native text selection — visually distinct, replaced
//     by the next keystroke, accepted on Tab / →, dropped on Escape.
(function () {
    var KEYS = [
        'sub', 'rating', 'type', 'author', 'flair', 'sort',
        'mode', 'date', 'after', 'before',
        'score', 'ups', 'comments', 'cached'
    ];
    // Per-key value enums. Only keys with a closed set live here; free-text
    // keys (sub, author, flair, score numbers, dates) get no value suggestion.
    var VALUES = {
        rating: ['nsfw', 'sfw'],
        type: ['image', 'video', 'gif'],
        sort: ['hot', 'new', 'top', 'rising', 'controversial', 'relevance'],
        mode: ['raw', 'full'],
        date: ['today', 'week', 'month', 'year']
    };

    // Longest-prefix completion of `partial` from `pool` — returns the suffix
    // to append. Empty when nothing matches or `partial` is already complete.
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

    // Compute the ghost suffix for `value` at `caret`. The current "token" is
    // everything after the last whitespace up to the caret; suggestions only
    // fire when the caret sits at the very end of that token's text (so a
    // mid-edit caret doesn't splice ghost text into the middle of the field).
    function suggestFor(value, caret) {
        if (caret !== value.length) return '';
        var i = value.length;
        while (i > 0 && !/\s/.test(value.charAt(i - 1))) i--;
        var token = value.slice(i);
        if (!token) return '';

        var colon = token.indexOf(':');
        if (colon > 0) {
            var key = token.slice(0, colon).toLowerCase();
            var val = token.slice(colon + 1);
            var pool = VALUES[key];
            if (!pool) return '';
            return completeFrom(pool, val);
        }
        var suf = completeFrom(KEYS, token);
        return suf ? suf + ':' : '';
    }

    // Overlay mode: pinned muted ghost <div> behind the input, prefix in
    // transparent so the suffix appears just past the typed text.
    function attachOverlay(input, field) {
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

        var pendingSuffix = '';

        function render() {
            prefixSpan.textContent = input.value;
            suffixSpan.textContent = pendingSuffix;
        }
        function refresh() {
            pendingSuffix = suggestFor(input.value, input.selectionStart);
            render();
        }
        function accept() {
            if (!pendingSuffix) return false;
            var pos = input.value.length;
            input.value = input.value + pendingSuffix;
            try { input.setSelectionRange(pos + pendingSuffix.length, pos + pendingSuffix.length); } catch (e) {}
            refresh();
            return true;
        }

        input.addEventListener('input', refresh);
        input.addEventListener('keyup', function (e) {
            if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
                e.key === 'ArrowUp' || e.key === 'ArrowDown' ||
                e.key === 'Home' || e.key === 'End') {
                refresh();
            }
        });
        input.addEventListener('keydown', function (e) {
            if (e.key === 'Tab' && !e.shiftKey && pendingSuffix) {
                e.preventDefault();
                accept();
                return;
            }
            if (e.key === 'ArrowRight' && pendingSuffix) {
                if (input.selectionStart === input.value.length) {
                    e.preventDefault();
                    accept();
                }
                return;
            }
            if (e.key === 'Escape' && pendingSuffix) {
                pendingSuffix = '';
                render();
            }
        });
        input.addEventListener('blur', function () {
            pendingSuffix = '';
            render();
        });
        input.addEventListener('focus', refresh);
        refresh();
    }

    // Selection mode: the ghost lives inside the field's own value as a
    // highlighted trailing selection. Works equally for <input> and <textarea>
    // (including the auto-growing/wrapping homepage filter), no overlay CSS
    // needed. `committed` tracks the caret-side length so Backspace/Escape can
    // drop the ghost without nuking real typed characters.
    function attachSelection(el) {
        var committed = el.value.length;
        var ghosting = false;

        function clearGhost() {
            if (!ghosting) return;
            el.value = el.value.slice(0, committed);
            ghosting = false;
            try { el.setSelectionRange(committed, committed); } catch (e) {}
        }

        function refresh() {
            // Treat the field as having only the committed (real) text; the
            // suggestion engine should never see the previous ghost suffix.
            var real = ghosting ? el.value.slice(0, committed) : el.value;
            committed = real.length;
            ghosting = false;
            if (el.value !== real) el.value = real;

            // Only suggest when the caret is at the end of the real text.
            var caret;
            try { caret = el.selectionStart; } catch (e) { caret = real.length; }
            if (caret !== real.length) return;

            var suf = suggestFor(real, real.length);
            if (!suf) return;
            el.value = real + suf;
            ghosting = true;
            try { el.setSelectionRange(real.length, real.length + suf.length); } catch (e) {}
        }

        function accept() {
            if (!ghosting) return false;
            var end = el.value.length;
            committed = end;
            ghosting = false;
            try { el.setSelectionRange(end, end); } catch (e) {}
            // Accepting a key (`rating:`) immediately exposes the value enum.
            refresh();
            return true;
        }

        el.addEventListener('beforeinput', function (e) {
            // The browser would replace just the selection (the ghost) on a
            // normal insert — that's fine. But for deletion keys we want to
            // strip the ghost first and have the delete act on real text.
            if (!ghosting) return;
            if (e.inputType === 'deleteContentBackward' || e.inputType === 'deleteContentForward') {
                e.preventDefault();
                clearGhost();
            }
        });

        el.addEventListener('input', function () {
            // Any input event after a ghost insert means real characters
            // landed (the ghost selection was overwritten); the new value is
            // entirely real, recompute from there.
            ghosting = false;
            committed = el.value.length;
            refresh();
        });

        el.addEventListener('keydown', function (e) {
            if ((e.key === 'Tab' && !e.shiftKey) || e.key === 'ArrowRight') {
                if (ghosting) {
                    // For ArrowRight, only accept when the caret/selection sits
                    // at the very end — otherwise it's a normal caret move.
                    if (e.key === 'ArrowRight' && el.selectionEnd !== el.value.length) return;
                    e.preventDefault();
                    accept();
                }
                return;
            }
            if (e.key === 'Escape' && ghosting) {
                e.preventDefault();
                clearGhost();
            }
        });

        el.addEventListener('blur', clearGhost);
        // Ghost text lives inside el.value as a trailing selection; the form
        // would otherwise submit the suggestion as if the user had typed it.
        // Capture-phase so we strip before the native submission reads values.
        if (el.form) el.form.addEventListener('submit', clearGhost, true);
        el.addEventListener('focus', function () {
            committed = el.value.length;
            ghosting = false;
            refresh();
        });

        // Initial state: no ghost (the field is rendered server-side with the
        // user's saved query; we shouldn't be guessing at load).
        committed = el.value.length;
    }

    var navInput = document.getElementById('search');
    var navField = document.getElementById('search_field');
    if (navInput && navField) attachOverlay(navInput, navField);

    var homepageQuery = document.getElementById('front_page_subs');
    if (homepageQuery) attachSelection(homepageQuery);
})();
