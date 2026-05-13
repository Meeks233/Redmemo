function SubPicker(opts) {
	var selectedEl = document.getElementById(opts.selectedId);
	var searchEl = document.getElementById(opts.searchId);
	var listEl = document.getElementById(opts.listId);
	var onChangeCallback = opts.onChange || function(){};

	if (!selectedEl || !searchEl || !listEl) return null;

	function postCount(name) {
		for (var i = 0; i < window._allSubs.length; i++) {
			if (window._allSubs[i].name === name) return window._allSubs[i].posts;
		}
		return 0;
	}

	function makeTag(name) {
		var span = document.createElement('span');
		span.className = 'sub-tag';
		span.dataset.sub = name;
		span.textContent = 'r/' + name;
		var count = postCount(name);
		if (count > 0) {
			var badge = document.createElement('span');
			badge.className = 'sub-tag-count';
			badge.textContent = count;
			span.appendChild(badge);
		}
		var btn = document.createElement('button');
		btn.type = 'button';
		btn.className = 'sub-tag-remove';
		btn.textContent = '×';
		btn.onclick = function() { removeSub(name); };
		span.appendChild(btn);
		return span;
	}

	function getSelected() {
		var tags = selectedEl.querySelectorAll('.sub-tag');
		var names = [];
		tags.forEach(function(t) { names.push(t.dataset.sub); });
		return names;
	}

	function addSub(name) {
		if (getSelected().indexOf(name) !== -1) return;
		selectedEl.appendChild(makeTag(name));
		onChangeCallback(getSelected());
		renderList();
	}

	function removeSub(name) {
		selectedEl.querySelectorAll('.sub-tag').forEach(function(t) {
			if (t.dataset.sub === name) t.remove();
		});
		onChangeCallback(getSelected());
		renderList();
	}

	function refreshTags() {
		var names = getSelected();
		selectedEl.innerHTML = '';
		names.forEach(function(n) { selectedEl.appendChild(makeTag(n)); });
	}

	function renderList() {
		var q = searchEl.value.toLowerCase().trim();
		var sel = getSelected();
		var source = q
			? window._allSubs.filter(function(s) { return s.name.toLowerCase().indexOf(q) !== -1; })
			: window._topSubs;
		source = source.filter(function(s) { return sel.indexOf(s.name) === -1; });
		if (!q) source = source.slice(0, 10);
		var html = '';
		source.forEach(function(s) {
			html += '<div class="sub-picker-item" data-name="' + s.name + '">';
			html += '<span>r/' + s.name + '</span>';
			if (s.posts > 0) html += '<span class="sub-picker-posts">' + s.posts + ' posts</span>';
			html += '</div>';
		});
		if (source.length === 0 && q) {
			var inLocal = window._allSubs.some(function(s) { return s.name.toLowerCase() === q; });
			if (!inLocal) {
				html = '<button type="button" class="sub-probe-btn" data-query="' + q + '">Try</button>';
				html += '<div class="sub-picker-empty">Not archived locally</div>';
			} else {
				html = '<div class="sub-picker-empty">Already selected</div>';
			}
		}
		listEl.innerHTML = html;

		listEl.querySelectorAll('.sub-picker-item').forEach(function(el) {
			el.onclick = function() { addSub(el.dataset.name); };
		});
		var probeBtn = listEl.querySelector('.sub-probe-btn');
		if (probeBtn) {
			probeBtn.onclick = function() { probeSub(probeBtn.dataset.query); };
		}
	}

	function probeSub(name) {
		var btn = listEl.querySelector('.sub-probe-btn');
		if (btn) { btn.disabled = true; btn.textContent = 'Probing...'; }
		fetch('/api/probe-sub?name=' + encodeURIComponent(name))
			.then(function(r) { return r.json(); })
			.then(function(d) {
				if (d.exists) {
					var already = window._allSubs.some(function(s) { return s.name === d.name; });
					if (!already) window._allSubs.push({ name: d.name, posts: 0 });
					addSub(d.name);
					searchEl.value = '';
					renderList();
				} else {
					listEl.innerHTML = '<div class="sub-picker-empty">Subreddit not found on Reddit</div>';
				}
			})
			.catch(function() {
				listEl.innerHTML = '<div class="sub-picker-empty">Probe failed</div>';
			});
	}

	refreshTags();
	searchEl.addEventListener('input', renderList);
	renderList();

	return { getSelected: getSelected, addSub: addSub, removeSub: removeSub, render: renderList };
}
