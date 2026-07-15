// OpenMessage marketing site — minimal client behavior.
// Self-hosted fonts (bundled by Vite; no CDN, no runtime network) + a few
// small, progressively-enhanced interactions.
import "@fontsource-variable/hanken-grotesk";
import "@fontsource-variable/jetbrains-mono";
import "./styles.css";

const root = document.documentElement;
const prefersDark = window.matchMedia("(prefers-color-scheme: dark)");
const THEME_BG = { light: "#f3f5f4", dark: "#0c1013" };

function resolvedTheme() {
  return root.getAttribute("data-theme") || (prefersDark.matches ? "dark" : "light");
}

// Keep the browser-chrome color honest for the theme actually showing,
// including when the visitor overrides their system preference.
let tcMeta;
function syncThemeColor() {
  if (!tcMeta) {
    tcMeta = document.createElement("meta");
    tcMeta.setAttribute("name", "theme-color");
    document.head.appendChild(tcMeta);
  }
  tcMeta.setAttribute("content", THEME_BG[resolvedTheme()]);
}

// Theme toggle.
const toggle = document.getElementById("theme-toggle");
function labelToggle() {
  if (!toggle) return;
  toggle.setAttribute(
    "aria-label",
    resolvedTheme() === "dark" ? "Switch to light theme" : "Switch to dark theme"
  );
}
toggle?.addEventListener("click", () => {
  const next = resolvedTheme() === "dark" ? "light" : "dark";
  root.setAttribute("data-theme", next);
  try {
    localStorage.setItem("om-theme", next);
  } catch (e) {}
  syncThemeColor();
  labelToggle();
});
prefersDark.addEventListener("change", () => {
  if (!root.getAttribute("data-theme")) {
    syncThemeColor();
    labelToggle();
  }
});
syncThemeColor();
labelToggle();

// The hero rail animates on load via pure CSS under .js-motion (set pre-paint),
// so it needs no JS trigger and degrades to its resolved state without motion.
// Reveal features as they enter the viewport (only when motion is allowed).
if (root.classList.contains("js-motion")) {
  const reveals = document.querySelectorAll(".reveal");
  if ("IntersectionObserver" in window && reveals.length) {
    const io = new IntersectionObserver(
      (entries, obs) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            entry.target.classList.add("is-in");
            obs.unobserve(entry.target);
          }
        }
      },
      { rootMargin: "0px 0px -12% 0px", threshold: 0.15 }
    );
    reveals.forEach((el) => io.observe(el));
  } else {
    reveals.forEach((el) => el.classList.add("is-in"));
  }
}

// Hairline under the sticky header once the page scrolls.
const masthead = document.getElementById("masthead");
if (masthead) {
  const onScroll = () => masthead.classList.toggle("is-stuck", window.scrollY > 8);
  onScroll();
  window.addEventListener("scroll", onScroll, { passive: true });
}
