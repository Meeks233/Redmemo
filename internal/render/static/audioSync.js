// @license http://www.gnu.org/licenses/agpl-3.0.html AGPL-3.0
// audioSync.js — for every v.redd.it DASH/CMAF video on the page, ask the
// server whether its audio track has been muxed in yet. v.redd.it serves
// video and audio as separate streams; muxing them takes a few seconds, so a
// freshly-seen video is served silent first. While the mux runs we show a
// small notice under the video; once the audio is ready we reload just that
// <video> in place (preserving playback position) so it picks up the sound
// without a full page reload.
//
// Online listings lazy-load video — lazyMedia.js defers the URL to data-src
// and only sets the real src once the post nears the viewport — and infinite
// scroll appends posts after load. So a one-shot scan misses everything: we
// watch each <video> and start tracking it the moment it actually loads.
(function () {
    "use strict";

    // /vid/<id>/(DASH|CMAF)_<height>.mp4 — the muxable video-only segment.
    var MUXABLE = /\/vid\/[^/]+\/(?:DASH|CMAF)_\d+\.mp4(?:\?|$)/;

    var POLL_MS = 2000;
    var MAX_POLLS = 150; // ~5 min ceiling

    var tracked = ("WeakSet" in window) ? new WeakSet() : null;
    var trackedList = [];

    function isTracked(video) {
        return tracked ? tracked.has(video) : trackedList.indexOf(video) !== -1;
    }
    function markTracked(video) {
        if (tracked) {
            tracked.add(video);
        } else {
            trackedList.push(video);
        }
    }

    // muxableSrc returns the muxable /vid/ URL currently live on a <video> —
    // its src attribute or a <source src>. data-src is deliberately ignored:
    // a lazy video is only tracked once lazyMedia.js has hydrated it.
    function muxableSrc(video) {
        var direct = video.getAttribute("src");
        if (direct && MUXABLE.test(direct)) {
            return direct;
        }
        var sources = video.querySelectorAll("source");
        for (var i = 0; i < sources.length; i++) {
            var s = sources[i].getAttribute("src");
            if (s && MUXABLE.test(s)) {
                return s;
            }
        }
        return null;
    }

    function buildNotice() {
        var box = document.createElement("div");
        box.className = "audio-sync-notice";
        box.setAttribute("role", "status");
        var spinner = document.createElement("span");
        spinner.className = "audio-sync-spinner";
        var text = document.createElement("span");
        text.className = "audio-sync-text";
        box.appendChild(spinner);
        box.appendChild(text);
        return box;
    }

    function track(video, src) {
        var notice = null;
        var polls = 0;

        function showPending() {
            if (!notice) {
                notice = buildNotice();
                var host = video.closest(".post_media_content") || video.parentNode;
                host.appendChild(notice);
            }
            notice.querySelector(".audio-sync-text").textContent =
                "Video has audio, downloading…";
        }

        // applyReady swaps the silent stand-in for the freshly muxed audio
        // copy by reloading just this <video> in place — no full page reload.
        // The silent response was served no-store, so video.load() re-requests
        // and the server now returns the muxed file. Playback position and
        // play/pause state are preserved across the swap.
        function applyReady() {
            // No notice means the video was already muxed when the page
            // loaded — it is playing the audio copy already, nothing to swap.
            if (!notice) {
                return;
            }

            var resumeAt = video.currentTime || 0;
            var wasPlaying = !video.paused && !video.ended;

            function restore() {
                video.removeEventListener("loadedmetadata", restore);
                try {
                    if (resumeAt > 0 && (!video.duration || resumeAt < video.duration)) {
                        video.currentTime = resumeAt;
                    }
                } catch (e) { /* seeking not ready — ignore */ }
                if (wasPlaying) {
                    var p = video.play();
                    if (p && p.catch) { p.catch(function () {}); }
                }
            }
            video.addEventListener("loadedmetadata", restore);
            video.load();

            notice.classList.add("audio-sync-ready");
            notice.querySelector(".audio-sync-text").textContent = "Audio synced";
            var done = notice;
            notice = null;
            setTimeout(function () {
                if (done && done.parentNode) {
                    done.parentNode.removeChild(done);
                }
            }, 4000);
        }

        function clearNotice() {
            if (notice && notice.parentNode) {
                notice.parentNode.removeChild(notice);
            }
            notice = null;
        }

        function poll() {
            fetch("/api/audio_status?src=" + encodeURIComponent(src), { cache: "no-store" })
                .then(function (resp) { return resp.json(); })
                .then(function (data) {
                    if (data.state === "ready") {
                        applyReady();
                        return;
                    }
                    if (data.state === "silent" || data.state === "unsupported") {
                        clearNotice();
                        return;
                    }
                    // pending
                    showPending();
                    polls++;
                    if (polls < MAX_POLLS) {
                        setTimeout(poll, POLL_MS);
                    }
                })
                .catch(function () {
                    polls++;
                    if (polls < MAX_POLLS) {
                        setTimeout(poll, POLL_MS * 2);
                    }
                });
        }

        poll();
    }

    // consider starts tracking a <video> once it has a real muxable src.
    function consider(video) {
        if (isTracked(video)) {
            return;
        }
        var src = muxableSrc(video);
        if (src) {
            markTracked(video);
            track(video, src);
        }
    }

    // watch wires a <video> up exactly once: track it now if it already has a
    // real src (non-lazy listings, post pages), otherwise pick it up on
    // loadstart — which fires when lazyMedia.js hydrates data-src and calls
    // video.load().
    function watch(video) {
        if (video.audioSyncWatched) {
            return;
        }
        video.audioSyncWatched = true;
        video.addEventListener("loadstart", function () { consider(video); });
        consider(video);
    }

    function scan() {
        var videos = document.querySelectorAll("video");
        for (var i = 0; i < videos.length; i++) {
            watch(videos[i]);
        }
    }

    function init() {
        scan();
        // Infinite scroll appends posts after load — watch them as they arrive.
        if (window.MutationObserver) {
            new MutationObserver(scan).observe(document.body, {
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
