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

  // Start playback, coping with the browser autoplay policy.
  //
  // The data-viewport-autoplay videos are NOT muted, so before the user
  // has interacted with the page the browser rejects play() outright.
  // That is why the first videos on a freshly loaded page never started,
  // while videos reached after scrolling did: scrolling is itself a user
  // gesture that grants the page sticky activation.
  //
  // Browsers always allow *muted* autoplay, so on rejection we retry with
  // the video muted. The user keeps their native controls to unmute.
  function tryPlay(video) {
    var p = video.play();
    if (p && typeof p.catch === "function") {
      p.catch(function () {
        video.muted = true;
        video.play().catch(function () {});
      });
    }
  }

  function distanceToViewportCenter(video) {
    var rect = video.getBoundingClientRect();
    return Math.abs((rect.top + rect.bottom) / 2 - viewportHeight() / 2);
  }

  // Play only the fully-visible video closest to the viewport center.
  function updatePlayback() {
    if (document.visibilityState !== "visible") {
      candidates.forEach(function (video) {
        if (!video.paused) video.pause();
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

    candidates.forEach(function (video) {
      if (video === active) {
        tryPlay(video);
      } else if (!video.paused) {
        video.pause();
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
          if (!entry.target.paused) entry.target.pause();
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
    new MutationObserver(scheduleObserve)
      .observe(document.body, {
        childList: true,
        subtree: true,
        attributeFilter: ["data-viewport-autoplay"],
      });
  }
})();
