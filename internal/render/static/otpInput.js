// Upgrade each .otp-input into a row of single-digit cells. The original input
// is kept (hidden) as the source of truth so the form still submits its name
// (code / current_code), and no-JS users fall back to the plain numeric field.
(function () {
  "use strict";
  var LEN = 6;

  function upgrade(src) {
    var cells = [];
    var submitted = false;
    var autosubmit = src.hasAttribute("data-autosubmit");
    var box = document.createElement("div");
    box.className = "otp";
    box.setAttribute("role", "group");
    box.setAttribute("aria-label", src.placeholder || "one-time code");

    function sync() {
      var value = cells
        .map(function (c) {
          return c.value;
        })
        .join("");
      src.value = value;
      cells.forEach(function (c) {
        c.classList.toggle("filled", c.value !== "");
      });
      // Submit the moment all six digits are present — no manual click. Guard
      // with `submitted` so re-fired input events can't double-post the form.
      if (autosubmit && !submitted && value.length === LEN && src.form) {
        submitted = true;
        cells.forEach(function (c) {
          c.disabled = true;
        });
        if (src.form.requestSubmit) {
          src.form.requestSubmit();
        } else {
          src.form.submit();
        }
      }
    }

    function focusCell(i) {
      if (i >= 0 && i < LEN) {
        cells[i].focus();
        cells[i].select();
      }
    }

    for (var i = 0; i < LEN; i++) {
      var c = document.createElement("input");
      c.type = "text";
      c.className = "otp-cell";
      c.inputMode = "numeric";
      c.autocomplete = i === 0 ? "one-time-code" : "off";
      c.maxLength = 1;
      // size=1 trims the input's intrinsic width (default ~20 chars). Without it,
      // even with min-width:0 the cells advertise a huge preferred width that can
      // still nudge iOS Safari into shrink-to-fit on the narrowest viewports.
      c.size = 1;
      c.setAttribute("aria-label", "Digit " + (i + 1));

      (function (idx, cell) {
        cell.addEventListener("input", function () {
          // Keep only the last typed digit; ignore everything else.
          var d = (cell.value.match(/\d/g) || []).pop() || "";
          cell.value = d;
          sync();
          if (d) {
            focusCell(idx + 1);
          }
        });

        cell.addEventListener("keydown", function (e) {
          if (e.key === "Backspace") {
            if (cell.value) {
              cell.value = "";
              sync();
            } else {
              focusCell(idx - 1);
              if (cells[idx - 1]) {
                cells[idx - 1].value = "";
                sync();
              }
            }
            e.preventDefault();
          } else if (e.key === "ArrowLeft") {
            focusCell(idx - 1);
            e.preventDefault();
          } else if (e.key === "ArrowRight") {
            focusCell(idx + 1);
            e.preventDefault();
          } else if (
            e.key.length === 1 &&
            !/\d/.test(e.key) &&
            !e.ctrlKey &&
            !e.metaKey
          ) {
            // Block any single non-digit character at the source.
            e.preventDefault();
          }
        });

        cell.addEventListener("focus", function () {
          cell.select();
        });

        cell.addEventListener("paste", function (e) {
          e.preventDefault();
          var txt =
            (e.clipboardData || window.clipboardData).getData("text") || "";
          var digits = (txt.match(/\d/g) || []).slice(0, LEN - idx);
          for (var k = 0; k < digits.length; k++) {
            cells[idx + k].value = digits[k];
          }
          sync();
          focusCell(Math.min(idx + digits.length, LEN - 1));
        });
      })(i, c);

      cells.push(c);
      box.appendChild(c);
    }

    // Hide the original but keep it submitting; insert the cell row after it.
    src.type = "hidden";
    var autofocus = src.hasAttribute("autofocus");
    src.removeAttribute("autofocus");
    src.parentNode.insertBefore(box, src.nextSibling);
    if (autofocus) {
      focusCell(0);
    }
  }

  var inputs = document.querySelectorAll(".otp-input");
  for (var i = 0; i < inputs.length; i++) {
    upgrade(inputs[i]);
  }

  // The server re-renders this page (with an .err banner) after a wrong code,
  // which naturally clears the cells. Flash a shake + red outline so the reset
  // reads as "try again", then drop back to neutral and refocus the first cell.
  if (document.querySelector(".err")) {
    var boxes = document.querySelectorAll(".otp");
    for (var b = 0; b < boxes.length; b++) {
      (function (box) {
        box.classList.add("error", "shake");
        box.addEventListener("animationend", function () {
          box.classList.remove("shake");
        });
        var first = box.querySelector(".otp-cell");
        if (first) {
          first.addEventListener("input", function () {
            box.classList.remove("error");
          }, { once: true });
        }
      })(boxes[b]);
    }
  }
})();
