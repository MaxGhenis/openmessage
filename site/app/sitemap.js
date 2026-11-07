import { BLOG_POSTS, MAIN_CHECKED_ON, SITE_URL } from "../data/facts";

export default function sitemap() {
  const checked = new Date(`${MAIN_CHECKED_ON}T00:00:00Z`);
  return [
    { url: `${SITE_URL}/`, lastModified: checked },
    { url: `${SITE_URL}/privacy`, lastModified: checked },
    { url: `${SITE_URL}/blog`, lastModified: checked },
    ...BLOG_POSTS.map((post) => ({
      url: `${SITE_URL}/blog/${post.slug}`,
      lastModified: new Date(`${post.revised ?? post.published}T00:00:00Z`)
    }))
  ];
}
