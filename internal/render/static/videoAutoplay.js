(function () {
  if (!("IntersectionObserver" in window)) return;

  // All observed videos currently intersecting the viewport at all.
  // Eligibility + centermost decisions are made geometrically in
  // updatePlayback() — relying on intersectionRatio === 1.0 is flaky
  // (subpixel rounding reports 0.999…) and also wrongly excludes videos
  // that are taller than the viewport or only partly on-screen at first
  // load, which is exactly the homepage case that wasn't autoplaying.
  var candidates = new Set();
  var scrollScheduled = false;

  // The video chosen by the last updatePlayback() pass (centermost, eligible),
  // or null. Used to scope a manual pause to the current play session: when the
  // active video changes, the previous one's session is over.
  var activeVideo = null;

  // A video must have at least this fraction of its height on-screen to
  // be considered for autoplay.
  var VISIBLE_FRACTION = 0.5;

  function viewportHeight() {
    return window.innerHeight || document.documentElement.clientHeight;
  }

  // Fraction of the video's height currently inside the viewport (0..1).
  function visibleFraction(video) {
    var rect = video.getBoundingClientRect();
    if (rect.height <= 0) return 0;
    var visible = Math.min(rect.bottom, viewportHeight()) - Math.max(rect.top, 0);
    return Math.max(0, visible) / rect.height;
  }

  // Autoplay "intel": remember whether the *user* paused a video, so we never
  // fight their intent. The browser fires the same "pause"/"play" events
  // whether the script or the human triggered them, so we cannot tell them
  // apart after the fact. Instead, every time the script calls play()/pause()
  // it raises a one-shot flag that the matching event handler consumes; an
  // event seen with no flag set must therefore be user-initiated.
  //
  // Media events are dispatched on a later task, so the flag is still set when
  // the handler runs. We only raise it when the call will actually change
  // state (play() on a paused video, pause() on a playing one) — otherwise no
  // event fires and the flag would leak onto the next genuine user action.

  // Mark this video as paused by the script; the "pause" handler ignores it.
  function scriptPause(video) {
    if (video.paused) return;
    video._programmaticPause = true;
    video.pause();
  }

  // Start playback, coping with the browser autoplay policy.
  //
  // Before the user has interacted with the page the browser rejects an
  // unmuted play() outright. That is why the first videos on a freshly
  // loaded page never started, while videos reached after scrolling did:
  // scrolling is itself a user gesture that grants the page sticky
  // activation.
  //
  // Browsers always allow *muted* autoplay, so on rejection we may retry
  // with the video muted — but ONLY for videos the mute settings actually
  // want muted. The template's videoMuted() sets the `muted` content
  // attribute (reflected by defaultMuted) per the user's "Mute all" /
  // "Mute NSFW" choices. A video the user chose to hear must never be
  // silently muted just to satisfy autoplay; we leave it paused and the
  // next scroll (a user gesture) lets the unmuted play() succeed.
  function tryPlay(video) {
    // Respect an explicit user pause: do not resume on our own.
    if (video._userPaused) return;
    video._programmaticPlay = true;
    var p = video.play();
    if (p && typeof p.catch === "function") {
      p.catch(function () {
        // play() was rejected, so no "play" event will arrive to consume the
        // flag — clear it here so a later user play() is not mistaken for ours.
        video._programmaticPlay = false;
        // Only fall back to muted autoplay for videos meant to be muted.
        if (!video.defaultMuted) return;
        video.muted = true;
        video._programmaticPlay = true;
        video.play().catch(function () {
          video._programmaticPlay = false;
        });
      });
    }
  }

  // Attach the user-intent listeners once per video.
  function trackIntent(video) {
    if (video._intentTracked) return;
    video._intentTracked = true;
    video.addEventListener("pause", function () {
      if (video._programmaticPause) {
        video._programmaticPause = false;
        return;
      }
      // The user hit pause: remember it so re-entering the viewport or
      // returning to the tab does not yank playback back on.
      video._userPaused = true;
    });
    video.addEventListener("play", function () {
      if (video._programmaticPlay) {
        video._programmaticPlay = false;
        return;
      }
      // The user hit play: forget the earlier manual pause so normal
      // viewport-driven autoplay resumes.
      video._userPaused = false;
    });
  }

  function distanceToViewportCenter(video) {
    var rect = video.getBoundingClientRect();
    return Math.abs((rect.top + rect.bottom) / 2 - viewportHeight() / 2);
  }

  // Play only the fully-visible video closest to the viewport center.
  function updatePlayback() {
    if (document.visibilityState !== "visible") {
      candidates.forEach(function (video) {
        scriptPause(video);
      });
      return;
    }

    var active = null;
    var bestDistance = Infinity;
    candidates.forEach(function (video) {
      if (visibleFraction(video) < VISIBLE_FRACTION) return;
      var d = distanceToViewportCenter(video);
      if (d < bestDistance) {
        bestDistance = d;
        active = video;
      }
    });

    // A manual pause only lasts for the current play session — i.e. while this
    // video stays the active (centermost) one. The moment the user moves on to
    // a different video, the old one's session is over, so forget its
    // _userPaused flag: scrolling back to it later should autoplay again
    // instead of leaving it stuck. While the tab is hidden we return early
    // above without touching activeVideo, so returning to the tab still finds
    // the same active video and keeps respecting a pause — matching the prior
    // behavior and avoiding a conflict with it.
    if (activeVideo && activeVideo !== active && activeVideo._userPaused) {
      activeVideo._userPaused = false;
    }
    activeVideo = active;

    candidates.forEach(function (video) {
      if (video === active) {
        tryPlay(video);
      } else {
        scriptPause(video);
      }
    });
  }

  var observer = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        if (entry.isIntersecting) {
          candidates.add(entry.target);
        } else {
          candidates.delete(entry.target);
          scriptPause(entry.target);
        }
      });
      updatePlayback();
    },
    { threshold: [0, 0.25, 0.5, 0.75, 1.0] }
  );

  // While scrolling, the centermost video changes even if several stay
  // fully visible — re-evaluate on scroll (throttled via rAF).
  window.addEventListener(
    "scroll",
    function () {
      if (scrollScheduled) return;
      scrollScheduled = true;
      window.requestAnimationFrame(function () {
        scrollScheduled = false;
        updatePlayback();
      });
    },
    { passive: true }
  );

  // Pause when leaving the tab; re-evaluate on return.
  document.addEventListener("visibilitychange", updatePlayback);

  function observeVideos() {
    document.querySelectorAll("video[data-viewport-autoplay]").forEach(function (v) {
      if (!v._viewportObserved) {
        v._viewportObserved = true;
        trackIntent(v);
        observer.observe(v);
      }
    });
  }

  observeVideos();
  // Kick off playback for videos already on-screen at first load.
  updatePlayback();

  // Coalesce mutation bursts: many DOM changes in one frame (infinite-scroll
  // appends, hydration) collapse into a single rescan on the next frame.
  var observeScheduled = false;
  function scheduleObserve() {
    if (observeScheduled) return;
    observeScheduled = true;
    window.requestAnimationFrame(function () {
      observeScheduled = false;
      observeVideos();
    });
  }

  if (window.MutationObserver) {
    // attributeFilter, not a blanket attributes:true — the latter fires this
    // callback on every attribute change anywhere in the document (styles,
    // classes, lazy-load src swaps), which scans the whole tree each time and
    // freezes the page as it grows. We only need to notice videos that gain
    // the data-viewport-autoplay marker; node insertions are caught by
    // childList. The full-document rescan is also debounced to one per frame.
    //
    // Scope to <main> so the observer ignores mutations in the nav/footer
    // chrome — all posts (including infinite-scroll appends) live inside it.
    var observeRoot = document.querySelector("main") || document.body;
    new MutationObserver(scheduleObserve)
      .observe(observeRoot, {
        childList: true,
        subtree: true,
        attributeFilter: ["data-viewport-autoplay"],
      });
  }
})();
