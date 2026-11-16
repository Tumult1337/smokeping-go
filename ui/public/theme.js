// Key and values mirror src/theme.ts.
(function () {
  try {
    var pref = localStorage.getItem("gosmokeping.theme") || "system";
    var dark =
      pref === "dark" ||
      (pref !== "light" &&
        matchMedia("(prefers-color-scheme: dark)").matches);
    var theme = dark ? "dark" : "light";
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
  } catch (_) {
    document.documentElement.dataset.theme = "dark";
  }
})();
