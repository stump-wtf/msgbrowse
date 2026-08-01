// Video player modal for the Media gallery (issue #4).
// Delegated click handling on video tiles, CSP-safe (no inline handlers).
(function () {
  "use strict";

  var modal = null;
  var videoEl = null;
  var titleEl = null;

  function createModal() {
    if (modal) return modal;

    modal = document.createElement("div");
    modal.className = "video-modal";
    modal.setAttribute("role", "dialog");
    modal.setAttribute("aria-modal", "true");
    modal.setAttribute("aria-label", "Video player");
    modal.hidden = true;

    var backdrop = document.createElement("div");
    backdrop.className = "video-modal-backdrop";
    backdrop.addEventListener("click", closeModal);

    var panel = document.createElement("div");
    panel.className = "video-modal-panel";

    var header = document.createElement("div");
    header.className = "video-modal-header";

    titleEl = document.createElement("span");
    titleEl.className = "video-modal-title";

    var closeBtn = document.createElement("button");
    closeBtn.className = "video-modal-close";
    closeBtn.setAttribute("aria-label", "Close video player");
    closeBtn.innerHTML = "✕";
    closeBtn.addEventListener("click", closeModal);

    header.appendChild(titleEl);
    header.appendChild(closeBtn);

    var body = document.createElement("div");
    body.className = "video-modal-body";

    videoEl = document.createElement("video");
    videoEl.className = "video-modal-video";
    videoEl.controls = true;
    videoEl.playsInline = true;

    body.appendChild(videoEl);
    panel.appendChild(header);
    panel.appendChild(body);
    modal.appendChild(backdrop);
    modal.appendChild(panel);

    document.body.appendChild(modal);
    return modal;
  }

  function openModal(src, name) {
    createModal();
    titleEl.textContent = name || "Video";
    videoEl.src = src;
    modal.hidden = false;
    document.body.style.overflow = "hidden";
    videoEl.focus();
  }

  function closeModal() {
    if (!modal) return;
    videoEl.pause();
    videoEl.src = "";
    modal.hidden = true;
    document.body.style.overflow = "";
  }

  function onKeydown(e) {
    if (e.key === "Escape" && modal && !modal.hidden) {
      closeModal();
    }
  }

  function onClick(e) {
    var tile = e.target.closest(".video-tile");
    if (!tile) return;
    var src = tile.getAttribute("data-video-src");
    var name = tile.getAttribute("data-video-name");
    if (src) {
      e.preventDefault();
      openModal(src, name);
    }
  }

  function onKeydownTile(e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    var tile = e.target.closest(".video-tile");
    if (!tile) return;
    var src = tile.getAttribute("data-video-src");
    var name = tile.getAttribute("data-video-name");
    if (src) {
      e.preventDefault();
      openModal(src, name);
    }
  }

  document.addEventListener("click", onClick);
  document.addEventListener("keydown", onKeydownTile);
  document.addEventListener("keydown", onKeydown);
})();
