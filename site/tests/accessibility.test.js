// Accessibility rules that live in the stylesheet and the shared chrome. The
// components are JSX, which these tests don't compile, so they read sources
// as text, as tests/content.test.js does.
import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import fc from "fast-check";
import { describe, expect, test } from "vitest";

const siteRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (file) => readFileSync(path.join(siteRoot, file), "utf8");

const css = read("app/globals.css");
const header = read("components/site-header.jsx");
const copyCommand = read("components/copy-command.jsx");
const themeToggle = read("components/theme-toggle.jsx");
const layout = read("app/layout.jsx");

// The declarations of the first rule whose selector is exactly `selector`.
function block(selector) {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) {
    throw new Error(`no "${selector}" rule in globals.css`);
  }
  const open = css.indexOf("{", start);
  const close = css.indexOf("}", open);
  return css.slice(open + 1, close);
}

function tokens(selector) {
  return Object.fromEntries([...block(selector).matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
}

// WCAG 2.x relative luminance and contrast ratio.
function luminance(hex) {
  const value = Number.parseInt(hex.slice(1), 16);
  const channel = (shift) => {
    const c = ((value >> shift) & 255) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(16) + 0.7152 * channel(8) + 0.0722 * channel(0);
}

function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const light = tokens(":root");
const darkBySystem = { ...light, ...tokens(':root:not([data-theme="light"])') };
const darkByChoice = { ...light, ...tokens(':root[data-theme="dark"]') };
const themes = { light, "dark (system)": darkBySystem, "dark (chosen)": darkByChoice };

describe("contrast helper", () => {
  test("matches known WCAG ratios", () => {
    expect(contrast("#000000", "#ffffff")).toBeCloseTo(21, 5);
    expect(contrast("#777777", "#ffffff")).toBeCloseTo(4.48, 2);
  });

  test("is symmetric and between 1 and 21", () => {
    const hex = fc.integer({ min: 0, max: 0xffffff }).map((n) => `#${n.toString(16).padStart(6, "0")}`);
    fc.assert(
      fc.property(hex, hex, (a, b) => {
        const ratio = contrast(a, b);
        return ratio === contrast(b, a) && ratio >= 1 && ratio <= 21 + 1e-9;
      })
    );
  });
});

describe("color tokens", () => {
  test("both dark blocks set the same tokens to the same values", () => {
    expect(tokens(':root:not([data-theme="light"])')).toEqual(tokens(':root[data-theme="dark"]'));
  });

  test.each(Object.entries(themes))("the text field border holds 3:1 against the page and the field in %s", (_name, theme) => {
    expect(contrast(theme["--field-border"], theme["--paper"])).toBeGreaterThanOrEqual(3);
    expect(contrast(theme["--field-border"], theme["--surface"])).toBeGreaterThanOrEqual(3);
  });

  test("the text field draws its border in --field-border", () => {
    expect(block(".field")).toMatch(/border:\s*1px solid var\(--field-border\)/);
  });

  test.each(Object.entries(themes))("link underlines hold 3:1 against the page and surfaces in %s", (_name, theme) => {
    const rule = css.match(/^a \{([^}]*)\}/m);
    expect(rule).not.toBeNull();
    const token = rule[1].match(/text-decoration-color:\s*var\((--[\w-]+)\)/)[1];
    expect(contrast(theme[token], theme["--paper"])).toBeGreaterThanOrEqual(3);
    expect(contrast(theme[token], theme["--surface"])).toBeGreaterThanOrEqual(3);
  });
});

describe("browser toolbar color", () => {
  const colors = Object.fromEntries([...themeToggle.matchAll(/(light|dark): "(#[0-9a-f]{6})"/g)].map((m) => [m[1], m[2]]));

  test("the theme toggle paints the page color of each theme", () => {
    expect(colors).toEqual({ light: light["--paper"], dark: darkByChoice["--paper"] });
  });

  test("the layout's viewport theme colors are the same two colors", () => {
    expect(layout).toContain(`media: "(prefers-color-scheme: light)", color: "${colors.light}"`);
    expect(layout).toContain(`media: "(prefers-color-scheme: dark)", color: "${colors.dark}"`);
  });

  test("the toggle reports its state and updates every theme-color meta", () => {
    expect(themeToggle).toMatch(/aria-pressed=\{/);
    expect(themeToggle).toContain(`querySelectorAll('meta[name="theme-color"]')`);
  });
});

describe("masthead", () => {
  test("the row and the nav wrap rather than overflow", () => {
    expect(block(".masthead__inner")).toMatch(/flex-wrap:\s*wrap/);
    expect(block(".nav")).toMatch(/flex-wrap:\s*wrap/);
  });

  test("below 24rem the GitHub icon, and only it, drops out of the header", () => {
    const narrow = css.slice(css.indexOf("@media (max-width: 24rem)"));
    expect(narrow).toMatch(/^@media \(max-width: 24rem\) \{\s*\.nav__repo \{\s*display: none;/);
    const repoLinks = [...header.matchAll(/<a [^>]*className="[^"]*\bnav__repo\b[^"]*"[^>]*>/g)];
    expect(repoLinks).toHaveLength(1);
    expect(repoLinks[0][0]).toContain("href={REPO_URL}");
  });

  test("on short viewports the masthead stops sticking, after the rules it overrides", () => {
    const short = css.indexOf("@media (max-height: 30rem)");
    expect(short).toBeGreaterThan(0);
    const end = css.indexOf("\n}\n", short);
    const rules = css.slice(short, end);
    expect(rules).toMatch(/\.masthead \{\s*position: static;/);
    expect(rules).toMatch(/html \{\s*scroll-padding-top: 1rem;/);
    expect(rules).toMatch(/\.section__head \{\s*top: 1rem;/);
    // Media queries add no specificity, so a later rule for the same selector would win.
    for (const selector of [".masthead {", "html {", ".section__head {"]) {
      expect(css.slice(0, short), selector).toContain(selector);
      expect(css.slice(end), selector).not.toContain(selector);
    }
  });

  test("the Blog link marks the Blog index as the page and a post as inside it", () => {
    expect(header).toMatch(/current === "blog"\)\s*\{\s*return "page";/);
    expect(header).toMatch(/current === "post" \? "true" : undefined/);
    expect(header).toMatch(/href="\/blog" aria-current=\{blogCurrent\(current\)\}/);
    expect(css).toContain('.nav__link[aria-current="page"],\n.nav__link[aria-current="true"] {');
  });
});

describe("download links", () => {
  function sources(dir) {
    return readdirSync(path.join(siteRoot, dir)).flatMap((name) => {
      const relative = path.join(dir, name);
      if (statSync(path.join(siteRoot, relative)).isDirectory()) {
        return sources(relative);
      }
      return /\.jsx?$/.test(name) ? [relative] : [];
    });
  }

  test("no page or component adds a query string to the download path", () => {
    const offenders = [...sources("app"), ...sources("components")].filter((file) => /DOWNLOAD_PATH\}\?/.test(read(file)));
    expect(offenders).toEqual([]);
  });
});

describe("live regions and controls", () => {
  test("no button holds a live region, which Safari would never expose", () => {
    for (const file of readdirSync(path.join(siteRoot, "components"))) {
      const text = read(path.join("components", file));
      for (const button of text.matchAll(/<button[\s\S]*?<\/button>/g)) {
        expect(button[0], file).not.toMatch(/aria-live|role="status"/);
      }
    }
  });

  test("the copy status region follows the button and is always rendered", () => {
    const button = copyCommand.indexOf("</button>");
    const status = copyCommand.indexOf('role="status"');
    expect(button).toBeGreaterThan(0);
    expect(status).toBeGreaterThan(button);
    expect(copyCommand).not.toMatch(/\{status &&/);
  });

  test("the selected network tab keeps a marker in forced-colors mode", () => {
    const forced = css.slice(css.indexOf("@media (forced-colors: active)"));
    expect(forced).toMatch(/^@media \(forced-colors: active\) \{\s*\.routes__tab\[aria-selected="true"\] \{[^}]*background: Highlight;/);
  });

  test("the tool result pane and the thread keep their focus rings inside the clipped frame", () => {
    expect(block(".result:focus-visible")).toMatch(/outline-offset:\s*-3px/);
    expect(block(".thread:focus-visible")).toMatch(/outline-offset:\s*-3px/);
  });

  test("a busy button is styled through aria-disabled", () => {
    expect(block('.btn[aria-disabled="true"]')).toMatch(/opacity:/);
  });
});
