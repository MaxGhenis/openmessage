"use client";

import { useEffect, useState } from "react";
import { IconMoon, IconSun } from "@tabler/icons-react";

// Browser toolbar colors. They match --paper in app/globals.css and the
// viewport themeColor in app/layout.jsx (tests/accessibility.test.js checks).
const THEME_COLOR = { light: "#f4f6fa", dark: "#09111c" };
const DARK_QUERY = "(prefers-color-scheme: dark)";

// The pre-paint script in app/layout.jsx sets data-theme from a stored choice
// before first render. Without one, the page follows the system setting
// through CSS alone.
function storedTheme() {
  const value = document.documentElement.getAttribute("data-theme");
  return value === "dark" || value === "light" ? value : null;
}

function activeTheme() {
  return storedTheme() || (window.matchMedia(DARK_QUERY).matches ? "dark" : "light");
}

// app/layout.jsx emits one theme-color meta per system color scheme. A stored
// choice overrides the system scheme, so every one of them takes its color.
function paintThemeColor(theme) {
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    meta.setAttribute("content", THEME_COLOR[theme]);
  }
}

export function ThemeToggle() {
  // null until mount: the server sees neither the stored choice nor the system setting.
  const [theme, setTheme] = useState(null);

  useEffect(() => {
    const stored = storedTheme();
    if (stored) {
      paintThemeColor(stored);
    }
    setTheme(activeTheme());

    // With no stored choice, the button follows the system setting as the page does.
    const media = window.matchMedia(DARK_QUERY);
    const follow = () => setTheme(activeTheme());
    media.addEventListener("change", follow);
    return () => media.removeEventListener("change", follow);
  }, []);

  function toggle() {
    const next = activeTheme() === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    paintThemeColor(next);
    setTheme(next);
    try {
      localStorage.setItem("om-theme", next);
    } catch {
      // Storage can be unavailable (private mode); the choice still holds for this page.
    }
  }

  return (
    <button
      type="button"
      className="icon-btn theme-toggle"
      onClick={toggle}
      aria-label="Dark theme"
      aria-pressed={theme === null ? undefined : theme === "dark"}
    >
      <IconMoon className="moon" stroke={1.7} aria-hidden="true" />
      <IconSun className="sun" stroke={1.7} aria-hidden="true" />
    </button>
  );
}
