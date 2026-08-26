// auditloop client script. Vanilla JS. Wired on both DOMContentLoaded and
// htmx:load (the body is hx-boost="true", so navigations are AJAX swaps that do
// NOT fire DOMContentLoaded — per-element handlers must re-bind on htmx:load).
(function () {
  "use strict";

  function boot() {
    wireLogin();
    wireLogout();
    wireAuthMode();
    wireCarousels();
  }

  // Dashboard project-card image carousels. The track is a native scroll-snap
  // container (keyboard-scrollable via tabindex=0 for a11y); the prev/next buttons
  // scroll it by roughly one thumbnail width. Progressive enhancement — with JS off
  // the track still scrolls by touch/trackpad/keyboard. Idempotent (data-wired).
  function wireCarousels() {
    document.querySelectorAll("[data-carousel]").forEach(function (root) {
      if (root.dataset.wiredCarousel) return;
      root.dataset.wiredCarousel = "1";
      var track = root.querySelector("[data-carousel-track]");
      if (!track) return;
      function step() {
        var first = track.querySelector("[data-carousel-slide]");
        return first ? first.getBoundingClientRect().width + 12 : track.clientWidth;
      }
      root.querySelectorAll("[data-carousel-prev]").forEach(function (b) {
        b.addEventListener("click", function () {
          track.scrollBy({ left: -step(), behavior: prefersReducedMotion() ? "auto" : "smooth" });
        });
      });
      root.querySelectorAll("[data-carousel-next]").forEach(function (b) {
        b.addEventListener("click", function () {
          track.scrollBy({ left: step(), behavior: prefersReducedMotion() ? "auto" : "smooth" });
        });
      });
    });
  }

  function prefersReducedMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  // Progressive disclosure for the target Authentication card: the login-recipe
  // body (guided form + advanced + credentials) is only shown when the "Login
  // recipe" radio is selected. Idempotent (data-wired guard); re-bound on
  // htmx:load per the hx-boost convention.
  function wireAuthMode() {
    document.querySelectorAll("[data-auth-form]").forEach(function (form) {
      if (form.dataset.wiredAuth) return;
      form.dataset.wiredAuth = "1";
      var body = form.querySelector("[data-auth-recipe]");
      if (!body) return;
      var radios = form.querySelectorAll('input[name="auth_mode"]');
      function sync() {
        var login = form.querySelector('input[name="auth_mode"][value="login"]');
        body.classList.toggle("hidden", !(login && login.checked));
      }
      radios.forEach(function (r) {
        r.addEventListener("change", sync);
      });
      sync();
    });
  }

  // Sign-in form. Under DEV_MODE (auth bypassed) just go to the dashboard;
  // otherwise sign in via supabase-js and sync the session cookie.
  function wireLogin() {
    var form = document.querySelector("[data-login-form]");
    if (!form || form.dataset.wired) return;
    form.dataset.wired = "1";
    var cfg = window.AUDITLOOP || {};
    form.addEventListener("submit", async function (e) {
      e.preventDefault();
      var err = form.querySelector("[data-login-error]");
      if (err) err.classList.add("hidden");
      if (cfg.devMode) {
        window.location.href = "/dashboard";
        return;
      }
      var email = form.querySelector("#email").value;
      var password = form.querySelector("#password").value;
      var sb = window.auditloopSupabase;
      if (!sb) {
        showErr(err, "Auth is not configured.");
        return;
      }
      var res = await sb.auth.signInWithPassword({ email: email, password: password });
      if (res.error) {
        showErr(err, res.error.message || "Sign-in failed.");
        return;
      }
      var token = res.data && res.data.session && res.data.session.access_token;
      if (token) {
        await fetch("/api/auth/sync", { method: "POST", headers: { Authorization: "Bearer " + token } });
      }
      window.location.href = "/dashboard";
    });
  }

  function showErr(el, msg) {
    if (!el) return;
    el.textContent = msg;
    el.classList.remove("hidden");
  }

  function wireLogout() {
    document.querySelectorAll("[data-logout]").forEach(function (btn) {
      if (btn.dataset.wired) return;
      btn.dataset.wired = "1";
      btn.addEventListener("click", function () {
        if (window.auditloopLogout) window.auditloopLogout();
        else window.location.href = "/login";
      });
    });
  }

  // Register the service worker once (PWA installability + offline shell).
  function registerSW() {
    if ("serviceWorker" in navigator) {
      navigator.serviceWorker.register("/sw.js").catch(function () {});
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    boot();
    registerSW();
  });
  document.addEventListener("htmx:load", boot);
})();
