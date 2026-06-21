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
  // Listing-card right strip: a link post / single-link self post with no Reddit
  // thumbnail. We unfurl the link and drop its og:image into the strip (an image
  // thumbnail, NOT a full card) so the listing matches a normal link post.
  var THUMB_SELECTOR = "a.post_thumbnail[data-unfurl-thumb]";
  var MAX_CONCURRENT = 2; // gentle on third-party hosts (GitHub et al.)

  var queue = [];
  var active = 0;

  // Page-level dedup. A link post repeats its destination URL twice — once as the
  // top #post_url and again as a bare auto-link in the body ("GitHub: <url>"). We
  // only ever want ONE intel card per URL, so the first occurrence in document
  // order wins and every later duplicate stays a plain text link. Keyed by a
  // trailing-slash-normalised URL so ".../posta" and ".../posta/" collapse.
  var seen = Object.create(null);
  function normURL(u) { return (u || "").replace(/\/+$/, "").toLowerCase(); }

  // clipOf returns the unexpanded teaser clip an element lives in (with its expand
  // state), or null when the element isn't inside a collapsed listing preview.
  function clipOf(el) {
    var clip = el.closest && el.closest(".post_clipped");
    if (!clip) return null;
    var toggle = clip.parentNode && clip.parentNode.querySelector(".post_expand_toggle");
    return { clip: clip, expanded: !!(toggle && toggle.checked) };
  }

  // A long listing body is rendered into a collapsed teaser: .post_clipped caps it
  // at ~250px with a bottom gradient mask that fades the final lines out (the mask
  // starts at FADE_START of the box height). A link is parked — left a plain link
  // until the user expands the preview — in two cases inside an UNEXPANDED clip:
  //
  //   (a) it sits in the fade tail (or scrolled clean out below the cap): it's
  //       "about to disappear", so unfurling a card there is wasted + noisy; and
  //   (b) the clip is already card-blocked — an earlier link in this same teaser
  //       unfurled into an OVER-LONG banner/video card (taller than the 250px
  //       window, which alone swallows the whole preview), so per the over-long
  //       rule NO further link here parses until expand (see tooTallForClip).
  //
  // The change listener below re-enqueues both kinds once the cap drops. A
  // non-clipped (short) body is never affected — its single inline card is fine.
  var FADE_START = 0.6; // mirror .post_clipped mask-image gradient stop (#000 60%)
  function clippedAway(el) {
    var c = clipOf(el);
    if (!c || c.expanded) return false;
    if (c.clip.getAttribute("data-cards-blocked")) return true; // (b) over-long card already here
    var offset = el.getBoundingClientRect().top - c.clip.getBoundingClientRect().top;
    return offset > c.clip.clientHeight * FADE_START; // (a) in the fade tail / below the cap
  }

  // tooTallForClip is the over-long guard: a banner (--media) or video card is far
  // taller than the ~250px teaser, so dropping one into an unexpanded clip blows
  // the preview out. When the card we just built is one of those AND lives in an
  // unexpanded clip, we refuse it and mark the clip card-blocked, so every later
  // link in the same teaser also stays a plain link until the user expands it.
  // A compact (--card) thumbnail row fits the window and is always allowed.
  function tooTallForClip(el, card) {
    if (!card.classList.contains("link-preview--media") &&
        !card.classList.contains("link-preview--video")) return false;
    var c = clipOf(el);
    if (!c || c.expanded) return false;
    c.clip.setAttribute("data-cards-blocked", "1");
    return true;
  }

  function pump() {
    while (active < MAX_CONCURRENT && queue.length) {
      var el = queue.shift();
      if (!el || !el.isConnected) continue;
      // Re-check geometry at dequeue time (images loading above may have pushed the
      // link down into the fade/clip zone since it was observed). Park it for the
      // expand re-scan instead of carding a link that's no longer really visible.
      if (clippedAway(el)) { el.setAttribute("data-unfurl-state", "clipped"); continue; }
      active++;
      // A strip thumbnail fills its image; everything else builds a card.
      if (el.hasAttribute("data-unfurl-thumb")) fillThumb(el);
      else unfurl(el);
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
          // The fetch was in flight; if media loading above pushed this link into
          // the fade/clip zone meanwhile, park it for the expand re-scan rather
          // than dropping a card no one can see.
          if (clippedAway(el)) { el.setAttribute("data-unfurl-state", "clipped"); return; }
          var card = buildCard(data, el.getAttribute("href"));
          // Over-long guard: a banner/video card would swallow an unexpanded teaser.
          // Refuse it (and block the clip's later links) until the user expands.
          if (card && tooTallForClip(el, card)) { el.setAttribute("data-unfurl-state", "clipped"); return; }
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

  // fillThumb unfurls a listing strip's link and, on success, swaps the link/
  // placeholder for the og:image — a cover-cropped thumbnail filling the strip,
  // exactly the surface a Reddit-thumbnailed link post already shows. On a miss
  // (or a metadata-only unfurl with no image) the placeholder is left untouched.
  function fillThumb(a) {
    var url = a.getAttribute("data-unfurl-thumb");
    a.setAttribute("data-unfurl-state", "fetching");
    fetch("/api/unfurl?url=" + encodeURIComponent(url), { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : { status: "failed" }; })
      .then(function (data) {
        if (!data || data.status !== "ok" || !data.image) return;
        var img = el("img", "unfurl-thumb");
        img.alt = "";
        // NOTE: no loading="lazy" here. The img is detached until it loads, and a
        // disconnected lazy image is never fetched — so the load event would never
        // fire and the strip would hang. fillThumb only runs once the strip is in
        // view (IntersectionObserver), so an eager load is correct anyway.
        img.addEventListener("load", function () {
          var ph = a.querySelector(":scope > svg"); // the placeholder glyph
          if (ph) ph.parentNode.removeChild(ph);
          a.classList.remove("no_thumbnail");
          var wrap = el("div", "unfurl-thumb-wrap");
          wrap.appendChild(img);
          // Move the domain label into the image box so it overlays the image's
          // bottom edge (the box is now natural-aspect, not the full strip).
          var span = a.querySelector(":scope > span");
          if (span) wrap.appendChild(span);
          a.insertBefore(wrap, a.firstChild);
        });
        // GitHub's opengraph host (opengraph.githubassets.com) 429s a burst of
        // card images; retry a few times with jittered backoff before giving up
        // and leaving the placeholder, mirroring buildCard's image handling.
        var attempts = 0;
        img.addEventListener("error", function () {
          if (attempts < 3) {
            attempts++;
            var sep = data.image.indexOf("?") >= 0 ? "&" : "?";
            setTimeout(function () { img.src = data.image + sep + "rmretry=" + attempts; },
              1200 * attempts + Math.floor(Math.random() * 1500));
          }
        });
        img.src = data.image;
      })
      .catch(function () {})
      .then(done, done);
  }

  function el(tag, cls) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    return n;
  }

  // sizeVideo stamps the card's aspect-ratio from the poster's real dimensions so
  // a portrait clip renders portrait before the user ever hits play (a not-yet-
  // played <video> reports no dimensions of its own). Images need no sizing — the
  // CSS chip is a fixed square regardless of the source pixels.
  function sizeVideo(m, w, h) {
    if (w && h) m.style.aspectRatio = w + " / " + h;
  }

  // applyImageVariant makes the FINAL big-vs-small call from the real loaded
  // pixels (the only fully reliable signal — server isWideImage only seeds the
  // pre-load placeholder to avoid a flip). A wide landscape preview (GitHub's
  // 1280×640 repo card, a news hero shot) becomes a full-width banner (--media);
  // a small square favicon / site logo (Stack Overflow's apple-touch-icon) stays
  // a compact thumbnail row (--card). Square-but-large og:images stay compact too
  // — only a clearly landscape image earns the banner, so a logo never balloons.
  var BANNER_MIN_LONG = 400; // a banner image's longer side must clear this
  var BANNER_MIN_RATIO = 1.3; // …and it must be at least this much wider than tall
  function applyImageVariant(a, img, w, h) {
    if (!w || !h) return;
    var wide = w / h >= BANNER_MIN_RATIO && Math.max(w, h) >= BANNER_MIN_LONG;
    // Late over-long guard: this card was inserted compact (the server hint said
    // not-wide) but the real pixels are a banner. If it lives in an unexpanded
    // teaser, do NOT let it balloon — keep it compact and block the clip's other
    // links, mirroring tooTallForClip's insert-time refusal. Once expanded it
    // unfurls at full banner size like anywhere else.
    if (wide) {
      var c = clipOf(a);
      if (c && !c.expanded) { c.clip.setAttribute("data-cards-blocked", "1"); wide = false; }
    }
    a.classList.remove("link-preview--media", "link-preview--card");
    if (wide) {
      a.classList.add("link-preview--media");
      img.style.aspectRatio = w + " / " + h; // hold the banner box; height follows width
    } else {
      a.classList.add("link-preview--card");
      img.style.aspectRatio = "";
    }
  }

  function buildCard(data, href) {
    // Each link unfurls into one of three card shapes: a playable clip keeps the
    // player on top (--video); an image-bearing link splits big-vs-small — a wide
    // GitHub/news preview banners full-width (--media), a bare logo/favicon stays a
    // compact thumbnail row (--card). image_wide is the server's pre-load hint; the
    // real call is remade from the loaded image's aspect ratio (applyImageVariant).
    var isVideo = !!data.video;
    var variant = isVideo ? "video" : (data.image && data.image_wide ? "media" : "card");
    var a = el("a", "link-preview link-preview--" + variant);
    a.href = data.url || href;
    a.target = "_blank";
    a.rel = "nofollow noopener noreferrer";

    if (isVideo) {
      var v = el("video", "link-preview-media");
      v.controls = true;
      v.preload = "none";
      v.playsInline = true;
      if (data.image) {
        v.poster = data.image;
        var probe = new Image();
        probe.addEventListener("load", function () { sizeVideo(v, probe.naturalWidth, probe.naturalHeight); });
        probe.src = data.image;
      }
      v.src = data.video;
      // A click on the <video> must not also follow the card link.
      v.addEventListener("click", function (e) { e.preventDefault(); e.stopPropagation(); });
      a.appendChild(v);
    } else if (data.image) {
      var img = el("img", "link-preview-media");
      img.loading = "lazy"; // native defer: only fetched when near the viewport
      img.alt = "";
      // Re-decide banner vs compact thumbnail from the real pixels once they load.
      img.addEventListener("load", function () { applyImageVariant(a, img, img.naturalWidth, img.naturalHeight); });
      // Image hosts that gate by IP (GitHub's opengraph.githubassets.com) can
      // 429 a burst of card images on a link-heavy page. Retry a couple of times
      // with jittered backoff — the throttle clears quickly — before giving up and
      // dropping the thumbnail (the card stays a tidy text-only row). The cache-
      // buster query forces a re-request rather than reuse of the failed response.
      var attempts = 0;
      img.addEventListener("error", function () {
        if (attempts < 3) {
          attempts++;
          var sep = data.image.indexOf("?") >= 0 ? "&" : "?";
          setTimeout(function () { img.src = data.image + sep + "rmretry=" + attempts; },
            1200 * attempts + Math.floor(Math.random() * 1500));
        } else if (img.parentNode) {
          img.parentNode.removeChild(img);
          // No image survived: collapse any banner back to the compact text row.
          a.classList.remove("link-preview--media");
          a.classList.add("link-preview--card");
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

  // strip turns an anchor back into a plain text link: a duplicate URL keeps the
  // bare link (redlib's simplicity) instead of growing a second identical card.
  function strip(a) {
    a.classList.remove("link-preview-lazy");
    a.removeAttribute("data-unfurl");
    a.setAttribute("data-unfurl-state", "duplicate");
  }

  function scan(root) {
    var r = root || document;
    r.querySelectorAll(SELECTOR).forEach(function (a) {
      if (a.getAttribute("data-unfurl-state")) return;
      var key = normURL(a.getAttribute("data-unfurl"));
      if (key && seen[key]) { strip(a); return; } // already carded elsewhere
      if (key) seen[key] = true;
      if (observer) observer.observe(a);
      else enqueue(a); // no IO support: just resolve them all
    });
    // Listing strip thumbnails: no per-URL dedup (each card owns its own strip).
    r.querySelectorAll(THUMB_SELECTOR).forEach(function (a) {
      if (a.getAttribute("data-unfurl-state")) return;
      if (observer) observer.observe(a);
      else enqueue(a);
    });
  }

  // Expanding a collapsed preview (.post_clipped) drops its height cap and reveals
  // the full body — so the links we parked (fade tail, or blocked behind an
  // over-long banner) are now fully visible and should unfurl. Lift the clip's
  // card-block and re-enqueue every parked link under the toggled wrapper. (The
  // toggle is a pure-CSS checkbox; we only need its change event.)
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (!t || !t.classList || !t.classList.contains("post_expand_toggle") || !t.checked) return;
    var wrap = t.closest(".post_body_wrap");
    if (!wrap) return;
    var clip = wrap.querySelector(".post_clipped");
    if (clip) clip.removeAttribute("data-cards-blocked"); // banners may unfurl now
    wrap.querySelectorAll(SELECTOR).forEach(function (a) {
      if (a.getAttribute("data-unfurl-state") === "clipped") {
        a.removeAttribute("data-unfurl-state");
        enqueue(a);
      }
    });
  });

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
