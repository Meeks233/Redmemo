// @license http://www.gnu.org/licenses/agpl-3.0.html AGPL-3.0
// videoReload.js — recover videos that load a short partial then stall and
// never request the rest. Some Reddit CMAF/DASH clips arrive over a flaky
// upstream window where the browser fetches a few hundred KB, hits an error
// (H2 stream reset, decoder hiccup, idle-timeout abort) and then sits there
// forever — readyState stays low, the network panel shows no follow-up range
// request, and the play button does nothing. The user is left with a
// permanently-stuck poster frame.
//
// This script watches every <video>, notices the two classes of stall, and
// re-issues the load:
//   1. An `error` event — the browser already gave up; we must reissue.
//   2. A "no progress" stall — the video is meant to be playing (autoplay
//      candidate or user-played), readyState is below HAVE_FUTURE_DATA, the
//      buffered end is well short of the duration, and `currentTime` hasn't
//      advanced in STALL_S seconds. The browser hasn't fired `error` but
//      isn't fetching either; reloading kicks a fresh range request.
// Reloads preserve playback position and play-intent (so an autoplay video
// resumes itself). The retry counter is bounded so a genuinely broken file
// doesn't hammer the proxy forever.
(function () {
    "use strict";

    // Backoff between reloads: first retry is fast (the common case is one
    // transient blip), later ones space out to avoid hammering a truly broken
    // upstream. Capped at MAX_RETRIES total.
    var RETRY_DELAYS_MS = [800, 2000, 5000, 12000];
    var MAX_RETRIES = RETRY_DELAYS_MS.length;

    // How long currentTime must sit unchanged (with the player wanting to
    // play and buffer not yet at the end) before we declare a no-progress
    // stall. Below ~3s genuine network jitter and codec startup latency trip
    // false positives.
    var STALL_S = 4;

    // How often the stall watchdog samples currentTime. 1s is plenty — the
    // STALL_S threshold needs only a handful of samples.
    var WATCHDOG_INTERVAL_MS = 1000;

    // Treat the video as "almost fully buffered" within this slack. With
    // bufferedEnd within END_SLACK_S of duration there's nothing left to
    // fetch — a paused-at-end video is not a stall.
    var END_SLACK_S = 0.5;

    // showNotice posts a "Reconnecting video…" badge on the post chrome so
    // the viewer understands the spinner means we're retrying — not that
    // playback is just permanently broken. Reuses audioSync.js's notice
    // classes so the look matches; if audioSync is already showing one, we
    // stay quiet (its "Loading video…" already covers the same intent).
    function buildNotice(text) {
        var box = document.createElement("div");
        box.className = "audio-sync-notice";
        box.setAttribute("role", "status");
        var spinner = document.createElement("span");
        spinner.className = "audio-sync-spinner";
        var label = document.createElement("span");
        label.className = "audio-sync-text";
        label.textContent = text;
        box.appendChild(spinner);
        box.appendChild(label);
        return box;
    }

    function showNotice(video, st, text) {
        var host = video.closest(".post_media_content") || video.parentNode;
        if (!host) return;
        // audioSync.js owns the badge while it's running — don't double up.
        if (host.querySelector(".audio-sync-notice") && !st.notice) return;
        if (!st.notice) {
            st.notice = buildNotice(text);
            host.appendChild(st.notice);
            return;
        }
        st.notice.classList.remove("audio-sync-ready");
        var t = st.notice.querySelector(".audio-sync-text");
        if (t) t.textContent = text;
    }

    function flashNoticeReady(st, text, holdMs) {
        if (!st.notice) return;
        st.notice.classList.add("audio-sync-ready");
        var t = st.notice.querySelector(".audio-sync-text");
        if (t) t.textContent = text;
        var n = st.notice;
        st.notice = null;
        setTimeout(function () {
            if (n && n.parentNode) n.parentNode.removeChild(n);
        }, holdMs);
    }

    function clearNotice(st) {
        if (!st.notice) return;
        if (st.notice.parentNode) st.notice.parentNode.removeChild(st.notice);
        st.notice = null;
    }

    // stripFrag drops our #vr<n> cache-buster so we can re-append a fresh one
    // each retry without compounding the URL.
    function stripFrag(url) {
        var i = url.indexOf("#");
        return i >= 0 ? url.slice(0, i) : url;
    }

    function currentSrc(video) {
        // currentSrc reflects what the browser actually chose (works for both
        // direct src and child <source> elements). Falls back to the attribute
        // when the browser hasn't selected one yet.
        return video.currentSrc || video.getAttribute("src") || "";
    }

    // wantsToPlay distinguishes "user paused / waiting for autoplay gesture"
    // from "player wants frames now but isn't getting them". A truly paused
    // video isn't stalled — it's idle by design.
    function wantsToPlay(video) {
        if (video.ended) return false;
        // _userPaused / _programmaticPause are set by videoAutoplay.js. If the
        // user explicitly paused, leave the video alone.
        if (video._userPaused) return false;
        // paused + not seeking + no recent play intent → idle, not stalled.
        if (video.paused) return false;
        return true;
    }

    function bufferedEnd(video) {
        var b = video.buffered;
        if (!b || b.length === 0) return 0;
        return b.end(b.length - 1);
    }

    // needsMoreData: the player is past or near the end of its buffer and
    // there's more video left to fetch. Only then is a no-progress stretch
    // actually a stall — when bufferedEnd already covers the duration we're
    // just paused-at-end, not stuck.
    function needsMoreData(video) {
        var dur = video.duration;
        if (!isFinite(dur) || dur <= 0) {
            // Unknown duration (metadata not in yet): if the player is below
            // HAVE_FUTURE_DATA it's plausibly stuck — let the stall watchdog
            // decide based on currentTime not advancing.
            return video.readyState < 3;
        }
        return bufferedEnd(video) < dur - END_SLACK_S;
    }

    // doReload re-issues the network request for the same src by appending a
    // fresh fragment (which the proxy ignores) and calling load(). Position
    // and play intent are captured beforehand and restored on loadedmetadata.
    function doReload(video, st) {
        if (st.reloading) return;
        if (st.retries >= MAX_RETRIES) return;
        var src = stripFrag(currentSrc(video));
        if (!src) return;

        st.reloading = true;
        // Promote a scheduled notice to active "Reconnecting…" while bytes
        // are actually re-requested — viewer sees the spinner spinning over
        // a real fetch, not just a stale spinner from N seconds ago.
        showNotice(video, st, "Reconnecting video…");
        var resumeAt = video.currentTime || 0;
        // wantsToPlay() collapses to false the moment we set the new src
        // (paused becomes true while the element re-initialises), so capture
        // intent *before* touching the element.
        var resumePlay = wantsToPlay(video) || !video.paused;
        st.retries++;

        function onErr() {
            // The cache-busted reload itself failed (error fired instead of
            // loadedmetadata) — clear st.reloading so the bounded-retry path
            // isn't wedged true forever. The attach()-level error handler will
            // schedule the next retry (subject to the retry budget).
            video.removeEventListener("loadedmetadata", restore);
            video.removeEventListener("error", onErr);
            st.reloading = false;
        }
        function restore() {
            video.removeEventListener("loadedmetadata", restore);
            video.removeEventListener("error", onErr);
            try {
                if (resumeAt > 0 &&
                    (!video.duration || resumeAt < video.duration)) {
                    video.currentTime = resumeAt;
                }
            } catch (e) { /* seek not ready — ignore */ }
            if (resumePlay) {
                var p = video.play();
                if (p && p.catch) { p.catch(function () {}); }
            }
            st.reloading = false;
            st.lastTime = video.currentTime || 0;
            st.lastProgressAt = Date.now();
            // Mark the moment the reload landed — the watchdog uses this to
            // decide if subsequent forward progress means the retry actually
            // recovered playback (then we flash "Video ready" and clear).
            st.awaitingProgress = true;
            st.reloadedAt = Date.now();
        }
        video.addEventListener("loadedmetadata", restore);
        video.addEventListener("error", onErr);

        // Prefer setting the live src to a cache-busted form so the browser
        // refetches even if it would otherwise cling to the failed resource.
        // The fragment is dropped before the URL hits the proxy.
        var direct = video.getAttribute("src");
        if (direct) {
            video.setAttribute("src", stripFrag(direct) + "#vr" + Date.now());
        } else {
            // Child <source> form: bump every matching source.
            var sources = video.querySelectorAll("source");
            for (var i = 0; i < sources.length; i++) {
                var s = sources[i].getAttribute("src");
                if (s) {
                    sources[i].setAttribute("src",
                        stripFrag(s) + "#vr" + Date.now());
                }
            }
        }
        video.load();
    }

    function scheduleReload(video, st) {
        if (st.reloading || st.pendingReload) return;
        if (st.retries >= MAX_RETRIES) {
            // Out of retries — leave a terminal notice so the viewer knows
            // we've given up and the still frame isn't a transient state.
            showNotice(video, st, "Couldn't load video");
            return;
        }
        var delay = RETRY_DELAYS_MS[st.retries] || RETRY_DELAYS_MS[RETRY_DELAYS_MS.length - 1];
        st.pendingReload = true;
        // Surface the spinner immediately on detection so the viewer doesn't
        // sit in front of a frozen frame for the backoff delay wondering if
        // anything is happening.
        showNotice(video, st, "Reconnecting video…");
        setTimeout(function () {
            st.pendingReload = false;
            // Conditions may have changed by the time the timer fires (user
            // paused, video finished, src swapped by audioSync.js applyReady).
            // Re-check before pulling the trigger.
            if (st.detached) return;
            if (video.ended) return;
            // Successful reload bumps lastProgressAt; if currentTime has
            // moved since we scheduled, the player recovered on its own.
            if ((video.currentTime || 0) > st.scheduleTime + 0.05) return;
            doReload(video, st);
        }, delay);
        st.scheduleTime = video.currentTime || 0;
    }

    // tickWatchdog samples progress at WATCHDOG_INTERVAL_MS. The actual
    // decision lives here so error-driven reloads and timer-driven reloads
    // share the same backoff/retry state.
    function tickWatchdog(video, st) {
        if (st.detached || video.ended) return;
        var now = Date.now();
        var t = video.currentTime || 0;
        if (t > st.lastTime + 0.01) {
            // Forward progress — reset the stall clock and forgive earlier
            // retries: a long-playing video that hits a single transient
            // glitch should not exhaust its retry budget.
            st.lastTime = t;
            st.lastProgressAt = now;
            if (st.awaitingProgress) {
                // The viewer is seeing fresh frames after our reload — flash
                // the success state and clear the badge after a moment.
                st.awaitingProgress = false;
                flashNoticeReady(st, "Video ready", 2500);
            }
            if (st.retries > 0 && t > st.lastResetTime + 5) {
                st.retries = 0;
                st.lastResetTime = t;
            }
            return;
        }
        if (!wantsToPlay(video)) {
            // Idle by design — keep the timestamps fresh so a later resume
            // doesn't see ancient lastProgressAt as instantly-stalled.
            st.lastProgressAt = now;
            return;
        }
        if (!needsMoreData(video)) {
            // Buffer already covers the rest of the clip — not a fetch stall.
            st.lastProgressAt = now;
            return;
        }
        if (now - st.lastProgressAt < STALL_S * 1000) return;
        // No progress, wants to play, more data to fetch, threshold exceeded —
        // this is exactly the partial-then-stuck case. Schedule a reload.
        scheduleReload(video, st);
    }

    function attach(video) {
        if (video._videoReloadWatched) return;
        video._videoReloadWatched = true;
        var st = {
            retries: 0,
            lastTime: 0,
            lastResetTime: 0,
            lastProgressAt: Date.now(),
            scheduleTime: 0,
            reloading: false,
            pendingReload: false,
            detached: false,
        };
        video._videoReloadState = st;

        // The `error` event is the loud signal — the browser has surfaced a
        // MediaError and will not retry on its own. Schedule a reload right
        // away (subject to the retry budget).
        video.addEventListener("error", function () {
            // Some sources (audioSync applyReady) deliberately swap src and
            // briefly clear the element — that fires error too. The watchdog
            // would catch a real stall anyway; only act here if there is a
            // concrete MediaError attached.
            if (!video.error) return;
            scheduleReload(video, st);
        });

        // `stalled` / `suspend` alone aren't reliable stall signals (Chrome
        // fires `stalled` for any quiet 3s, including normal end-of-buffer
        // pauses). We use the time-progression watchdog instead, but reset
        // the progress timestamp on these events so they don't muddy it.
        video.addEventListener("playing", function () {
            st.lastTime = video.currentTime || 0;
            st.lastProgressAt = Date.now();
        });
        video.addEventListener("progress", function () {
            // Bytes are arriving — give the player room to consume them.
            st.lastProgressAt = Date.now();
        });

        // Cheap interval timer: a watchdog per video at 1Hz is well under any
        // measurable cost even on a page with dozens of videos. Stops when
        // the element leaves the DOM.
        st.timer = setInterval(function () {
            if (!video.isConnected) {
                st.detached = true;
                clearNotice(st);
                clearInterval(st.timer);
                return;
            }
            tickWatchdog(video, st);
        }, WATCHDOG_INTERVAL_MS);
    }

    function scan() {
        // Exclude link-preview videos — external, never-cached embeds owned by
        // linkPreview.js; the reddit reload/mux machinery must not attach to them.
        var videos = document.querySelectorAll("video:not(.link-preview-media)");
        for (var i = 0; i < videos.length; i++) {
            attach(videos[i]);
        }
    }

    function init() {
        scan();
        if (window.MutationObserver) {
            // Coalesce mutation bursts the same way every other media script
            // does — a per-mutation full-document scan on an infinitely
            // scrolled page locks the main thread.
            var scheduled = false;
            function scheduleScan() {
                if (scheduled) return;
                scheduled = true;
                window.requestAnimationFrame(function () {
                    scheduled = false;
                    scan();
                });
            }
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
