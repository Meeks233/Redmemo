(function () {
  if (!("IntersectionObserver" in window)) return;

  var observer = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        var video = entry.target;
        if (entry.intersectionRatio >= 1.0) {
          video.play().catch(function () {});
        } else {
          if (!video.paused) video.pause();
        }
      });
    },
    { threshold: 1.0 }
  );

  function observeVideos() {
    document.querySelectorAll("video[data-viewport-autoplay]").forEach(function (v) {
      if (!v._viewportObserved) {
        v._viewportObserved = true;
        observer.observe(v);
      }
    });
  }

  observeVideos();

  if (window.MutationObserver) {
    new MutationObserver(function () { observeVideos(); })
      .observe(document.body, { childList: true, subtree: true, attributes: true });
  }
})();
