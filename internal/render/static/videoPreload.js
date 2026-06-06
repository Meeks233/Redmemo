(function () {
  if (!("IntersectionObserver" in window)) return;

  // Long-lived observation: toggle preload between "auto" (in / near viewport)
  // and "metadata" (off-screen) so the browser releases speculative buffers as
  // the user scrolls past. The earlier one-shot version unobserved on first
  // intersect and left every visited video at preload="auto", which froze the
  // page once ~15 videos had accumulated full media buffers in memory.
  //
  // Crucially, demote ONLY when the video is paused. Flipping preload to
  // "metadata" on a playing video makes browsers abort the in-flight range
  // request — that regression was breaking playback of every video the user
  // scrolled near. rootMargin="100%" keeps the active video on "auto" while
  // the user reads comments below it; once it leaves that wider zone, it has
  // almost always already been paused (by the user, or by videoAutoplay's
  // scriptPause for autoplay shorts), so the demote is safe.
  var observer = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        var video = entry.target;
        if (entry.isIntersecting) {
          video.preload = "auto";
        } else if (video.paused) {
          video.preload = "metadata";
        }
      });
    },
    { rootMargin: "100%" }
  );

  function observeVideos() {
    document.querySelectorAll("video").forEach(function (v) {
      if (v._preloadObserved) return;
      v._preloadObserved = true;
      observer.observe(v);
    });
  }

  observeVideos();

  // Coalesce mutation bursts into one rescan per frame so a growing,
  // infinitely-scrolled page doesn't run a full-document query per mutation.
  var scheduled = false;
  function scheduleObserve() {
    if (scheduled) return;
    scheduled = true;
    window.requestAnimationFrame(function () {
      scheduled = false;
      observeVideos();
    });
  }

  // Scope to <main> so nav/footer chrome mutations don't trigger rescans; all
  // posts (including infinite-scroll appends) live inside it.
  var observeRoot = document.querySelector("main") || document.body;
  var mo = window.MutationObserver && new MutationObserver(scheduleObserve);
  if (mo) mo.observe(observeRoot, { childList: true, subtree: true });
})();
