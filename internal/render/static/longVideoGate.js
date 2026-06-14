// @license http://www.gnu.org/licenses/agpl-3.0.html AGPL-3.0
// longVideoGate.js — turn a long-video placeholder card into a real <video>
// element when the user clicks the cloud-download button, with a stop button
// to cancel the download and restore the gate.
//
// Server-rendered <div class="long-video-gate" data-long-src="..."> stands in
// for the <video> element on clips exceeding the user-configured threshold.
// Nothing is downloaded while the gate is mounted. On click or Enter/Space we
// swap the gate for a <video controls src=...> whose URL already carries
// `&long=1` so the backend priority gate (Priority.Long) drops its bytes to
// the bottom of the bandwidth bucket. A stop button lets the user abort the
// download and return to the gate placeholder.
(function () {
    "use strict";

    function activate(gate) {
        if (gate._activated) return;
        gate._activated = true;
        var src = gate.getAttribute("data-long-src") || "";
        if (!src) return;

        var video = document.createElement("video");
        video.className = "post_media_video";
        video.setAttribute("controls", "");
        video.setAttribute("preload", "auto");
        video.src = src;

        var stopBtn = document.createElement("button");
        stopBtn.type = "button";
        stopBtn.className = "long-video-stop";
        stopBtn.innerHTML = '<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"></rect></svg>';

        var wrapper = document.createElement("div");
        wrapper.className = "long-video-active";
        wrapper.appendChild(video);
        wrapper.appendChild(stopBtn);

        gate.parentNode.replaceChild(wrapper, gate);

        stopBtn.addEventListener("click", function (ev) {
            ev.stopPropagation();
            video.pause();
            video.removeAttribute("src");
            video.load();
            gate._activated = false;
            wrapper.parentNode.replaceChild(gate, wrapper);
        });

        var p = video.play();
        if (p && p.catch) p.catch(function () {});
    }

    function onActivate(ev) {
        var gate = ev.target.closest(".long-video-gate");
        if (!gate) return;
        if (ev.type === "keydown") {
            if (ev.key !== "Enter" && ev.key !== " ") return;
            ev.preventDefault();
        }
        activate(gate);
    }

    function init() {
        document.addEventListener("click", onActivate);
        document.addEventListener("keydown", onActivate);
    }

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", init);
    } else {
        init();
    }
})();
// @license-end
