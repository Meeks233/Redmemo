// @license http://www.gnu.org/licenses/agpl-3.0.html AGPL-3.0
// imageReload.js — recover post images that could not be served yet.
//
// When RedMemo cannot serve an image (the upstream fetch was rate-limited,
// blocked, or returned a non-image body) the media proxy answers the <img>
// request with 503, so the element fires an `error` event. This script catches
// that: it hides the browser's broken-image icon, draws an animated loader
// spinner in the slot, and — once the image scrolls near the viewport — polls
// /api/media_status. As soon as the server reports the media cached and ready,
// it reloads just that one <img> in place, with no page refresh.
//
// All visuals are applied inline (and the spin runs via the Web Animations
// API), so the spinner shows correctly even when the page is using a stale
// browser-cached style.css. Modelled on audioSync.js's poll-then-reload flow.
(function () {
    "use strict";

    var POLL_MS = 2500;
    var MAX_POLLS = 120; // ~5 min ceiling per image

    // Proxy-path prefixes whose readiness /api/media_status can report. An
    // image pointing straight at an external host can't be polled — skip it.
    var PROXY_RE = /^\/(?:img|preview|thumb|emoji)\//;

    // The lucide loader glyph, drawn over an unloaded image. stroke=currentColor
    // picks up the spinner span's colour; the Web Animations API spins it.
    var SPINNER_SVG =
        '<svg xmlns="http://www.w3.org/2000/svg" width="36" height="36" ' +
        'viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
        'stroke-linecap="round" stroke-linejoin="round" ' +
        'class="lucide lucide-loader-circle-icon lucide-loader-circle">' +
        '<path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>';

    function currentSrc(img) {
        return img.currentSrc || img.getAttribute("src") || "";
    }

    // setLoading toggles the animated lucide loader on the image's slot. The
    // host carries the spinner (an <img> can't render a ::after), shown while
    // the picture is unhydrated or still downloading and cleared once it paints.
    function setLoading(img, on) {
        var host = img.closest(".post_media_image");
        if (host) host.classList.toggle("media-loading", on);
    }

    // stripFrag drops the #... cache-buster doReload appends, so the polled
    // status path and reload base URL stay stable across retries.
    function stripFrag(url) {
        var i = url.indexOf("#");
        return i >= 0 ? url.slice(0, i) : url;
    }

    var observer = ("IntersectionObserver" in window)
        ? new IntersectionObserver(onIntersect, { rootMargin: "300px 0px" })
        : null;

    function onIntersect(entries) {
        entries.forEach(function (entry) {
            var st = entry.target.imageReloadState;
            if (!st || st.done) return;
            st.visible = entry.isIntersecting;
            if (st.visible) kick(st);
        });
    }

    // showSpinner hides the broken <img> and draws the animated loader over its
    // slot. Every style is set inline so it does not depend on style.css being
    // a fresh (non-cached) copy.
    function showSpinner(st) {
        var img = st.img;
        img.style.visibility = "hidden";

        var host = img.closest(".post_media_image") || img.parentNode;
        if (!host) return;
        st.host = host;
        st.prevPosition = host.style.position;
        st.prevMinHeight = host.style.minHeight;
        if (window.getComputedStyle(host).position === "static") {
            host.style.position = "relative";
        }
        // A broken <img> often collapses to no height — reserve a slot so the
        // centred spinner has somewhere to sit.
        if (!host.style.minHeight) {
            host.style.minHeight = "200px";
        }

        var box = document.createElement("span");
        box.className = "image-reload-spinner";
        box.setAttribute("role", "status");
        box.setAttribute("aria-label", "Loading image");
        box.style.cssText =
            "position:absolute;top:50%;left:50%;" +
            "transform:translate(-50%,-50%);display:flex;" +
            "pointer-events:none;z-index:2;color:var(--accent,#d54455);";
        box.innerHTML = SPINNER_SVG;
        host.appendChild(box);
        st.spinner = box;

        // Spin via the Web Animations API — no @keyframes / stylesheet needed.
        var svg = box.firstChild;
        if (svg && svg.animate) {
            st.anim = svg.animate(
                [{ transform: "rotate(0deg)" }, { transform: "rotate(360deg)" }],
                { duration: 800, iterations: Infinity, easing: "linear" }
            );
        }
    }

    // finish reveals the image and tears the spinner down — used both on a
    // successful reload and when the poll budget is spent (the image then falls
    // back to the browser's own broken-image icon, an honest "still blocked").
    function finish(st) {
        if (st.done) return;
        st.done = true;
        if (st.anim) {
            try { st.anim.cancel(); } catch (e) { /* ignore */ }
        }
        if (st.spinner && st.spinner.parentNode) {
            st.spinner.parentNode.removeChild(st.spinner);
        }
        st.img.style.visibility = "";
        if (st.host) {
            st.host.style.position = st.prevPosition || "";
            st.host.style.minHeight = st.prevMinHeight || "";
        }
        if (observer) observer.unobserve(st.img);
    }

    // doReload forces the browser to refetch this exact image. The #... suffix
    // is never sent upstream (pathToCDNURL ignores the fragment) but makes the
    // URL distinct enough that the <img> re-requests instead of clinging to its
    // failed state. The 503 was served no-store, so the refetch is fresh.
    function doReload(st) {
        st.polling = false; // the load/error event drives the next step now
        st.img.src = st.base + "#ir" + Date.now();
    }

    function poll(st) {
        if (st.done) return;
        if (st.polls >= MAX_POLLS) { finish(st); return; }
        // Viewport-gated: a poll only happens for images near the viewport.
        // When the user scrolls away the loop parks itself and onIntersect
        // restarts it once the image comes back into view.
        if (observer && !st.visible) { st.polling = false; return; }
        st.polls++;
        fetch("/api/media_status?path=" + encodeURIComponent(st.statusPath),
              { cache: "no-store" })
            .then(function (resp) { return resp.json(); })
            .then(function (data) {
                if (st.done) return;
                if (data.state === "ready") { doReload(st); return; }
                // "unsupported": this proxy path can't be polled at all.
                // "failed": upstream is permanently 404/410 (expired signed
                // Reddit CDN URL) — no amount of polling will recover it.
                // Both are terminal: drop the spinner and let the broken-image
                // icon stand in honestly instead of spinning for ~5 minutes.
                if (data.state === "unsupported" || data.state === "failed") {
                    finish(st);
                    return;
                }
                setTimeout(function () { poll(st); }, POLL_MS); // pending
            })
            .catch(function () {
                if (st.done) return;
                setTimeout(function () { poll(st); }, POLL_MS * 2);
            });
    }

    function kick(st) {
        if (st.done || st.polling) return;
        st.polling = true;
        poll(st);
    }

    function onError(img) {
        setLoading(img, false); // hand the slot over to the error/poll spinner
        var src = stripFrag(currentSrc(img));
        if (!src) return;
        var u;
        try { u = new URL(src, window.location.href); } catch (e) { return; }
        // Only RedMemo-proxied media can be polled for readiness.
        if (u.origin !== window.location.origin || !PROXY_RE.test(u.pathname)) {
            return;
        }

        var st = img.imageReloadState;
        if (!st) {
            st = img.imageReloadState = {
                img: img,
                base: src,
                statusPath: u.pathname + u.search,
                spinner: null,
                host: null,
                anim: null,
                polls: 0,
                visible: !observer,
                polling: false,
                done: false,
            };
            showSpinner(st);
            if (observer) observer.observe(img);
        }
        if (st.done) return;
        // First failure, or a reload attempt that failed again — (re)start the
        // poll loop so the next ready signal triggers another reload.
        if (st.visible) kick(st);
    }

    function onLoad(img) {
        setLoading(img, false); // pixels arrived — drop the loader, show the image
        var st = img.imageReloadState;
        if (st && !st.done) finish(st); // the reload (or a late load) succeeded
    }

    function watch(img) {
        if (img.imageReloadWatched) return;
        img.imageReloadWatched = true;
        img.addEventListener("error", function () { onError(img); });
        img.addEventListener("load", function () { onLoad(img); });
        if (img.complete && img.naturalWidth > 0) {
            // Already painted (served from cache before this ran) — no loader.
            setLoading(img, false);
        } else if (img.complete && img.naturalWidth === 0 && currentSrc(img)) {
            // Already broken before this script ran (image above the fold whose
            // request failed during initial page load).
            onError(img);
        } else {
            // A lazy slot still awaiting its src, or a download in flight: show
            // the animated loader until the load/error event resolves it.
            setLoading(img, true);
        }
    }

    function scan() {
        var imgs = document.querySelectorAll(".post_media_image img");
        for (var i = 0; i < imgs.length; i++) {
            watch(imgs[i]);
        }
    }

    function init() {
        scan();
        // Infinite scroll appends posts after load — watch them as they arrive.
        if (window.MutationObserver) {
            // Coalesce mutation bursts into one rescan per frame. Without this
            // every single childList mutation under <main> ran a full-document
            // querySelectorAll synchronously: infinite-scroll appends posts one
            // node at a time, and the page's own video/audio scripts (autoplay
            // pause/play, audio-sync notices) and this script's own spinner
            // add/remove all mutate <main> too. On a long, infinitely-scrolled
            // page that is an O(nodes × mutations) scan storm that locks the main
            // thread and freezes the whole page — videos included. The sibling
            // media scripts (lazyMedia/videoPreload/videoAutoplay/audioSync) all
            // debounce the same way.
            var scheduled = false;
            function scheduleScan() {
                if (scheduled) return;
                scheduled = true;
                window.requestAnimationFrame(function () {
                    scheduled = false;
                    scan();
                });
            }
            // Scope to <main> so nav/footer chrome mutations don't trigger
            // rescans; all post images (including infinite-scroll appends) are
            // inside it.
            var observeRoot = document.querySelector("main") || document.body;
            new MutationObserver(scheduleScan).observe(observeRoot, {
                childList: true,
                subtree: true,
            });
        }
    }

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", init);
    } else {
        init();
    }
})();
// @license-end
