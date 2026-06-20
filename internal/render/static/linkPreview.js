(function () {
  // Lazy, client-driven link previews. The server renders each bare external
  // link as <a class="link-preview-lazy" data-unfurl="URL" href="URL">URL</a>.
  // We observe those anchors and, only once one scrolls near the viewport, ask
  // /api/unfurl for its metadata (one link at a time, concurrency-limited, so a
  // megathread of links never bursts cross-site fetches and gets the host to
  // rate-limit). On success we replace the link with a preview card whose image
  // and video are loaded DIRECTLY by this browser — RedMemo never proxies that
  // media. On failure the original link is left exactly as it was.
  //
  // Mirrors lazyMedia.js's IntersectionObserver approach; runs on any page that
  // ships the media bundle (post + listing pages), where link-bearing bodies
  // live.

  var SELECTOR = "a.link-preview-lazy[data-unfurl]";
  var MAX_CONCURRENT = 2; // gentle on third-party hosts (GitHub et al.)

  var queue = [];
  var active = 0;

  function pump() {
    while (active < MAX_CONCURRENT && queue.length) {
      var el = queue.shift();
      if (el && el.isConnected) {
        active++;
        unfurl(el);
      }
    }
  }

  function enqueue(el) {
    if (el.getAttribute("data-unfurl-state")) return;
    el.setAttribute("data-unfurl-state", "queued");
    queue.push(el);
    pump();
  }

  function done() {
    active--;
    pump();
  }

  function unfurl(el) {
    var url = el.getAttribute("data-unfurl");
    fetch("/api/unfurl?url=" + encodeURIComponent(url), { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : { status: "failed" }; })
      .then(function (data) {
        if (data && data.status === "ok") {
          var card = buildCard(data, el.getAttribute("href"));
          if (card && el.parentNode) el.parentNode.replaceChild(card, el);
        } else {
          // Leave the plain link; mark so we don't retry it.
          el.setAttribute("data-unfurl-state", "failed");
          el.removeAttribute("data-unfurl");
        }
      })
      .catch(function () {
        el.setAttribute("data-unfurl-state", "failed");
        el.removeAttribute("data-unfurl");
      })
      .then(done, done);
  }

  function el(tag, cls) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    return n;
  }

  function buildCard(data, href) {
    var variant = data.video ? "video" : data.image_wide ? "large" : data.image ? "small" : "text";
    var a = el("a", "link-preview link-preview--" + variant);
    a.href = data.url || href;
    a.target = "_blank";
    a.rel = "nofollow noopener noreferrer";

    // Media: image banner / thumbnail / playable video — all loaded directly by
    // the browser from the third-party host.
    if (data.video) {
      var v = el("video", "link-preview-media");
      v.controls = true;
      v.preload = "none";
      if (data.image) v.poster = data.image;
      v.src = data.video;
      // A click on the <video> controls must not also follow the card link.
      v.addEventListener("click", function (e) { e.preventDefault(); e.stopPropagation(); });
      a.appendChild(v);
    } else if (data.image) {
      var img = el("img", "link-preview-media");
      img.loading = "lazy"; // native defer: only fetched when near the viewport
      img.alt = "";
      // The server's image_wide is only a hint. The real image's aspect ratio is
      // the authoritative signal: GitHub (and others) set summary_large_image
      // even on profile pages whose og:image is a SQUARE avatar — rendering that
      // as a banner blows a logo up huge. So once the image loads, reclassify by
      // its natural ratio — clearly landscape → large banner, square-ish → small
      // logo thumbnail.
      img.addEventListener("load", function () {
        if (!img.naturalWidth || !img.naturalHeight) return;
        var wide = img.naturalWidth / img.naturalHeight >= 1.6;
        a.classList.toggle("link-preview--large", wide);
        a.classList.toggle("link-preview--small", !wide);
      });
      // Image hosts that gate by IP (GitHub's opengraph.githubassets.com) can
      // 429 a burst of card images on a link-heavy page. Retry a couple of times
      // with jittered backoff — the throttle clears quickly — before giving up
      // and degrading the card to text-only. The cache-buster query forces the
      // browser to re-request rather than reuse the failed response.
      var attempts = 0;
      img.addEventListener("error", function () {
        if (attempts < 3) {
          attempts++;
          var sep = data.image.indexOf("?") >= 0 ? "&" : "?";
          setTimeout(function () { img.src = data.image + sep + "rmretry=" + attempts; },
            1200 * attempts + Math.floor(Math.random() * 1500));
        } else if (img.parentNode) {
          img.parentNode.removeChild(img);
          a.classList.remove("link-preview--small", "link-preview--large");
          a.classList.add("link-preview--text");
        }
      });
      img.src = data.image;
      a.appendChild(img);
    }

    var body = el("span", "link-preview-body");
    if (data.site) {
      var s = el("span", "link-preview-site");
      s.textContent = data.site;
      body.appendChild(s);
    }
    if (data.title) {
      var t = el("span", "link-preview-title");
      t.textContent = data.title;
      body.appendChild(t);
    }
    if (data.description) {
      var d = el("span", "link-preview-desc");
      d.textContent = data.description;
      body.appendChild(d);
    }
    a.appendChild(body);
    return a;
  }

  var observer = ("IntersectionObserver" in window)
    ? new IntersectionObserver(function (entries) {
        entries.forEach(function (entry) {
          if (!entry.isIntersecting) return;
          observer.unobserve(entry.target);
          enqueue(entry.target);
        });
      }, { rootMargin: "400px 0px" })
    : null;

  function scan(root) {
    (root || document).querySelectorAll(SELECTOR).forEach(function (a) {
      if (a.getAttribute("data-unfurl-state")) return;
      if (observer) observer.observe(a);
      else enqueue(a); // no IO support: just resolve them all
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { scan(document); });
  } else {
    scan(document);
  }

  // Re-scan when comment threads are injected lazily (load-more, replies).
  if ("MutationObserver" in window) {
    new MutationObserver(function (muts) {
      muts.forEach(function (m) {
        m.addedNodes && m.addedNodes.forEach(function (node) {
          if (node.nodeType === 1) scan(node);
        });
      });
    }).observe(document.body, { childList: true, subtree: true });
  }
})();
