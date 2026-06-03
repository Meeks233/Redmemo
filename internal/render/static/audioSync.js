// @license http://www.gnu.org/licenses/agpl-3.0.html AGPL-3.0
// audioSync.js — for every v.redd.it DASH/CMAF video on the page, ask the
// server whether its audio track has been muxed in yet. v.redd.it serves
// video and audio as separate streams; muxing them takes a few seconds (and
// requires the full video to land on disk first), so while we wait we pair
// the silent <video> with a hidden <audio> companion fetched separately via
// /api/audio_track. The audio file is only a few hundred KB, so the viewer
// hears sound almost immediately while the much larger video continues to
// stream/lazy-load. Once the server finishes muxing the single combined mp4
// we tear the companion down and reload the <video> in place to pick up the
// canonical muxed copy (which carries its own audio).
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

    // Drift tolerance between the video clock and the companion audio clock.
    // Two separate media elements never share a sample clock, so they will
    // drift by a few tens of ms over time. Nudging on every timeupdate (~250
    // ms) is enough; anything tighter is audible as a stutter.
    var SYNC_DRIFT_S = 0.18;

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
        // Companion <audio> playing the standalone DASH_AUDIO_*.mp4 track for
        // this video. Created on the first "pending" poll, removed when the
        // server-side mux completes (applyReady) or the video turns out to
        // have no audio (clearNotice on "silent").
        var audioEl = null;
        var syncListeners = null;
        function attachAudioCompanion() {
            if (audioEl) return;
            audioEl = document.createElement("audio");
            audioEl.preload = "auto";
            audioEl.src = "/api/audio_track?src=" + encodeURIComponent(src);
            audioEl.style.display = "none";
            // The companion plays the real audio track, so its mute/volume
            // must mirror the <video> the viewer sees — including the
            // template's defaultMuted attribute (set from the "Mute all" /
            // "Mute NSFW" preferences). Don't force the <video> muted: the
            // DASH/CMAF video-only segment has no audio track to leak, and
            // forcing it would override the user's chosen sound mode.
            audioEl.muted = video.muted;

            function safePlay(el) {
                var p = el.play();
                if (p && p.catch) { p.catch(function () {}); }
            }
            function syncPlay() {
                if (!audioEl) return;
                try { audioEl.currentTime = video.currentTime || 0; }
                catch (e) { /* not seekable yet */ }
                safePlay(audioEl);
            }
            function syncPause() {
                if (audioEl) { audioEl.pause(); }
            }
            function syncSeek() {
                if (!audioEl) return;
                try { audioEl.currentTime = video.currentTime || 0; }
                catch (e) { /* ignore */ }
            }
            function syncRate() {
                if (audioEl) { audioEl.playbackRate = video.playbackRate; }
            }
            function nudge() {
                if (!audioEl || audioEl.paused) return;
                var drift = (video.currentTime || 0) - (audioEl.currentTime || 0);
                if (drift > SYNC_DRIFT_S || drift < -SYNC_DRIFT_S) {
                    try { audioEl.currentTime = video.currentTime || 0; }
                    catch (e) { /* ignore */ }
                }
            }
            function syncVolume() {
                if (!audioEl) return;
                audioEl.volume = video.volume;
                // Mirror mute toggles so the user clicking the <video>'s
                // mute control silences the companion in lockstep.
                audioEl.muted = video.muted;
            }

            syncListeners = {
                play: syncPlay,
                pause: syncPause,
                seeking: syncSeek,
                seeked: syncSeek,
                ratechange: syncRate,
                timeupdate: nudge,
                waiting: syncPause,
                playing: syncPlay,
                ended: syncPause,
                volumechange: syncVolume,
            };
            for (var ev in syncListeners) {
                video.addEventListener(ev, syncListeners[ev]);
            }
            audioEl.playbackRate = video.playbackRate;
            audioEl.volume = video.volume;

            // Once the companion has enough audio buffered to play, the
            // viewer is already hearing sound — drop the "Loading audio…"
            // notice even though the background mux is still running. The
            // poller stays alive so applyReady can later swap to the single
            // muxed mp4, but it no longer needs UI to announce that.
            audioEl.addEventListener("canplay", function () {
                if (notice && notice.parentNode) {
                    notice.parentNode.removeChild(notice);
                }
                notice = null;
            });

            (video.parentNode || document.body).appendChild(audioEl);

            // Video may already be playing when the companion attaches
            // (autoplay listings) — get audio in sync straight away.
            if (!video.paused && !video.ended) {
                syncPlay();
            }
        }

        function detachAudioCompanion() {
            if (!audioEl) return;
            for (var ev in syncListeners) {
                video.removeEventListener(ev, syncListeners[ev]);
            }
            syncListeners = null;
            try {
                audioEl.pause();
                // Firefox holds onto already-buffered audio after removeChild
                // unless we also drop the source and reset the element — the
                // muxed <video> would then play sound through the dangling
                // companion, defeating the user's mute preference.
                audioEl.removeAttribute("src");
                while (audioEl.firstChild) {
                    audioEl.removeChild(audioEl.firstChild);
                }
                audioEl.load();
            } catch (e) { /* ignore */ }
            if (audioEl.parentNode) {
                audioEl.parentNode.removeChild(audioEl);
            }
            audioEl = null;
        }

        function showPending() {
            if (!notice) {
                notice = buildNotice();
                var host = video.closest(".post_media_content") || video.parentNode;
                host.appendChild(notice);
            }
            // The audio_status poll returns "pending" until the server-side
            // mux completes — and the mux is gated on the FULL video landing
            // on disk, not on the (tiny) audio probe. So while this notice is
            // up the bottleneck is the video download, not the audio. The
            // separate <audio> companion is already streaming sound in
            // parallel; the canplay handler above tears the notice down the
            // moment audible bytes are buffered, which usually beats the
            // video by a wide margin.
            notice.querySelector(".audio-sync-text").textContent =
                "Loading video…";
        }

        // applyReady swaps the silent stand-in for the freshly muxed audio
        // copy by reloading just this <video> in place — no full page reload.
        // The silent response was served no-store, so video.load() re-requests
        // and the server now returns the muxed file. Playback position and
        // play/pause state are preserved across the swap. The companion
        // <audio> is torn down first so it doesn't double up with the audio
        // baked into the muxed file.
        function applyReady() {
            // No notice means the video was already muxed when the page
            // loaded — it is playing the audio copy already, nothing to swap.
            if (!notice && !audioEl) {
                return;
            }

            var resumeAt = video.currentTime || 0;
            var wasPlaying = !video.paused && !video.ended;
            // The muxed file carries its own audio track. Reset the <video>
            // back to the template's chosen sound mode (defaultMuted reflects
            // the "Mute all" / "Mute NSFW" preferences) so the swap doesn't
            // leave the viewer with a different mute state than every other
            // video on the page.
            var preferredMuted = video.defaultMuted;

            detachAudioCompanion();

            function restore() {
                video.removeEventListener("loadedmetadata", restore);
                try {
                    if (resumeAt > 0 && (!video.duration || resumeAt < video.duration)) {
                        video.currentTime = resumeAt;
                    }
                } catch (e) { /* seeking not ready — ignore */ }
                // Keep the muted DOM attribute in lockstep with the property.
                // Firefox's built-in <video> controls take the icon state
                // from the attribute on a fresh src; if the property and the
                // attribute disagree the toolbar shows the mute icon while
                // audio is actually playing (or vice-versa).
                video.muted = preferredMuted;
                if (preferredMuted) {
                    video.setAttribute("muted", "");
                } else {
                    video.removeAttribute("muted");
                }
                if (wasPlaying) {
                    var p = video.play();
                    if (p && p.catch) { p.catch(function () {}); }
                }
            }
            video.addEventListener("loadedmetadata", restore);
            video.load();

            // Always announce the swap so the viewer knows the full clip is
            // now playing from the cached muxed copy (no longer streaming
            // live from the CDN). If the canplay handler already tore the
            // "Loading…" notice down because the companion audio landed
            // first, build a fresh one here.
            if (!notice) {
                notice = buildNotice();
                var host = video.closest(".post_media_content") || video.parentNode;
                if (host) { host.appendChild(notice); }
            }
            notice.classList.add("audio-sync-ready");
            notice.querySelector(".audio-sync-text").textContent = "Video ready";
            var done = notice;
            notice = null;
            setTimeout(function () {
                if (done && done.parentNode) {
                    done.parentNode.removeChild(done);
                }
            }, 4000);
        }

        // overlayCard hides the spinning <video> and drops a card with the
        // given SVG icon and copy in its place. Shared between the soft
        // (question-mark) and terminal (X) presentations; the loop is parked
        // either way because both states forbid further polling — a revival,
        // when allowed, comes from the user reloading the post page.
        function overlayCard(iconPaths, title, hint) {
            var host = video.closest(".post_media_content") || video.parentNode;
            if (!host) return;
            try { video.pause(); } catch (e) { /* ignore */ }
            video.style.display = "none";
            var card = document.createElement("div");
            card.className = "media-unavailable-card";
            card.setAttribute("role", "status");
            var paths = "";
            for (var i = 0; i < iconPaths.length; i++) {
                paths += '<path d="' + iconPaths[i] + '"/>';
            }
            card.innerHTML =
                '<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" ' +
                'viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
                'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
                paths + '</svg>' +
                '<p class="media-unavailable-text">' + title + '</p>' +
                '<p class="media-unavailable-hint">' + hint + '</p>';
            host.appendChild(card);
            polls = MAX_POLLS;
        }

        // showUncertain: soft refusal — ledger said no for now, but a fresh
        // page load (which calls Revive) might bring it back. lucide
        // circle-question-mark glyph.
        function showUncertain() {
            overlayCard(
                ["M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3", "M12 17h.01"],
                "Media temporarily unavailable",
                "Reopen the post to retry"
            );
            // The question-mark glyph wraps an outer circle — add it separately
            // because overlayCard's path list models stroked paths, not circles.
            var card = video.parentNode && video.parentNode.querySelector(".media-unavailable-card svg");
            if (card) {
                card.insertAdjacentHTML("afterbegin",
                    '<circle cx="12" cy="12" r="10"/>');
            }
        }

        // showDead: terminal refusal — the ledger has already burned a
        // user-triggered retry. lucide X glyph + "Sorry, we missed it…".
        function showDead() {
            overlayCard(
                ["M18 6 6 18", "m6 6 12 12"],
                "Sorry, we missed it…",
                "Reddit removed this before we could archive it"
            );
        }

        function clearNotice() {
            detachAudioCompanion();
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
                    if (data.state === "unavailable") {
                        // Soft state: refused N times but the user reopening
                        // the post can still revive. Question-mark card +
                        // hint to retry.
                        clearNotice();
                        showUncertain();
                        return;
                    }
                    if (data.state === "dead") {
                        // Terminal: the URL already burned a user-triggered
                        // retry and Reddit said no again. No more requests
                        // ever; "Sorry, we missed it" with the X glyph.
                        clearNotice();
                        showDead();
                        return;
                    }
                    // pending — attach the companion (idempotent) so audio
                    // bytes start landing in parallel with the video.
                    attachAudioCompanion();
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
        // Coalesce mutation bursts into a single rescan per frame: appending a
        // notice (showPending) is itself a childList mutation, and on a large
        // infinitely-scrolled page a per-mutation full-document scan freezes it.
        if (window.MutationObserver) {
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
            // rescans; all videos (including infinite-scroll appends) are inside it.
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
