import { SITE_URL } from "../data/facts";

export default function robots() {
  return {
    rules: [{ userAgent: "*", allow: "/", disallow: ["/api/", "/download"] }],
    sitemap: `${SITE_URL}/sitemap.xml`
  };
}
