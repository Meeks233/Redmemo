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
		loading = true;
		spinner.style.display = '';
		fetch(url)
			.then(function(r) {
				if (r.status === 429) {
					setTimeout(function() { loading = false; doFetch(url, cb); }, interval);
					return null;
				}
				return r.text();
			})
			.then(function(html) {
				if (html === null) return;
				spinner.style.display = 'none';
				cb(html);
			})
			.catch(function() {
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

	window.addEventListener('scroll', function() {
		if (window.innerHeight + window.scrollY >= document.body.offsetHeight - 50) {
			loadMore();
		}
	});
})();
