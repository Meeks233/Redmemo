(function(){
	var KEY = '_qr';

	function initRing(arcEl, txtEl, radius) {
		if (!arcEl || !txtEl) return;
		var circ = 2 * Math.PI * radius;
		arcEl.style.strokeDasharray = circ;
		var currentRem = -1;
		var tickIv = null;
		var pollIv = null;

		function renderArc(left, win) {
			if (win <= 0) win = 1;
			arcEl.style.strokeDashoffset = circ * (1 - left / win);
		}

		function renderNum(rem) {
			if (rem === currentRem) return;
			var old = currentRem;
			currentRem = rem;
			txtEl.textContent = rem + ' left';
			if (old >= 0) {
				txtEl.classList.remove('flip');
				void txtEl.offsetWidth;
				txtEl.classList.add('flip');
			}
		}

		function save(rem, left, win, ts) {
			try { sessionStorage.setItem(KEY, JSON.stringify({r:rem,l:left,w:win,t:ts})); } catch(e){}
		}

		var _endMs = 0;
		var _win = 600;

		function startTick(rem, left, win, startTs) {
			if (tickIv) clearInterval(tickIv);
			_endMs = Date.now() + left * 1000;
			_win = win;
			renderNum(rem);
			renderArc(left, win);
			save(rem, left, win, startTs);
			tickIv = setInterval(function() {
				var now = Date.now();
				var secLeft = Math.max(0, Math.round((_endMs - now) / 1000));
				renderArc(secLeft, _win);
				save(currentRem, secLeft, _win, now);
				if (secLeft <= 0) {
					clearInterval(tickIv);
					tickIv = null;
					fetchStatus();
				}
			}, 1000);
		}

		function fetchStatus() {
			fetch('/api/status').then(function(r){ return r.json(); }).then(function(d) {
				var rem = d.remaining, left = d.reset || 0, win = d.window || 600;
				renderNum(rem);
				if (left > 0) {
					startTick(rem, left, win, Date.now());
				} else {
					renderArc(0, win);
					save(rem, 0, win, Date.now());
				}
			}).catch(function(){ setTimeout(fetchStatus, 3000); });
		}

		pollIv = setInterval(function() {
			fetch('/api/status').then(function(r){ return r.json(); }).then(function(d) {
				var rem = d.remaining, left = d.reset || 0, win = d.window || 600;
				renderNum(rem);
				if (left > 0) {
					if (!tickIv) {
						startTick(rem, left, win, Date.now());
					} else {
						_endMs = Date.now() + left * 1000;
						_win = win;
						save(rem, left, win, Date.now());
					}
				}
			}).catch(function(){});
		}, 5000);

		var cached;
		try { cached = JSON.parse(sessionStorage.getItem(KEY)); } catch(e){}
		if (cached && cached.t && cached.l > 0) {
			var elapsed = Math.round((Date.now() - cached.t) / 1000);
			var left = cached.l - elapsed;
			if (left > 0) {
				startTick(cached.r, left, cached.w, cached.t);
				return;
			}
		}
		fetchStatus();
	}

	initRing(
		document.getElementById('nav-ring-arc'),
		document.getElementById('nav-quota-text'),
		8
	);
})();
