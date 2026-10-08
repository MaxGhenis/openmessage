import Link from "next/link";

import { SiteFooter } from "../../components/site-footer";
import { SiteHeader } from "../../components/site-header";
import { BLOG_POSTS } from "../../data/facts";
import { formatDate } from "../../lib/release";

export const metadata = {
  title: "Engineering notes",
  description: "How each network connection in OpenMessage works, with links to the code.",
  alternates: { canonical: "/blog" }
};

export default function BlogIndexPage() {
  return (
    <>
      <SiteHeader />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>Engineering notes</h1>
          <p>How each network connection works, with links to the code.</p>
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
