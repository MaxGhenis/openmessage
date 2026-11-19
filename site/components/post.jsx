import Link from "next/link";

import { BLOG_POSTS } from "../data/facts";
import { pageMetadata } from "../lib/metadata";
import { formatDate } from "../lib/release";
import { SiteFooter } from "./site-footer";
import { SiteHeader } from "./site-header";

export function postMetadata(slug) {
  const post = BLOG_POSTS.find((candidate) => candidate.slug === slug);
  return pageMetadata({
    title: post.title,
    description: post.description,
    path: `/blog/${post.slug}`,
    openGraph: { type: "article", publishedTime: post.published, modifiedTime: post.revised }
  });
}

export function Post({ slug, lead, children }) {
  const post = BLOG_POSTS.find((candidate) => candidate.slug === slug);
  return (
    <>
      <SiteHeader current="post" />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>{post.title}</h1>
          <p>{lead}</p>
          <p className="dateline">
            <time dateTime={post.published}>{formatDate(`${post.published}T00:00:00Z`)}</time>
            {post.revised && (
              <>
                , revised <time dateTime={post.revised}>{formatDate(`${post.revised}T00:00:00Z`)}</time>
              </>
            )}
          </p>
        </header>
        <article className="prose">
          {children}
          <p>
            <Link href="/blog">All posts</Link>
          </p>
        </article>
      </main>
      <SiteFooter />
    </>
  );
}
