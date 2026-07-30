/*
 * maktoob interface behaviour.
 *
 * Progressive enhancement is the rule here, not a preference. Every page works
 * with this file blocked: search and the chat filter are a GET form, import is
 * a multipart POST that redirects, and the transcript is server-rendered. What
 * this script adds is liveness and in-place correction.
 *
 * Nothing below renders a note. When the list or the transcript needs to
 * change, the page is re-fetched and the relevant element swapped in, so the Go
 * templates stay the only place that knows how a note looks. Two renderers that
 * drift is how an interface starts lying about its own data.
 */
(function () {
  "use strict";

  var i18nEl = document.getElementById("i18n");
  var t = i18nEl ? i18nEl.dataset : {};
  var live = document.getElementById("live");
  // The chrome's direction. Anything this script builds has to carry it
  // explicitly, because the transcript it gets inserted into is dir="rtl" and
  // English strings would otherwise render with their punctuation reversed.
  var uiDir = t.dir || document.documentElement.getAttribute("dir") || "ltr";

  // Seeking and correcting only work with this file running, so the markup
  // ships them hidden and they are revealed here. Without this they are buttons
  // that focus, accept a keypress and do nothing — on a long transcript, dozens
  // of dead tab stops between the reader and the rest of the page.
  function revealScriptedControls(root) {
    (root || document).querySelectorAll("[data-js-only][hidden]").forEach(function (el) {
      el.hidden = false;
    });
  }
  revealScriptedControls();

  function announce(message) {
    if (!live || !message) return;
    // Cleared first: repeating identical text into a live region is often
    // dropped as "unchanged" and the user hears nothing.
    live.textContent = "";
    window.setTimeout(function () { live.textContent = message; }, 60);
  }

  /* --- audio seeking ---------------------------------------------------- */

  var player = document.getElementById("player");

  document.addEventListener("click", function (ev) {
    var seek = ev.target.closest(".seek");
    if (!seek || !player) return;
    ev.preventDefault();
    var start = parseFloat(seek.dataset.start || "0");
    if (!isNaN(start)) {
      player.currentTime = start;
      var playing = player.play();
      if (playing && playing.catch) playing.catch(function () { /* autoplay refused */ });
    }
  });

  /* --- highlight the segment linked from a search result ---------------- */

  function highlightHash() {
    var id = window.location.hash.slice(1);
    if (!id) return;
    document.querySelectorAll(".segment.is-target").forEach(function (el) {
      el.classList.remove("is-target");
    });
    var target = document.getElementById(id);
    if (target && target.classList.contains("segment")) {
      target.classList.add("is-target");
    }
  }
  window.addEventListener("hashchange", highlightHash);
  highlightHash();

  /* --- correcting a line ------------------------------------------------ */

  var editorOpen = false;

  document.addEventListener("click", function (ev) {
    var button = ev.target.closest("button.edit");
    if (!button) return;
    ev.preventDefault();
    openEditor(button.closest(".segment"), button);
  });

  function openEditor(segment, trigger) {
    if (!segment || segment.querySelector(".editor")) return;

    var body = segment.querySelector(".segment-body");
    var textEl = segment.querySelector(".segment-text");
    var actions = segment.querySelector(".segment-actions");
    if (!body || !textEl) return;

    editorOpen = true;

    var editor = document.createElement("div");
    editor.className = "editor";

    var field = document.createElement("textarea");
    field.value = textEl.textContent.trim();
    field.setAttribute("aria-label", t.edit || "Edit");
    // The transcript is Arabic; the textarea must not inherit the chrome's
    // direction or the caret and punctuation land on the wrong side.
    field.setAttribute("dir", "auto");
    field.lang = "ar";

    var row = document.createElement("div");
    row.className = "editor-actions";
    // Without this the buttons inherit the transcript's rtl and Save/Cancel
    // render in the opposite order to every other pair of buttons on the page.
    row.setAttribute("dir", uiDir);

    var save = document.createElement("button");
    save.type = "button";
    save.textContent = t.save || "Save";

    var cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "secondary";
    cancel.textContent = t.cancel || "Cancel";

    var error = document.createElement("p");
    error.className = "editor-error";
    error.setAttribute("dir", uiDir);
    // role="alert" so the failure is spoken. Without it a screen-reader user
    // presses Save, hears nothing at all, and has no way to learn the
    // correction was lost.
    error.setAttribute("role", "alert");
    error.hidden = true;

    row.appendChild(save);
    row.appendChild(cancel);
    editor.appendChild(field);
    editor.appendChild(row);
    editor.appendChild(error);

    textEl.hidden = true;
    if (actions) actions.hidden = true;
    body.insertBefore(editor, actions || null);
    field.focus();
    field.setSelectionRange(field.value.length, field.value.length);

    function close() {
      editorOpen = false;
      editor.remove();
      textEl.hidden = false;
      if (actions) actions.hidden = false;
      if (trigger) trigger.focus();
    }

    cancel.addEventListener("click", close);

    editor.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape") {
        ev.preventDefault();
        close();
      }
      // Enter alone inserts a newline: a transcript line can legitimately wrap,
      // and a bare Enter that saved would make that impossible to type.
      if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) {
        ev.preventDefault();
        save.click();
      }
    });

    save.addEventListener("click", function () {
      var id = segment.dataset.segmentId;
      save.disabled = true;
      cancel.disabled = true;
      save.textContent = t.saving || "Saving";
      error.hidden = true;

      fetch("/api/segments/" + encodeURIComponent(id), {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text: field.value })
      })
        .then(function (res) {
          if (!res.ok) throw new Error("save failed: " + res.status);
          return res.json();
        })
        .then(function (seg) {
          applySegment(segment, seg);
          close();
          announce(t.saved || "");
        })
        .catch(function () {
          save.disabled = false;
          cancel.disabled = false;
          save.textContent = t.save || "Save";
          error.textContent = t.saveFailed || "Could not save.";
          error.hidden = false;
          announce(t.saveFailedAnnounce || t.saveFailed || "");
        });
    });
  }

  // applySegment writes back exactly what the server stored, not what was
  // typed. If the two ever differ, the page should show the truth.
  function applySegment(segment, seg) {
    var textEl = segment.querySelector(".segment-text");
    if (textEl) textEl.textContent = seg.text;

    segment.classList.toggle("is-edited", !!seg.edited);
    segment.classList.toggle("is-suspect", !!seg.suspect);
    segment.classList.toggle("is-lowconf", !!seg.low_confidence);

    var markers = segment.querySelector(".markers");
    var text = (seg.markers || []).join(", ");
    if (text) {
      if (!markers) {
        markers = document.createElement("p");
        markers.className = "markers";
        markers.setAttribute("dir", uiDir);
        var body = segment.querySelector(".segment-body");
        var actions = segment.querySelector(".segment-actions");
        if (body) body.insertBefore(markers, actions || null);
      }
      markers.textContent = text;
    } else if (markers) {
      markers.remove();
    }
  }

  /* --- live updates ----------------------------------------------------- */

  if (!window.EventSource) return;

  var pending = null;

  function scheduleRefresh() {
    // A batch of finished notes produces a burst of events. One refresh at the
    // end of the burst is the same result for a fraction of the work.
    if (pending) window.clearTimeout(pending);
    pending = window.setTimeout(refresh, 400);
  }

  function refresh() {
    pending = null;
    // Never yank the page out from under someone mid-correction.
    if (editorOpen) return;

    fetch(window.location.href, { headers: { "Accept": "text/html" } })
      .then(function (res) {
        if (!res.ok) throw new Error("refresh failed");
        return res.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        if (!swap(doc, "#note-list") && !swap(doc, "#segments")) {
          // The region we track is absent — usually the empty state becoming a
          // first note, or the reverse. A reload is correct and rare.
          window.location.reload();
        }
        highlightHash();
      })
      .catch(function () { /* the next event will try again */ });
  }

  // swap replaces a region with its freshly-rendered version, keeping keyboard
  // focus where the user left it.
  //
  // replaceWith destroys the focused element, and the browser then resets focus
  // to <body> — silently throwing a keyboard user to the top of the document
  // mid-navigation. With a burst of notes finishing, that happens every 400ms
  // and makes the page unusable without a mouse. So the focused control is
  // identified by a stable attribute before the swap and refocused after it.
  function swap(doc, selector) {
    var fresh = doc.querySelector(selector);
    var current = document.querySelector(selector);
    if (!fresh || !current) return false;

    var path = focusPath(document.activeElement, current);
    current.replaceWith(fresh);
    if (path) restoreFocus(path, fresh);

    revealScriptedControls(fresh);
    return true;
  }

  // focusPath describes the focused element well enough to find its counterpart
  // in the replacement markup, or returns null if focus is outside the region
  // being swapped.
  function focusPath(active, region) {
    if (!active || active === document.body || !region.contains(active)) return null;

    var owner = active.closest("[data-segment-id], [data-note-id]");
    if (!owner) return null;

    return {
      ownerAttr: owner.hasAttribute("data-segment-id") ? "data-segment-id" : "data-note-id",
      ownerValue: owner.getAttribute("data-segment-id") || owner.getAttribute("data-note-id"),
      // Which control within that row, so focus lands on the seek button again
      // rather than merely somewhere in the right note.
      controlClass: active.className || ""
    };
  }

  function restoreFocus(path, fresh) {
    var owner = fresh.querySelector(
      "[" + path.ownerAttr + '="' + (window.CSS && CSS.escape ? CSS.escape(path.ownerValue) : path.ownerValue) + '"]'
    );
    if (!owner) return;

    var target = null;
    if (path.controlClass) {
      target = owner.querySelector("." + path.controlClass.split(/\s+/)[0]);
    }
    if (!target) {
      target = owner.querySelector("a, button, [tabindex]");
    }
    if (target && typeof target.focus === "function") target.focus();
  }

  var source = new EventSource("/events");
  var noteOnPage = (document.querySelector(".player") || {}).dataset;
  var currentNoteID = noteOnPage ? noteOnPage.noteId : null;

  source.addEventListener("arrived", function (ev) {
    var data = parse(ev.data);
    announce(data.text || "");
    // A new note never concerns an open transcript page.
    if (!currentNoteID) scheduleRefresh();
  });

  source.addEventListener("updated", function (ev) {
    var data = parse(ev.data);
    if (currentNoteID && data.note_id !== currentNoteID) return;
    if (data.text) announce(data.text);
    scheduleRefresh();
  });

  function parse(raw) {
    try { return JSON.parse(raw) || {}; } catch (e) { return {}; }
  }
})();
