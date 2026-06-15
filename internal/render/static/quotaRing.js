// quotaRing.js drives the navbar quota indicator.
//
//   - The ring fills proportionally to the REMAINING access count
//     (remaining / capacity) and shows that number in its centre.
//   - The text beside it is the time until the quota window resets,
//     formatted mm:ss and ticking down every second. CSS hides this text on
//     small screens (it is for medium/large screens only); the JS keeps it
//     updated regardless so it is correct the moment it becomes visible.
//
// /api/status supplies {remaining, capacity, reset, window}. We poll it every
// 5s for fresh counts; the mm:ss clock is interpolated locally between polls.
(function(){
	var KEY = '_qr';
	var CAP_FALLBACK = 99;

	function initRing(arcEl, countEl, txtEl, radius) {
		if (!arcEl || !countEl) return;
		var circ = 2 * Math.PI * radius;
		arcEl.style.strokeDasharray = circ;
		var currentRem = -1;
		var clockIv = null;
		var pollIv = null;
		var inFlight = false;
		var retryIv = null;

		// Fill the arc to remaining/capacity (clamped to [0,1]).
		function renderArc(rem, cap) {
			if (cap <= 0) cap = CAP_FALLBACK;
			var frac = rem / cap;
			if (frac < 0) frac = 0;
			if (frac > 1) frac = 1;
			arcEl.style.strokeDashoffset = circ * (1 - frac);
		}

		// The remaining count sits in the ring centre — but only surfaces when
		// the quota is running low (<=25% remaining). Above that the ring shows
		// fill alone, keeping the navbar uncluttered while plenty is left.
		function renderCount(rem, cap) {
			if (cap <= 0) cap = CAP_FALLBACK;
			countEl.style.display = (rem / cap <= 0.25) ? '' : 'none';
			if (rem === currentRem) return;
			var old = currentRem;
			currentRem = rem;
			countEl.textContent = rem;
			if (old >= 0) {
				countEl.classList.remove('flip');
				void countEl.getBoundingClientRect();
				countEl.classList.add('flip');
			}
		}

		// mm:ss countdown to the window reset (right-hand text).
		function renderClock(sec) {
			if (!txtEl) return;
			if (sec < 0) sec = 0;
			var m = Math.floor(sec / 60);
			var s = sec % 60;
			txtEl.textContent = m + ':' + (s < 10 ? '0' + s : s);
		}

		function save(rem, cap, endMs) {
			try { sessionStorage.setItem(KEY, JSON.stringify({r:rem,c:cap,e:endMs})); } catch(e){}
		}

		var _endMs = 0;
		var _cap = CAP_FALLBACK;

		// Drive the mm:ss clock locally so it ticks smoothly between polls.
		function startClock() {
			if (clockIv) clearInterval(clockIv);
			tick();
			clockIv = setInterval(tick, 1000);
		}

		function tick() {
			var sec = Math.max(0, Math.round((_endMs - Date.now()) / 1000));
			renderClock(sec);
			if (sec <= 0) {
				clearInterval(clockIv);
				clockIv = null;
				fetchStatus();
			}
		}

		function apply(d, persist) {
			var rem = d.remaining;
			var cap = d.capacity || CAP_FALLBACK;
			var reset = d.reset || 0;
			_cap = cap;
			_endMs = Date.now() + reset * 1000;
			renderCount(rem, cap);
			renderArc(rem, cap);
			if (reset > 0) {
				startClock();
			} else {
				if (clockIv) { clearInterval(clockIv); clockIv = null; }
				renderClock(0);
			}
			if (persist) save(rem, cap, _endMs);
		}

		function fetchStatus() {
			if (inFlight) return;
			inFlight = true;
			fetch('/api/status').then(function(r){ return r.json(); }).then(function(d) {
				inFlight = false;
				apply(d, true);
			}).catch(function(){
				inFlight = false;
				if (retryIv) clearTimeout(retryIv);
				retryIv = setTimeout(fetchStatus, 3000);
			});
		}

		pollIv = setInterval(fetchStatus, 5000);

		// Instant paint from cache so the ring never flashes empty on load.
		var cached;
		try { cached = JSON.parse(sessionStorage.getItem(KEY)); } catch(e){}
		if (cached && typeof cached.r === 'number') {
			_cap = cached.c || CAP_FALLBACK;
			_endMs = cached.e || 0;
			renderCount(cached.r, _cap);
			renderArc(cached.r, _cap);
			if (_endMs > Date.now()) startClock(); else renderClock(0);
		}
		fetchStatus();
	}

	initRing(
		document.getElementById('nav-ring-arc'),
		document.getElementById('nav-ring-count'),
		document.getElementById('nav-quota-text'),
		8
	);
})();
