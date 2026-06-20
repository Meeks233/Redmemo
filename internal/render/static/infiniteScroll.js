(function(){
	var posts = document.getElementById('posts');
	var spinner = document.getElementById('load-spinner');
	var endMsg = document.getElementById('load-end');
	var loader = document.getElementById('infinite-loader');
	if (!loader || !posts) return;

	var offset = parseInt(loader.dataset.offset, 10) || 0;
	var step = parseInt(loader.dataset.offsetStep, 10) || 5;
	var endpoint = loader.dataset.endpoint || '/';
	var extraQS = loader.dataset.qs || '';
	var sort = loader.dataset.sort || '';
	var interval = (parseInt(loader.dataset.interval, 10) || 2) * 1000;
	var loading = false;
	var done = false;
	var gen = 0;
	var pending429 = null;

	// --- View restoration -------------------------------------------------
	// Clicking a post is a full-page navigation; pressing Back would otherwise
	// reload the listing fresh, discarding every infinitely-scrolled post and
	// resetting the scroll position. Two layers keep the view intact:
	//   1. The browser back/forward cache (bfcache) restores the live page as-is
	//      when eligible — nothing for us to do (the script does not re-run).
	//   2. When bfcache misses, we re-render from a sessionStorage snapshot that
	//      we write on pagehide. Restoring from storage also spares the backend
	//      the work of re-fetching every page the user had already scrolled.
	var SNAP_TTL = 30 * 60 * 1000; // 30 min — stale snapshots are ignored.

	function snapKey() {
		return 'rm:scroll:' + location.pathname + location.search;
	}

	function navType() {
		try {
			var nav = performance.getEntriesByType('navigation')[0];
			if (nav && nav.type) return nav.type; // navigate | reload | back_forward | prerender
		} catch (e) {}
		// Legacy fallback (deprecated performance.navigation).
		if (performance.navigation) {
			if (performance.navigation.type === 2) return 'back_forward';
			if (performance.navigation.type === 1) return 'reload';
		}
		return 'navigate';
	}

	function saveSnapshot() {
		try {
			sessionStorage.setItem(snapKey(), JSON.stringify({
				html: posts.innerHTML,
				offset: offset,
				sort: sort,
				done: done,
				scrollY: window.scrollY || window.pageYOffset || 0,
				ts: Date.now()
			}));
		} catch (e) {}
	}

	function restoreScroll(y) {
		if (!y) return;
		// Media is lazy-loaded, so document height grows after restore; keep
		// re-applying the target until it sticks (or content can't reach it).
		var attempts = 0;
		function set() {
			window.scrollTo(0, y);
			attempts++;
			if (Math.abs((window.scrollY || window.pageYOffset || 0) - y) > 2 && attempts < 40) {
				window.requestAnimationFrame(set);
			}
		}
		set();
		window.addEventListener('load', function () { window.scrollTo(0, y); }, { once: true });
	}

	function tryRestore() {
		if (navType() !== 'back_forward') return false;
		var raw;
		try { raw = sessionStorage.getItem(snapKey()); } catch (e) { return false; }
		if (!raw) return false;
		var snap;
		try { snap = JSON.parse(raw); } catch (e) { return false; }
		if (!snap || !snap.html || (Date.now() - (snap.ts || 0)) > SNAP_TTL) return false;
		posts.innerHTML = snap.html;
		if (typeof snap.offset === 'number') offset = snap.offset;
		if (snap.sort) sort = snap.sort;
		done = !!snap.done;
		if (done && endMsg) endMsg.style.display = '';
		restoreScroll(snap.scrollY || 0);
		return true;
	}

	// Let us own scroll position on back/forward; the browser's own guess fights
	// the snapshot restore on a page whose height changes after re-render.
	if ('scrollRestoration' in history) history.scrollRestoration = 'manual';
	window.addEventListener('pagehide', saveSnapshot);
	// Safari may not fire pagehide reliably; persist when the tab is hidden too.
	document.addEventListener('visibilitychange', function () {
		if (document.visibilityState === 'hidden') saveSnapshot();
	});

	tryRestore();
	// ---------------------------------------------------------------------

	function buildURL(off, sortOverride) {
		var parts = [];
		if (extraQS) parts.push(extraQS);
		var s = sortOverride !== undefined ? sortOverride : sort;
		if (s) parts.push('sort=' + encodeURIComponent(s));
		parts.push('offset=' + off);
		parts.push('partial=1');
		return endpoint + '?' + parts.join('&');
	}

	function doFetch(url, cb) {
		var myGen = ++gen;
		loading = true;
		spinner.style.display = '';
		fetch(url)
			.then(function(r) {
				if (myGen !== gen) return null;
				if (r.status === 429) {
					pending429 = setTimeout(function() { loading = false; doFetch(url, cb); }, interval);
					return null;
				}
				if (!r.ok) {
					spinner.style.display = 'none';
					loading = false;
					return null;
				}
				return r.text();
			})
			.then(function(html) {
				if (myGen !== gen) return;
				if (html === null) return;
				spinner.style.display = 'none';
				cb(html);
			})
			.catch(function() {
				if (myGen !== gen) return;
				spinner.style.display = 'none';
				loading = false;
			});
	}

	function loadMore() {
		if (loading || done) return;
		doFetch(buildURL(offset), function(html) {
			if (!html.trim()) {
				done = true;
				endMsg.style.display = '';
				loading = false;
				return;
			}
			var tmp = document.createElement('div');
			tmp.innerHTML = '<hr class="sep" />' + html;
			while (tmp.firstChild) posts.appendChild(tmp.firstChild);
			offset += step;
			loading = false;
		});
	}

	window.switchSort = function(el, newSort) {
		if (sort === newSort) return false;
		gen++;
		if (pending429) { clearTimeout(pending429); pending429 = null; }
		sort = newSort;
		offset = 0;
		done = false;
		posts.innerHTML = '';
		endMsg.style.display = 'none';
		var links = document.querySelectorAll('#sort_options a');
		for (var i = 0; i < links.length; i++) links[i].classList.remove('selected');
		el.classList.add('selected');
		history.replaceState(null, '', endpoint + '?sort=' + newSort);
		doFetch(buildURL(0, newSort), function(html) {
			spinner.style.display = 'none';
			if (!html.trim()) {
				done = true;
				endMsg.style.display = '';
				loading = false;
				return;
			}
			posts.innerHTML = html;
			offset = step;
			loading = false;
		});
		return false;
	};

	// Throttle scroll handling to one rAF; document.body.offsetHeight forces
	// layout, so calling it on every raw scroll tick on a long, infinitely-
	// scrolled page locked the main thread. Passive listener so the browser
	// can scroll without waiting on this handler.
	var scrollScheduled = false;
	window.addEventListener('scroll', function() {
		if (scrollScheduled) return;
		scrollScheduled = true;
		window.requestAnimationFrame(function() {
			scrollScheduled = false;
			if (window.innerHeight + window.scrollY >= document.body.offsetHeight - 50) {
				loadMore();
			}
		});
	}, { passive: true });
})();
