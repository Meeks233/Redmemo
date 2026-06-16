// Cloud↔local source switch for the shared searchbox. The checkbox's checked
// state means "local"; toggling it swaps the form's action between the cloud
// endpoint (/search — live Reddit) and the local one (/archive — PostgreSQL),
// and updates the tooltip. The slash-draw animation is pure CSS (driven by the
// checkbox :checked state); this script only rewires where the form submits, so
// with JS off the control still renders and reflects the page's current mode.
//
// Manual-override model: by default the toggle auto-follows the server-rendered
// state, which already tracks the system setting + the page being viewed. The
// moment the user clicks it, that pick is stashed in a session-scoped field
// (sessionStorage) and replayed on every later page this session — so the
// toggle stops auto-following the page until the tab is closed. When the
// operator has disabled upstream the control is locked: the override is ignored
// and never written.
(function () {
  "use strict";

  var KEY = "redmemo:search_mode"; // session-scoped manual override: "cloud" | "local"

  var form = document.getElementById("searchbox");
  var toggle = document.getElementById("search_mode_toggle");
  if (!form || !toggle) return;

  var label = document.getElementById("search_mode");
  var locked = !!(label && label.getAttribute("data-locked"));
  var cloudAction = form.getAttribute("data-cloud-action") || "/search";
  var localAction = form.getAttribute("data-local-action") || "/archive";

  function session() {
    // sessionStorage can throw in privacy modes / sandboxed frames; degrade to
    // pure auto-follow (no persisted override) rather than break the toggle.
    try {
      return window.sessionStorage;
    } catch (e) {
      return null;
    }
  }

  function apply() {
    var local = toggle.checked;
    form.setAttribute("action", local ? localAction : cloudAction);
    if (label) {
      var title = label.getAttribute(
        local ? "data-title-local" : "data-title-cloud"
      );
      if (title) label.setAttribute("title", title);
    }
  }

  // Locked instances are cache-only: keep the server-forced local state, never
  // wire up the override or the change handler.
  if (locked) {
    apply();
    return;
  }

  // Replay a manual pick from earlier this session, overriding the page default.
  var store = session();
  if (store) {
    var saved = store.getItem(KEY);
    if (saved === "cloud") toggle.checked = false;
    else if (saved === "local") toggle.checked = true;
  }

  // A real user click is the only thing that fires `change` (programmatic
  // .checked assignment above does not) — so this is exactly "manual override".
  toggle.addEventListener("change", function () {
    if (store) store.setItem(KEY, toggle.checked ? "local" : "cloud");
    apply();
  });

  apply();
})();
