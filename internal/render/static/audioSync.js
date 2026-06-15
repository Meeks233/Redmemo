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

    // /vid/<id>/(DASH|CMAF)_<height>[.mp4] — the muxable video-only segment.
    // .mp4 is optional: 2019-era Reddit uploads serve the segments bare (e.g.
    // /vid/y7nhn25qior31/DASH_720?source=fallback) with audio at the sibling
    // /vid/<id>/audio path. Server-side IsMuxableVideoSegment matches both
    // shapes; this client regex must match the same set or audioSync.js skips
    // the video entirely and the "Loading audio…" notice never appears.
    var MUXABLE = /\/vid\/[^/]+\/(?:DASH|CMAF)_\d+(?:\.mp4)?(?:\?|$)/;

    var POLL_MS = 2000;
    var MAX_POLLS = 150; // ~5 min ceiling

    // Drift tolerance between the video clock and the companion audio clock.
    // Two separate media elements never share a sample clock, so they will
    // drift by a few tens of ms over time. Nudging on every timeupdate (~250
    // ms) is enough; anything tighter is audible as a stutter.
    var SYNC_DRIFT_S = 0.18;

    // If the companion <audio> only reaches `canplay` after the silent <video>
    // has been on screen for more than this many seconds, rewind both back to
    // 0 so the viewer gets the clip from the start with sound. Inside the
    // window, just letting sync land where it lands is less disruptive than a
    // surprise rewind.
    var LATE_AUDIO_REWIND_S = 3;

    // Maximum silent <video> playback time before audioSync.js parks playback
    // to yield wire bandwidth to the still-loading companion <audio>. With a
    // 5 MB/s global media bandwidth budget (see internal/media/bwlimit.go) and
    // an 11–60s video at 2–8 Mbps, continuous buffering steals the upstream
    // slot the (tiny) audio file needs to finish — pushing the rewind window
    // out so far that the user watches a third of the clip silently before
    // sound lands. Pausing at this threshold lets audio land in a couple of
    // seconds, then the canplay handler — or applyReady once the muxed file
    // is in — rewinds to 0 and resumes.
    var AUDIO_WAIT_PAUSE_S = 6;

    var tracked = ("WeakSet" in window) ? new WeakSet() : null;
    var trackedList = [];

    // Viewport gating: without it every muxable video on the page runs a
    // 2s-interval polling loop AND keeps a hidden <audio> companion buffering
    // its full track for up to 5 minutes, regardless of whether the post is on
    // screen. On an infinite-scroll feed that piles up to dozens of concurrent
    // fetch chains and audio decoders — enough to freeze the browser by ~15
    // videos. Mirrors imageReload.js's visible-gated poll pattern.
    var visObserver = ("IntersectionObserver" in window)
        ? new IntersectionObserver(function (entries) {
            entries.forEach(function (entry) {
                var v = entry.target;
                if (entry.isIntersecting) {
                    if (v._audioSyncResume) v._audioSyncResume();
                } else {
                    if (v._audioSyncPause) v._audioSyncPause();
                }
            });
        }, { rootMargin: "300px 0px" })
        : null;

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
        // Viewport-gated poll loop state. `visible` is the latest IO verdict;
        // `pollTimer` is the live setTimeout handle (so a pause can cancel it);
        // `pollPaused` means the poll loop parked itself because the video
        // scrolled off-screen and onIntersect will restart it. `done` shuts the
        // whole machine down for terminal states.
        var visible = visObserver ? false : true;
        var pollTimer = null;
        // pollPaused tracks "the loop wants to run but is parked waiting for
        // visibility". It must agree with `visible` at init: with an IO present
        // we start unseen, so the loop IS parked from t=0 — otherwise the IO's
        // first isIntersecting=true delivery sees pollPaused=false and the
        // resume gate refuses to kick off polling. Result: a video that
        // autoplays (play → started=true) before the IO callback yields
        // deadlocks — neither path ever calls poll(), and the "Loading audio…"
        // notice + companion <audio> never attach until the user scrolls the
        // post out and back in.
        var pollPaused = visObserver ? true : false;
        var done = false;
        // Focus gating, narrowed. With N videos visible at once (search
        // listing shows 5 per page), kicking the heavy /api/audio_track
        // companion fetch for all of them races the server's muxSem in
        // parallel — the bottom video (often the smallest file) lands audio
        // bytes first instead of the focused one.
        //
        // The earlier fix gated the WHOLE poll loop on play, but that left
        // non-centermost visible videos with `started=false` forever:
        // videoAutoplay only plays the centermost, the others never receive
        // a `play` event, and their /api/audio_status poll never starts. So
        // applyReady never fires for them either, and the viewer sees the
        // silent fallback indefinitely — exactly the "only the first video
        // gets the loading badge" report.
        //
        // Split the two concerns: the cheap status poll runs for any visible
        // video (status JSON, no muxSem cost); the expensive companion
        // <audio> only attaches once the video has actually played. The
        // notice ("Loading video…") shows on either path so the viewer sees
        // progress on every visible clip.
        var played = false;
        // Once the companion <audio> reaches `canplay` the viewer is
        // already hearing sound from this video. A later "unavailable"
        // verdict from /api/audio_status is the persistent ledger flapping
        // mid-mux (e.g. a parallel L5 retry tripping the failure counter);
        // it must not yank the working playback and overlay the
        // question-mark card. Only "dead" terminates audible playback.
        var companionLive = false;
        // audioWaitArmed: one-shot guard. After the bandwidth-yield pause
        // fires once, we don't fight a user who explicitly resumes silent
        // playback — they've made their choice.
        // pausedForAudio: set when *we* paused the video, so the canplay
        // handler knows it owes a safePlay to resume what we interrupted.
        var audioWaitArmed = true;
        var pausedForAudio = false;
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
            // The <video> failing to decode (corrupt mp4, fallback source
            // 404, MSE/codec refusal) renders the browser's broken-media
            // glyph but does NOT pause the element — paused stays false and
            // no "pause" event fires, so syncPause never runs. Without an
            // explicit error hook the companion <audio> (an independent
            // stream off /api/audio_track) keeps playing under the corrupt
            // placeholder, which is jarring. Tear the companion down so
            // sound dies with the picture.
            function syncError() {
                if (audioEl) { audioEl.pause(); }
                detachAudioCompanion();
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
                error: syncError,
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
                // `canplay` is NOT one-shot: while the companion's track is
                // still streaming in (rate-limited, sharing the wire with the
                // live silent video), the audio element repeatedly drains its
                // buffer and re-fills it, dropping below HAVE_FUTURE_DATA and
                // climbing back — firing `canplay` again every time. The
                // late-audio rewind below MUST run only on the first such
                // event; otherwise every buffer-recovery during the download
                // yanks playback back to 0:00 over and over (the user-visible
                // "video keeps restarting while loading" bug). companionLive
                // latches the first canplay, so capture its prior value as the
                // first-fire gate before flipping it.
                var firstCanplay = !companionLive;
                companionLive = true;
                if (notice && notice.parentNode) {
                    notice.parentNode.removeChild(notice);
                }
                notice = null;
                // Late-arriving audio: companion didn't buffer fast enough,
                // so the silent <video> has already been playing for more
                // than LATE_AUDIO_REWIND_S. Cutting sound in mid-clip is
                // jarring ("why is there suddenly audio?") and the viewer
                // missed the opening. Rewind to 0 so the clip plays once
                // properly with sound. Below the threshold the normal
                // syncPlay+nudge path lines audio up in place — no rewind
                // needed for a clip that just barely started.
                if (firstCanplay && !video.ended &&
                    (video.currentTime || 0) > LATE_AUDIO_REWIND_S) {
                    try { video.currentTime = 0; }
                    catch (e) { /* seeking not ready — ignore */ }
                    try { audioEl.currentTime = 0; }
                    catch (e) { /* ignore */ }
                    // If the viewer had it playing, keep it playing from
                    // the new position so they don't have to click again.
                    // Also resume when *we* parked it via the audio-wait
                    // watchdog below — the whole point of that pause was to
                    // hand control back here. syncPlay (wired into the
                    // video's `play` listener) restarts the companion in
                    // lockstep.
                    if (!video.paused || pausedForAudio) { safePlay(video); }
                }
                pausedForAudio = false;
            });

            // Bandwidth-yield watchdog: with the companion still buffering,
            // letting the silent <video> race past AUDIO_WAIT_PAUSE_S burns
            // the wire that audio needs to finish. Park playback at the
            // threshold so audio can land in a couple of seconds; the canplay
            // handler above rewinds and resumes. One-shot — once disarmed
            // (whether by canplay or by us pausing), we don't fight a user
            // who explicitly hits play on the silent video.
            function audioWaitWatchdog() {
                if (!audioWaitArmed || companionLive || done) return;
                if (video.paused || video.ended) return;
                if ((video.currentTime || 0) >= AUDIO_WAIT_PAUSE_S) {
                    audioWaitArmed = false;
                    pausedForAudio = true;
                    try { video.pause(); } catch (e) { /* ignore */ }
                    if (notice) {
                        var t = notice.querySelector(".audio-sync-text");
                        if (t) { t.textContent = "Waiting for audio…"; }
                    }
                }
            }
            video.addEventListener("timeupdate", audioWaitWatchdog);
            // Once audio is buffered (companionLive) the canplay handler
            // tears the notice down and we no longer need the watchdog —
            // but keep the listener installed; it's a cheap guard and
            // self-disables on the companionLive check.

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
            // Edge: the bandwidth-yield watchdog may have parked playback
            // earlier, then the viewer scrolled the post off-screen which
            // ran detachAudioCompanion (clearing audioEl) and clearNotice'd
            // anything visible. By the time ready lands, both are nil but
            // the video is still paused for an audio reason we promised to
            // unpause once audio was in. Honor that, then early-return.
            if (!notice && !audioEl) {
                if (pausedForAudio && !video.ended) {
                    pausedForAudio = false;
                    var p = video.play();
                    if (p && p.catch) { p.catch(function () {}); }
                }
                return;
            }

            var resumeAt = video.currentTime || 0;
            // wasPlaying treats a JS-induced pause (pausedForAudio — see
            // audioWaitWatchdog) as "the viewer wanted this playing". Without
            // the OR clause, a video parked by the bandwidth-yield watchdog
            // stays paused forever after the muxed copy lands: the viewer
            // explicitly hit play, we explicitly paused them, and the swap
            // owes that play back. The companion-canplay path already does
            // the same; this is the muxed-file analogue.
            var wasPlaying = (!video.paused && !video.ended) || pausedForAudio;
            pausedForAudio = false;
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
            pollTimer = null;
            if (done) return;
            // Park the loop while off-screen — onIntersect will resume it.
            if (!visible) { pollPaused = true; return; }
            pollPaused = false;
            fetch("/api/audio_status?src=" + encodeURIComponent(src), { cache: "no-store" })
                .then(function (resp) { return resp.json(); })
                .then(function (data) {
                    if (done) return;
                    if (data.state === "ready") {
                        done = true;
                        applyReady();
                        return;
                    }
                    if (data.state === "silent" || data.state === "unsupported") {
                        done = true;
                        clearNotice();
                        return;
                    }
                    if (data.state === "unavailable") {
                        // Soft refusal from the persistent ledger. If the
                        // companion is already playing audible bytes the
                        // viewer's experience is fine — the ledger flap is
                        // an internal mux retry, not user-facing breakage.
                        // Keep polling: either the mux completes ("ready")
                        // or escalates to terminal "dead".
                        if (companionLive) {
                            polls++;
                            if (polls < MAX_POLLS && visible) {
                                pollTimer = setTimeout(poll, POLL_MS);
                            } else if (polls < MAX_POLLS) {
                                pollPaused = true;
                            }
                            return;
                        }
                        done = true;
                        clearNotice();
                        showUncertain();
                        return;
                    }
                    if (data.state === "dead") {
                        done = true;
                        clearNotice();
                        showDead();
                        return;
                    }
                    // pending — show the notice for any visible video, but
                    // only attach the heavy companion <audio> for ones that
                    // actually played (focus gate, see `played` comment).
                    if (played) {
                        attachAudioCompanion();
                    }
                    showPending();
                    polls++;
                    if (polls < MAX_POLLS && visible) {
                        pollTimer = setTimeout(poll, POLL_MS);
                    } else if (polls < MAX_POLLS) {
                        pollPaused = true;
                    }
                })
                .catch(function () {
                    if (done) return;
                    polls++;
                    if (polls < MAX_POLLS && visible) {
                        pollTimer = setTimeout(poll, POLL_MS * 2);
                    } else if (polls < MAX_POLLS) {
                        pollPaused = true;
                    }
                });
        }

        // IntersectionObserver hooks: when the post scrolls off-screen, cancel
        // the next poll tick AND tear the companion <audio> down. Letting it
        // "coast" (the earlier behavior) accumulated one preload="auto" audio
        // element per video the user had ever scrolled past — on a 25-post
        // search page of video hits that's 25 parallel audio buffers plus 25
        // sets of sync event listeners, enough to freeze the browser as the
        // page loads and IO fires for many videos at once. The next pending
        // poll re-attaches the companion when the video scrolls back into view.
        video._audioSyncPause = function () {
            if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
            pollPaused = true;
            visible = false;
            detachAudioCompanion();
            companionLive = false;
        };
        video._audioSyncResume = function () {
            visible = true;
            if (done) return;
            // Kick off (or resume) polling whenever this video becomes
            // visible and no tick is already in flight. The status poll runs
            // for every visible muxable video — the focus gate now lives on
            // companion attach, not on the poll loop.
            if (!pollTimer) {
                pollPaused = false;
                poll();
            }
        };

        // `play` flips the focus gate so the companion <audio> can attach on
        // the next pending poll — autoplay hands focus to the centermost
        // clip first, so its companion wins the muxSem race over the rest.
        // If the poll loop already saw "pending" before play (visible but
        // unfocused), we don't have a notice yet either; the next tick will
        // both attach the companion and post the notice in lockstep.
        function onPlay() {
            if (done) return;
            played = true;
        }
        video.addEventListener("play", onPlay);
        if (!video.paused && !video.ended) {
            onPlay();
        }

        // Bootstrap polling for the non-IO fallback path (visible defaults to
        // true above). With IO present, the first isIntersecting callback
        // calls _audioSyncResume which starts the loop.
        if (!visObserver && visible) {
            poll();
        }

        if (visObserver) visObserver.observe(video);
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
