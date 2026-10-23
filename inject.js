/* Dilreet App Testing capture script. Injected only into pages served by
   the local preview server; never deployed. */
(function () {
  if (window.__dilreetInjected) return;
  window.__dilreetInjected = true;
  var UI = "__HP_UI_ORIGIN__";
  var inFrame = window.parent !== window;
  var queue = [], timer = null;

  function flush() {
    timer = null;
    if (!queue.length) return;
    var body = JSON.stringify(queue.splice(0, 50));
    var sent = false;
    try {
      if (navigator.sendBeacon) sent = navigator.sendBeacon("/__dilreet/event", new Blob([body], { type: "text/plain" }));
    } catch (e) {}
    if (!sent && window.__hpFetch) {
      try { window.__hpFetch.call(window, "/__dilreet/event", { method: "POST", body: body, keepalive: true }); } catch (e) {}
    }
    if (queue.length) timer = setTimeout(flush, 200);
  }
  function send(ev) {
    ev.url = ev.url || location.href;
    if (queue.length < 200) queue.push(ev);
    if (!timer) timer = setTimeout(flush, 150);
  }
  window.addEventListener("pagehide", flush);

  function fmt(args) {
    try {
      return Array.prototype.map.call(args, function (x) {
        if (x instanceof Error) return x.stack || String(x);
        if (x && typeof x === "object") { try { return JSON.stringify(x); } catch (e) { return String(x); } }
        return String(x);
      }).join(" ").slice(0, 2000);
    } catch (e) { return "(unprintable)"; }
  }

  ["error", "warn"].forEach(function (lv) {
    var orig = console[lv];
    console[lv] = function () {
      send({ kind: "console", level: lv, msg: fmt(arguments) });
      return orig && orig.apply(console, arguments);
    };
  });

  window.addEventListener("error", function (e) {
    var t = e.target;
    if (t && t !== window && t.tagName) {
      var u = t.currentSrc || t.src || t.href || "";
      send({ kind: "network", level: "error", msg: "Failed to load <" + t.tagName.toLowerCase() + "> " + u, url: location.href });
      return;
    }
    var where = e.filename ? " (" + e.filename + ":" + e.lineno + ":" + e.colno + ")" : "";
    send({ kind: "error", level: "error", msg: (e.message || "Script error") + where, stack: e.error && e.error.stack ? String(e.error.stack) : "" });
  }, true);

  window.addEventListener("unhandledrejection", function (e) {
    send({ kind: "error", level: "error", msg: "Unhandled promise rejection: " + fmt([e.reason]) });
  });

  document.addEventListener("securitypolicyviolation", function (e) {
    send({ kind: "csp", level: "warn", msg: "Content-Security-Policy blocked " + (e.blockedURI || "inline code") + " (" + e.violatedDirective + ")" });
  });

  if (window.fetch) {
    var origFetch = window.fetch;
    window.__hpFetch = origFetch;
    window.fetch = function (input, init) {
      var u = typeof input === "string" ? input : (input && input.url) || String(input);
      return origFetch.apply(this, arguments).then(function (r) {
        if (!r.ok && u.indexOf("/__dilreet/") < 0) send({ kind: "network", level: r.status >= 500 ? "error" : "warn", msg: "fetch " + r.status + " " + u });
        return r;
      }, function (err) {
        send({ kind: "network", level: "error", msg: "fetch failed: " + u + " (" + err + ")" });
        throw err;
      });
    };
  }

  if (window.XMLHttpRequest) {
    var xo = XMLHttpRequest.prototype.open, xs = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (m, u) { this.__hp = { m: m, u: String(u) }; return xo.apply(this, arguments); };
    XMLHttpRequest.prototype.send = function () {
      var x = this;
      x.addEventListener("loadend", function () {
        var i = x.__hp || {};
        if (x.status === 0) send({ kind: "network", level: "error", msg: "XHR failed: " + i.m + " " + i.u });
        else if (x.status >= 400) send({ kind: "network", level: x.status >= 500 ? "error" : "warn", msg: "XHR " + x.status + " " + i.m + " " + i.u });
      });
      return xs.apply(this, arguments);
    };
  }

  function nav() {
    if (inFrame) {
      try { window.parent.postMessage({ type: "hp-nav", href: location.href, title: document.title }, UI); } catch (e) {}
    }
  }
  ["pushState", "replaceState"].forEach(function (k) {
    var o = history[k];
    history[k] = function () { var r = o.apply(this, arguments); nav(); return r; };
  });
  window.addEventListener("popstate", nav);
  window.addEventListener("hashchange", nav);
  window.addEventListener("load", function () { nav(); setTimeout(nav, 500); });
  nav();

  function external(href) {
    if (inFrame) window.parent.postMessage({ type: "hp-external", href: href }, UI);
  }
  document.addEventListener("click", function (e) {
    var a = e.target && e.target.closest ? e.target.closest("a[href]") : null;
    if (!a || e.defaultPrevented || !inFrame) return;
    var u;
    try { u = new URL(a.href, location.href); } catch (x) { return; }
    if (u.protocol !== "http:" && u.protocol !== "https:") return;
    if (u.origin !== location.origin) {
      e.preventDefault();
      external(u.href);
    } else if (a.target && a.target !== "_self" && !a.hasAttribute("download")) {
      e.preventDefault();
      location.href = u.href;
    }
  }, true);

  var origOpen = window.open;
  window.open = function (u) {
    if (inFrame && u) {
      try {
        var x = new URL(u, location.href);
        if (x.origin === location.origin) { location.href = x.href; return null; }
        external(x.href);
        return null;
      } catch (e) {}
    }
    return origOpen.apply(window, arguments);
  };

  window.addEventListener("message", function (e) {
    if (e.origin !== UI || !e.data || e.source !== window.parent) return;
    var c = e.data.hpCmd;
    if (c === "back") history.back();
    else if (c === "forward") history.forward();
    else if (c === "reload") location.reload();
  });
})();
