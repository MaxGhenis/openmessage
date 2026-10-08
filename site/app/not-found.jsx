import Link from "next/link";

import { SiteFooter } from "../components/site-footer";
import { SiteHeader } from "../components/site-header";

export const metadata = { title: "Page not found" };

export default function NotFound() {
  return (
    <>
      <SiteHeader />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>There is no page at this address</h1>
          <p>
            Go to the <Link href="/">homepage</Link>, the <Link href="/blog">engineering notes</Link>{" "}
            or the <Link href="/privacy">privacy page</Link>.
          </p>
        </header>
      </main>
      <SiteFooter />
    </>
  );
}
