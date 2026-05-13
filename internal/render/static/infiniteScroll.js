(function(){
	var posts = document.getElementById('posts');
	var spinner = document.getElementById('load-spinner');
	var endMsg = document.getElementById('load-end');
	var loader = document.getElementById('infinite-loader');
	if (!loader || !posts) return;

	var offset = parseInt(loader.dataset.offset, 10) || 0;
	var sort = loader.dataset.sort;
	var interval = (parseInt(loader.dataset.interval, 10) || 2) * 1000;
	var loading = false;
	var done = false;

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
		doFetch('/?sort=' + sort + '&offset=' + offset + '&partial=1', function(html) {
			if (!html.trim()) {
				done = true;
				endMsg.style.display = '';
				loading = false;
				return;
			}
			var tmp = document.createElement('div');
			tmp.innerHTML = '<hr class="sep" />' + html;
			while (tmp.firstChild) posts.appendChild(tmp.firstChild);
			offset += 5;
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
		history.replaceState(null, '', '/?sort=' + newSort);
		doFetch('/?sort=' + newSort + '&offset=0&partial=1', function(html) {
			spinner.style.display = 'none';
			if (!html.trim()) {
				done = true;
				endMsg.style.display = '';
				loading = false;
				return;
			}
			posts.innerHTML = html;
			offset = 5;
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
