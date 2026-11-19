import Link from "next/link";

import { SiteFooter } from "../../components/site-footer";
import { SiteHeader } from "../../components/site-header";
import { BLOG_POSTS } from "../../data/facts";
import { pageMetadata } from "../../lib/metadata";
import { formatDate } from "../../lib/release";

const description = "How OpenMessage connects to Google Messages and WhatsApp, with links to the code.";

export const metadata = pageMetadata({ title: "Blog", description, path: "/blog" });

export default function BlogIndexPage() {
  return (
    <>
      <SiteHeader current="blog" />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>Blog</h1>
          <p>{description}</p>
        </header>
        <div className="post-list">
          {BLOG_POSTS.map((post) => (
            <Link key={post.slug} href={`/blog/${post.slug}`}>
              <h2>{post.title}</h2>
              <p>{post.description}</p>
              <p className="dateline">
                <time dateTime={post.published}>{formatDate(`${post.published}T00:00:00Z`)}</time>
              </p>
            </Link>
          ))}
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
