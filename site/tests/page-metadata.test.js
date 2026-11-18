// Share-card metadata. Next.js replaces the root layout's openGraph whole when
// a page sets its own, which drops the image app/opengraph-image.jsx adds, and
// it fills Twitter's title, description and image from openGraph only where the
// layout leaves them unset. lib/metadata.js is plain JS and is tested directly;
// the pages are JSX, which these tests don't compile, so they read as text, as
// tests/content.test.js does.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import fc from "fast-check";
import { describe, expect, test } from "vitest";

import { OG_ALT, OG_IMAGE, OG_IMAGE_ENTRY, pageMetadata } from "../lib/metadata.js";

const siteRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (file) => readFileSync(path.join(siteRoot, file), "utf8");

const text = fc.string({ minLength: 1, maxLength: 80 });
const pagePath = fc.stringMatching(/^\/[a-z0-9-]{0,24}(\/[a-z0-9-]{1,24})?$/);
const day = fc
  .date({ min: new Date("2020-01-01T00:00:00Z"), max: new Date("2030-01-01T00:00:00Z"), noInvalidDate: true })
  .map((date) => date.toISOString().slice(0, 10));

describe("pageMetadata", () => {
  test("names the page's own title, description, URL and image, for any page", () => {
    fc.assert(
      fc.property(text, text, pagePath, (title, description, pathname) => {
        const metadata = pageMetadata({ title, description, path: pathname });
        expect(metadata).toEqual({
          title,
          description,
          alternates: { canonical: pathname },
          openGraph: {
            title,
            description,
            url: pathname,
            siteName: "OpenMessage",
            locale: "en_US",
            type: "website",
            images: [OG_IMAGE_ENTRY]
          }
        });
      })
    );
  });

  test("an openGraph override sets the keys it names and keeps the rest", () => {
    fc.assert(
      fc.property(text, text, pagePath, day, day, (title, description, pathname, published, revised) => {
        const article = { type: "article", publishedTime: published, modifiedTime: revised };
        const { openGraph } = pageMetadata({ title, description, path: pathname, openGraph: article });
        expect(openGraph).toEqual({
          title,
          description,
          url: pathname,
          siteName: "OpenMessage",
          locale: "en_US",
          images: [OG_IMAGE_ENTRY],
          ...article
        });
      })
    );
  });

  test("the image path is the route app/opengraph-image.jsx serves", () => {
    expect(OG_IMAGE).toBe("/opengraph-image");
    expect(existsSync(path.join(siteRoot, "app", `${OG_IMAGE.slice(1)}.jsx`))).toBe(true);
  });
});

describe("pages that use it", () => {
  test("the root layout sets the Twitter card type and nothing Twitter could fill from openGraph", () => {
    const twitter = read("app/layout.jsx").match(/twitter:\s*\{([^}]*)\}/);
    expect(twitter).not.toBeNull();
    expect(twitter[1]).toMatch(/card:\s*"summary_large_image"/);
    expect(twitter[1]).not.toMatch(/\b(title|description|images)\b/);
  });

  test("the card names its alt text and size from the same constants pageMetadata uses", () => {
    const card = read("app/opengraph-image.jsx");
    expect(card).toMatch(/export const alt = OG_ALT;/);
    expect(card).toMatch(/export const size = OG_SIZE;/);
    expect(OG_IMAGE_ENTRY).toEqual({ url: OG_IMAGE, width: 1200, height: 630, alt: OG_ALT, type: "image/png" });
  });

  test("the privacy page takes its metadata from pageMetadata at /privacy", () => {
    const page = read("app/privacy/page.jsx");
    expect(page).toMatch(/pageMetadata\(\{\s*title:\s*"Privacy",[\s\S]*?path:\s*"\/privacy"\s*\}\)/);
    expect(page).not.toMatch(/openGraph:/);
  });

  test("the blog index takes its metadata from pageMetadata at /blog", () => {
    const page = read("app/blog/page.jsx");
    expect(page).toMatch(/pageMetadata\(\{\s*title:\s*"Blog",\s*description,\s*path:\s*"\/blog"\s*\}\)/);
    expect(page).not.toMatch(/openGraph:/);
  });

  test("each post takes its metadata from pageMetadata and stays an article with its dates", () => {
    const post = read("components/post.jsx");
    expect(post).toMatch(/return pageMetadata\(\{/);
    expect(post).toMatch(/path:\s*`\/blog\/\$\{post\.slug\}`/);
    expect(post).toMatch(
      /openGraph:\s*\{\s*type:\s*"article",\s*publishedTime:\s*post\.published,\s*modifiedTime:\s*post\.revised\s*\}/
    );
  });
});

describe("blog chrome", () => {
  test("the index and posts tell the header which section is current", () => {
    expect(read("app/blog/page.jsx")).toContain('<SiteHeader current="blog" />');
    expect(read("components/post.jsx")).toContain('<SiteHeader current="post" />');
  });

  test("the index title, heading and links call the section Blog", () => {
    const page = read("app/blog/page.jsx");
    expect(page).toContain("<h1>Blog</h1>");
    for (const file of ["app/blog/page.jsx", "components/post.jsx", "app/not-found.jsx", "app/layout.jsx"]) {
      expect(read(file), file).not.toMatch(/engineering notes|all notes/i);
    }
  });
});
