(function () {
  if (!("IntersectionObserver" in window)) return;

  var observer = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        if (!entry.isIntersecting) return;
        var video = entry.target;
        video.preload = "auto";
        observer.unobserve(video);
      });
    },
    { rootMargin: "200%" }
  );

  function observeVideos() {
    document.querySelectorAll("video[preload='metadata']").forEach(function (v) {
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
