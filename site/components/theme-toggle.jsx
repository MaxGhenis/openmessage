"use client";

import { IconMoon, IconSun } from "@tabler/icons-react";

// The pre-paint script in app/layout.jsx applies a stored choice before first
// render; this button only flips and stores it. With no stored choice the page
// follows the system setting through CSS alone.
export function ThemeToggle() {
  function toggle() {
    const root = document.documentElement;
    const systemDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    const current = root.getAttribute("data-theme") || (systemDark ? "dark" : "light");
    const next = current === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try {
      localStorage.setItem("om-theme", next);
    } catch {
      // Storage can be unavailable (private mode); the choice still holds for this page.
    }
  }

  return (
    <button type="button" className="icon-btn theme-toggle" onClick={toggle} aria-label="Switch between light and dark theme">
      <IconMoon className="moon" stroke={1.7} aria-hidden="true" />
      <IconSun className="sun" stroke={1.7} aria-hidden="true" />
    </button>
  );
}
