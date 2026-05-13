(function(){
	var KEY = '_qr';

	function initRing(arcEl, txtEl, radius) {
		if (!arcEl || !txtEl) return;
		var circ = 2 * Math.PI * radius;
		arcEl.style.strokeDasharray = circ;

		function render(rem, left, win) {
			txtEl.textContent = rem + " left";
			arcEl.style.strokeDashoffset = circ * (1 - left / win);
		}

		function save(rem, left, win, ts) {
			try { sessionStorage.setItem(KEY, JSON.stringify({r:rem,l:left,w:win,t:ts})); } catch(e){}
		}

		function tick(rem, left, win, startTs) {
			render(rem, left, win);
			save(rem, left, win, startTs);
			if (left <= 0) { update(); return; }
			var iv = setInterval(function() {
				left--;
				render(rem, left, win);
				save(rem, left, win, startTs);
				if (left <= 0) { clearInterval(iv); update(); }
			}, 1000);
		}

		function update() {
			fetch("/api/status").then(function(r){ return r.json(); }).then(function(d) {
				var rem = d.remaining, left = d.reset || 0, win = d.window || 600;
				if (left > 0) {
					tick(rem, left, win, Date.now());
				} else {
					render(rem, 0, win);
					save(rem, 0, win, Date.now());
					setTimeout(update, 5000);
				}
			}).catch(function(){ setTimeout(update, 10000); });
		}

		var cached;
		try { cached = JSON.parse(sessionStorage.getItem(KEY)); } catch(e){}
		if (cached && cached.t && cached.l > 0) {
			var elapsed = Math.round((Date.now() - cached.t) / 1000);
			var left = cached.l - elapsed;
			if (left > 0) {
				tick(cached.r, left, cached.w, cached.t);
				return;
			}
		}
		update();
	}

	initRing(
		document.getElementById("nav-ring-arc"),
		document.getElementById("nav-quota-text"),
		8
	);
})();
