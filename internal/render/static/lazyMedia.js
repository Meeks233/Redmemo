(function () {
  // Lazy-loads post media in online listings: an image or video resource is
  // only requested once its post scrolls near the viewport, instead of every
  // post on the page firing its requests at once on load. Templates emit the
  // media URL as data-src / data-poster (no src/poster) when this is active;
  // see the "lazy_media" preference and the post_in_list template partial.

  function hydrate(el) {
    var src = el.getAttribute("data-src");
    if (src) {
      el.removeAttribute("data-src");
      el.setAttribute("src", src);
    }
    var poster = el.getAttribute("data-poster");
    if (poster) {
      el.removeAttribute("data-poster");
      el.setAttribute("poster", poster);
    }
  }

  function hydratePost(post) {
    var nodes = post.querySelectorAll("[data-src],[data-poster]");
    if (!nodes.length) return;
    var videos = [];
    nodes.forEach(function (el) {
      hydrate(el);
      var v = null;
      if (el.tagName === "VIDEO") v = el;
      else if (el.tagName === "SOURCE" && el.parentNode && el.parentNode.tagName === "VIDEO") v = el.parentNode;
      // A <video> whose <source>/src changed must be reloaded to pick it up.
      if (v && videos.indexOf(v) === -1) videos.push(v);
    });
    videos.forEach(function (v) { v.load(); });
  }

  function hydrateAll() {
    document.querySelectorAll(".post").forEach(hydratePost);
  }

  // No IntersectionObserver — hydrate everything so media still works.
  if (!("IntersectionObserver" in window)) {
    hydrateAll();
    return;
  }

  var observer = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (entry) {
        if (!entry.isIntersecting) return;
        hydratePost(entry.target);
        observer.unobserve(entry.target);
      });
    },
    // Start the fetch a little before the post is actually visible so media
    // is usually ready by the time the user reaches it.
    { rootMargin: "300px 0px" }
  );

  function observePosts() {
    document.querySelectorAll(".post").forEach(function (post) {
      if (post.querySelector("[data-src],[data-poster]")) {
        observer.observe(post);
      }
    });
  }

  observePosts();

  // Infinite scroll appends new posts after load — observe them as they arrive.
  // Coalesce mutation bursts into one rescan per frame so a growing page
  // doesn't run a full-document query on every individual DOM mutation.
  var scheduled = false;
  function scheduleObserve() {
    if (scheduled) return;
    scheduled = true;
    window.requestAnimationFrame(function () {
      scheduled = false;
      observePosts();
    });
  }

  var mo = window.MutationObserver && new MutationObserver(scheduleObserve);
  if (mo) mo.observe(document.body, { childList: true, subtree: true });
})();
