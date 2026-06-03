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

	// trailingPartial is the fragment after the last '+' the caret is on, but only
	// when it's an incomplete BARE name. A clause containing '=' is an override
	// being typed — treat it as finished so the suggestion list reverts to top
	// picks instead of trying to autocomplete the override grammar.
	function trailingPartial() {
		var parts = inputEl.value.split('+');
		var last = parts[parts.length - 1];
		if (last.indexOf('=') >= 0) return '';
		var n = normalizeSub(last);
		return (n !== '' && !isKnownSub(n)) ? n : '';
	}

	// includedSet collects the sub names already committed to the value — both
	// bare clauses and override clauses (which we reduce to their sub name) —
	// so they're hidden from the suggestion list. A still-being-typed trailing
	// bare partial doesn't count yet.
	function includedSet() {
		var set = {};
		var parts = inputEl.value.split('+');
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
		var parts = inputEl.value.split('+');
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
		var q = trailingPartial();
		var included = includedSet();
		var source = q
			? window._allSubs.filter(function (s) { return s.name.toLowerCase().indexOf(q) !== -1; })
			: window._topSubs;
		source = source.filter(function (s) { return included[s.name.toLowerCase()] !== true; });
		if (!q) source = source.slice(0, 10);
		var html = '';
		source.forEach(function (s) {
			html += '<div class="sub-picker-item" data-name="' + s.name + '">';
			html += '<span>r/' + s.name + '</span>';
			if (s.posts > 0) html += '<span class="sub-picker-posts">' + s.posts + ' posts</span>';
			html += '</div>';
		});
		if (source.length === 0 && q) {
			if (!isKnownSub(q)) {
				html = '<button type="button" class="sub-probe-btn" data-query="' + q + '">Try</button>';
				html += '<div class="sub-picker-empty">Not archived locally</div>';
			} else {
				html = '<div class="sub-picker-empty">Already added</div>';
			}
		}
		listEl.innerHTML = html;

		listEl.querySelectorAll('.sub-picker-item').forEach(function (el) {
			el.onclick = function () { addSub(el.dataset.name); };
		});
		var probeBtn = listEl.querySelector('.sub-probe-btn');
		if (probeBtn) {
			probeBtn.onclick = function () { probeSub(probeBtn.dataset.query); };
		}
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

	inputEl.addEventListener('input', renderList);
	// Notify on blur and on each pick. The settings page uses this only to flag the
	// form dirty (Save bar) — the field itself is part of the form and persisted on
	// Save, so onChange no longer writes anything on its own.
	inputEl.addEventListener('change', function () { onChange(inputEl.value); });
	renderList();

	return { addSub: addSub, render: renderList };
}
