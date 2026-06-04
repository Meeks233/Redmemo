// NPPicker enhances the Natural-Prefetch field. The merged textarea carries a
// single '+'-separated stream where each clause is either a bare subreddit
// name ("golang") or a per-sub override clause ("cats=sort:rising&time:day").
// Clicking a suggestion appends "+name" as a bare clause; the user can then
// hand-edit any clause to attach an override. Suggestions are sourced from the
// locally-known subs (window._allSubs, seeded from the DB sub tables); a sub
// not found locally can be probed upstream once via /api/probe-sub.
//
// The Go backend re-validates and normalizes whatever is submitted (splitting
// the unified stream back into prefetch_subs and prefetch_sub_modes, dropping
// dead/invalid subs), then echoes the canonical merged form back on reload, so
// the parsing here only needs to be good enough to drive the suggestion UI.
function NPPicker(opts) {
	var inputEl = document.getElementById(opts.inputId);
	var listEl = document.getElementById(opts.listId);
	var onChange = opts.onChange || function () {};

	if (!inputEl || !listEl) return null;

	// clauseSub strips any "=k:v..." override tail and the optional r/ prefix,
	// returning the bare lowercase sub name (or '' if the clause is empty).
	function clauseSub(raw) {
		var eq = raw.indexOf('=');
		if (eq >= 0) raw = raw.substring(0, eq);
		return raw.replace(/^\/?r\//i, '').trim().toLowerCase();
	}

	function normalizeSub(raw) {
		return raw.replace(/^\/?r\//i, '').trim().toLowerCase();
	}

	function isKnownSub(n) {
		for (var i = 0; i < window._allSubs.length; i++) {
			if (window._allSubs[i].name.toLowerCase() === n) return true;
		}
		return false;
	}

	// trailingPartial is the fragment after the last '+' the caret is on. Any
	// non-empty bare clause (no '=') drives the suggestion filter so the list
	// keeps narrowing as the user types — even when the typed text already
	// matches a known sub, since they may still be extending it toward a longer
	// name (e.g. "cats" → "catsstandingup"). A clause containing '=' is an
	// override being typed; treat it as finished and revert to top picks rather
	// than autocompleting the override grammar.
	function trailingPartial(value) {
		if (value === undefined) value = inputEl.value;
		var parts = value.split('+');
		var last = parts[parts.length - 1];
		if (last.indexOf('=') >= 0) return '';
		return normalizeSub(last);
	}

	// effectiveValue returns just the user-typed portion of the textarea, stripping
	// a trailing ghost completion (a tail-anchored selection inserted by
	// inlineComplete). The suggestion list and includedSet must reason about what
	// the user actually typed, not the speculative completion, or filtering by the
	// completed name would always collapse to zero results.
	function effectiveValue() {
		var start = inputEl.selectionStart, end = inputEl.selectionEnd;
		if (start !== null && start < end && end === inputEl.value.length) {
			return inputEl.value.substring(0, start);
		}
		return inputEl.value;
	}

	// includedSet collects the sub names already committed to the value — both
	// bare clauses and override clauses (which we reduce to their sub name) —
	// so they're hidden from the suggestion list. A still-being-typed trailing
	// bare partial doesn't count yet.
	function includedSet(value) {
		if (value === undefined) value = inputEl.value;
		var set = {};
		var parts = value.split('+');
		for (var i = 0; i < parts.length; i++) {
			var hasEq = parts[i].indexOf('=') >= 0;
			var n = clauseSub(parts[i]);
			if (!hasEq && i === parts.length - 1 && n !== '' && !isKnownSub(n)) continue;
			if (n) set[n] = true;
		}
		return set;
	}

	// addSub merges name into the unified value. A bare partial being typed at
	// the tail is completed in place; otherwise name is appended as a new bare
	// "+name" token. Existing override clauses are preserved verbatim — the
	// picker only ever adds a bare entry, the user can hand-attach overrides.
	function addSub(name) {
		var parts = effectiveValue().split('+');
		var lastRaw = parts[parts.length - 1];
		var lastHasEq = lastRaw.indexOf('=') >= 0;
		var last = normalizeSub(lastRaw);
		if (!lastHasEq && last !== '' && !isKnownSub(last)) {
			parts[parts.length - 1] = name; // complete the bare partial
		} else if (last === '') {
			parts[parts.length - 1] = name; // trailing '+' or empty box
		} else {
			parts.push(name); // trailing token is a finished clause → append another
		}
		inputEl.value = parts.filter(function (p) { return p.trim() !== ''; }).join('+');
		onChange(inputEl.value);
		inputEl.focus();
		renderList();
	}

	function renderList() {
		var typed = effectiveValue();
		var q = trailingPartial(typed);
		var included = includedSet(typed);
		var source = q
			? window._allSubs.filter(function (s) { return s.name.toLowerCase().indexOf(q) !== -1; })
			: window._topSubs;
		source = source.filter(function (s) { return included[s.name.toLowerCase()] !== true; });
		if (!q) source = source.slice(0, 10);
		// Build via DOM nodes; sub names and user-typed `q` flow through textContent /
		// dataset, never through innerHTML, so a name like '<img onerror=...>' can't
		// execute and a typed query with HTML metacharacters stays inert.
		listEl.textContent = '';
		if (source.length === 0 && q) {
			if (!isKnownSub(q)) {
				var btn = document.createElement('button');
				btn.type = 'button';
				btn.className = 'sub-probe-btn';
				btn.dataset.query = q;
				btn.textContent = 'Try';
				btn.onclick = function () { probeSub(btn.dataset.query); };
				listEl.appendChild(btn);
				var empty1 = document.createElement('div');
				empty1.className = 'sub-picker-empty';
				empty1.textContent = 'Not archived locally';
				listEl.appendChild(empty1);
			} else {
				var empty2 = document.createElement('div');
				empty2.className = 'sub-picker-empty';
				empty2.textContent = 'Already added';
				listEl.appendChild(empty2);
			}
			return;
		}
		source.forEach(function (s) {
			var item = document.createElement('div');
			item.className = 'sub-picker-item';
			item.dataset.name = s.name;
			var label = document.createElement('span');
			label.textContent = 'r/' + s.name;
			item.appendChild(label);
			if (s.posts > 0) {
				var posts = document.createElement('span');
				posts.className = 'sub-picker-posts';
				posts.textContent = s.posts + ' posts';
				item.appendChild(posts);
			}
			item.onclick = function () { addSub(item.dataset.name); };
			listEl.appendChild(item);
		});
	}

	function probeSub(name) {
		var btn = listEl.querySelector('.sub-probe-btn');
		if (btn) { btn.disabled = true; btn.textContent = 'Probing...'; }
		fetch('/api/probe-sub?name=' + encodeURIComponent(name))
			.then(function (r) { return r.json(); })
			.then(function (d) {
				if (d.exists) {
					if (!isKnownSub(d.name.toLowerCase())) window._allSubs.push({ name: d.name, posts: 0 });
					addSub(d.name);
				} else {
					listEl.innerHTML = '<div class="sub-picker-empty">Subreddit not found on Reddit</div>';
				}
			})
			.catch(function () {
				listEl.innerHTML = '<div class="sub-picker-empty">Probe failed</div>';
			});
	}

	// bestInlineMatch picks the highest-post-count sub whose name starts with the
	// trailing partial but isn't already committed, so the suggestion the user is
	// most likely to want is the one offered as a ghost completion.
	function bestInlineMatch(q, included) {
		var match = null, bestPosts = -1;
		for (var i = 0; i < window._allSubs.length; i++) {
			var s = window._allSubs[i];
			var nm = s.name.toLowerCase();
			if (nm.length > q.length && nm.indexOf(q) === 0 && !included[nm] && s.posts > bestPosts) {
				match = s.name;
				bestPosts = s.posts;
			}
		}
		return match;
	}

	// Track whether the last edit was a delete so we don't immediately re-fill
	// what the user just removed; Backspace/Delete should dismiss the ghost.
	var lastEditWasDelete = false;
	inputEl.addEventListener('beforeinput', function (e) {
		lastEditWasDelete = !!(e.inputType && e.inputType.indexOf('delete') === 0);
	});

	// inlineComplete appends the missing suffix of the best match to the textarea
	// and selects it. Subsequent typing replaces the selection by default (so the
	// user can keep narrowing the partial), Backspace clears it, and Tab/End/→
	// accepts it. Only runs when the caret sits at the very end and nothing is
	// already selected — anything else and the user is mid-edit, leave them be.
	function inlineComplete() {
		if (lastEditWasDelete) return;
		var caret = inputEl.selectionEnd;
		if (caret !== inputEl.value.length) return;
		if (inputEl.selectionStart !== caret) return;
		var q = trailingPartial();
		if (!q) return;
		var match = bestInlineMatch(q, includedSet());
		if (!match) return;
		var anchor = inputEl.value.length;
		inputEl.value = inputEl.value + match.substring(q.length);
		inputEl.setSelectionRange(anchor, inputEl.value.length);
	}

	inputEl.addEventListener('input', function () {
		inlineComplete();
		renderList();
	});
	// Notify on blur and on each pick. The settings page uses this only to flag the
	// form dirty (Save bar) — the field itself is part of the form and persisted on
	// Save, so onChange no longer writes anything on its own.
	inputEl.addEventListener('change', function () { onChange(inputEl.value); });
	renderList();

	return { addSub: addSub, render: renderList };
}
