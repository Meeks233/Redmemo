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

  var mo = window.MutationObserver && new MutationObserver(function () { observeVideos(); });
  if (mo) mo.observe(document.body, { childList: true, subtree: true });
})();
