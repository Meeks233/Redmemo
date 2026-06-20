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

  // Media display bounds (CSS px). A real photo/video renders at its NATURAL
  // aspect ratio within these — portrait stays portrait, landscape stays
  // landscape — and is never upscaled past its native size. Only a small,
  // square-ish image (a logo / favicon / avatar) collapses to the compact
  // left-thumbnail layout.
  var MEDIA_MAX_W = 420, MEDIA_MAX_H = 440, MEDIA_MIN_W = 230, LOGO_MAX = 300;

  // applyMedia sizes the card from the media's REAL dimensions. This is the
  // authoritative classifier (the server's image_wide is only an initial hint):
  // og:image:width meta lies often (GitHub stamps summary_large_image on square
  // avatars; many sites omit it), but the loaded pixels never do.
  function applyMedia(a, m, w, h) {
    if (!w || !h) return;
    a.classList.remove("link-preview--media", "link-preview--small", "link-preview--text");
    a.style.width = "";
    m.style.aspectRatio = "";
    var maxd = Math.max(w, h), r = w / h;
    if (maxd <= LOGO_MAX && r >= 0.8 && r <= 1.25) {
      a.classList.add("link-preview--small"); // small square → logo thumbnail (CSS sizes it)
      return;
    }
    // Real media: fit within the bounds preserving aspect, never enlarging.
    a.classList.add("link-preview--media");
    var scale = Math.min(MEDIA_MAX_W / w, MEDIA_MAX_H / h, 1);
    var dw = Math.round(w * scale);
    a.style.width = Math.max(dw, MEDIA_MIN_W) + "px";
    m.style.aspectRatio = w + " / " + h; // holds the box; height follows width
  }

  function buildCard(data, href) {
    // Start every media card as a natural-aspect "media" card (reasonably sized),
    // not a tiny thumbnail — applyMedia downgrades to a logo thumbnail only if the
    // loaded pixels are actually small+square. This avoids the jarring "tiny → big"
    // flip and never leaves a real photo stuck at 84px.
    var initial = data.image || data.video ? "media" : "text";
    var a = el("a", "link-preview link-preview--" + initial);
    a.href = data.url || href;
    a.target = "_blank";
    a.rel = "nofollow noopener noreferrer";

    if (data.video) {
      var v = el("video", "link-preview-media");
      v.controls = true;
      v.preload = "none";
      v.playsInline = true;
      if (data.image) v.poster = data.image;
      v.src = data.video;
      // A click on the <video> must not also follow the card link.
      v.addEventListener("click", function (e) { e.preventDefault(); e.stopPropagation(); });
      // A not-yet-played video has no readable dimensions, so size the card from
      // the poster image's aspect (Twitter's video poster matches the clip) — a
      // portrait clip then renders portrait before the user ever hits play.
      if (data.image) {
        var probe = new Image();
        probe.addEventListener("load", function () { applyMedia(a, v, probe.naturalWidth, probe.naturalHeight); });
        probe.src = data.image;
      }
      a.appendChild(v);
    } else if (data.image) {
      var img = el("img", "link-preview-media");
      img.loading = "lazy"; // native defer: only fetched when near the viewport
      img.alt = "";
      img.addEventListener("load", function () { applyMedia(a, img, img.naturalWidth, img.naturalHeight); });
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
          a.style.width = "";
          a.classList.remove("link-preview--media", "link-preview--small");
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
