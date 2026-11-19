// Metadata for a page below the root layout. Next.js replaces the layout's
// openGraph object whole when a page sets its own, which drops the image from
// app/opengraph-image.jsx, so each page names its title, description, URL and
// image here. The layout sets only the Twitter card type, so Next.js fills the
// Twitter title, description and image from this openGraph.
export const OG_IMAGE = "/opengraph-image";
export const OG_ALT =
  "OpenMessage: your texts, WhatsApp and Signal in one SQLite database your agent can query";
export const OG_SIZE = { width: 1200, height: 630 };
// The file-based card's entry, which a page's own openGraph replaces: its
// size, alt text and type as Next.js emits them. Next.js adds a ?<hash> cache
// key to the card's URL; this entry uses the bare route, which serves the same
// image.
export const OG_IMAGE_ENTRY = { url: OG_IMAGE, ...OG_SIZE, alt: OG_ALT, type: "image/png" };

export function pageMetadata({ title, description, path, openGraph = {} }) {
  return {
    title,
    description,
    alternates: { canonical: path },
    openGraph: {
      title,
      description,
      url: path,
      siteName: "OpenMessage",
      locale: "en_US",
      type: "website",
      images: [{ ...OG_IMAGE_ENTRY }],
      ...openGraph
    }
  };
}
