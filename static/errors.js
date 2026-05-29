(function () {
  const STORAGE_KEY = "chat.errors";
  const MAX_ERRORS = 200;

  function escapeHtml(s) {
    return (s || "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#39;");
  }

  function loadErrors() {
    try {
      const raw = localStorage.getItem(STORAGE_KEY);
      if (!raw) return [];
      const parsed = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed : [];
    } catch (_) {
      return [];
    }
  }

  function saveErrors(errs) {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(errs.slice(-MAX_ERRORS)));
    } catch (_) {}
  }

  function normalizeError(item) {
    if (!item || typeof item !== "object") return null;
    const message = String(item.message || item.Message || "").trim();
    if (!message) return null;
    return {
      at: String(item.at || item.At || new Date().toISOString()),
      source: String(item.source || item.Source || "unknown"),
      message,
    };
  }

  function pushErrors(incoming) {
    if (!incoming || !incoming.length) return;
    const list = loadErrors();
    let changed = false;
    for (const raw of incoming) {
      const e = normalizeError(raw);
      if (!e) continue;
      list.push(e);
      changed = true;
    }
    if (changed) saveErrors(list);
  }

  function parseHeaderErrors(xhr) {
    if (!xhr || !xhr.getResponseHeader) return [];
    const raw = xhr.getResponseHeader("X-Chat-Errors");
    if (!raw) return [];
    try {
      const parsed = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed : [];
    } catch (_) {
      return [];
    }
  }

  function formatTime(iso) {
    try {
      const d = new Date(iso);
      if (Number.isNaN(d.getTime())) return iso;
      return d.toLocaleString();
    } catch (_) {
      return iso;
    }
  }

  function updateUI() {
    const badge = document.getElementById("errorsBadge");
    const listEl = document.getElementById("errorsList");
    const errs = loadErrors();
    if (badge) {
      if (errs.length > 0) {
        badge.hidden = false;
        badge.textContent = errs.length > 99 ? "99+" : String(errs.length);
      } else {
        badge.hidden = true;
        badge.textContent = "0";
      }
    }
    if (listEl) {
      if (!errs.length) {
        listEl.innerHTML = '<li class="errors-empty">No errors logged.</li>';
        return;
      }
      listEl.innerHTML = errs
        .slice()
        .reverse()
        .map(
          (e) =>
            '<li class="errors-item">' +
            '<div class="errors-itemmeta">' +
            '<span class="errors-source">' +
            escapeHtml(e.source) +
            "</span>" +
            '<time class="errors-time">' +
            escapeHtml(formatTime(e.at)) +
            "</time>" +
            "</div>" +
            '<p class="errors-msg">' +
            escapeHtml(e.message) +
            "</p>" +
            "</li>"
        )
        .join("");
    }
  }

  function closePanel() {
    const panel = document.getElementById("errorsPanel");
    const bell = document.getElementById("errorsBell");
    if (panel) panel.hidden = true;
    if (bell) bell.setAttribute("aria-expanded", "false");
  }

  function togglePanel() {
    const panel = document.getElementById("errorsPanel");
    const bell = document.getElementById("errorsBell");
    if (!panel || !bell) return;
    const open = panel.hidden;
    panel.hidden = !open;
    bell.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) updateUI();
  }

  function initFromDOM() {
    const el = document.getElementById("chat-initial-errors");
    if (!el) return;
    try {
      const parsed = JSON.parse(el.textContent || "[]");
      pushErrors(Array.isArray(parsed) ? parsed : []);
    } catch (_) {}
  }

  function bindUI() {
    const bell = document.getElementById("errorsBell");
    const clearBtn = document.getElementById("errorsClear");
    const wrap = document.querySelector(".errors-wrap");

    if (bell) bell.addEventListener("click", (e) => {
      e.stopPropagation();
      togglePanel();
    });

    if (clearBtn) clearBtn.addEventListener("click", () => {
      saveErrors([]);
      updateUI();
    });

    document.addEventListener("click", (e) => {
      const panel = document.getElementById("errorsPanel");
      if (!panel || panel.hidden) return;
      if (e.target && e.target.closest && e.target.closest(".errors-wrap")) return;
      closePanel();
    });

    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") closePanel();
    });
  }

  document.addEventListener("htmx:afterRequest", (e) => {
    const xhr = e.detail && e.detail.xhr;
    pushErrors(parseHeaderErrors(xhr));
    updateUI();
  });

  document.addEventListener("htmx:responseError", (e) => {
    const xhr = e.detail && e.detail.xhr;
    const fromHeader = parseHeaderErrors(xhr);
    if (fromHeader.length) {
      pushErrors(fromHeader);
    } else {
      const status = xhr ? xhr.status : 0;
      pushErrors([
        {
          source: "htmx",
          message: "Request failed" + (status ? " (HTTP " + status + ")" : ""),
          at: new Date().toISOString(),
        },
      ]);
    }
    updateUI();
  });

  document.addEventListener("DOMContentLoaded", () => {
    initFromDOM();
    bindUI();
    updateUI();
  });

  window.ChatErrors = { pushErrors, updateUI, loadErrors };
})();
