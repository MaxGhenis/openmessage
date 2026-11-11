import Link from "next/link";

import { RELEASES_URL, REPO_URL } from "../data/facts";

export function SiteFooter() {
  return (
    <footer className="foot">
      <div className="wrap foot__inner">
        <p>
          OpenMessage is free and open source, made by{" "}
          <a href="https://maxghenis.com" rel="noopener">
            Max Ghenis
          </a>{" "}
          and{" "}
          <a href={`${REPO_URL}/graphs/contributors`} rel="noopener">
            contributors
          </a>
          .
        </p>
        <nav className="foot__links" aria-label="Footer">
          <a href={REPO_URL} rel="noopener">
            GitHub
          </a>
          <a href={RELEASES_URL} rel="noopener">
            Releases
          </a>
          <Link href="/blog">Blog</Link>
          <Link href="/privacy">Privacy</Link>
          <a href={`${REPO_URL}/blob/main/LICENSE`} rel="noopener">
            License
          </a>
          <a href="https://github.com/sponsors/MaxGhenis" rel="noopener">
            Sponsor
          </a>
        </nav>
      </div>
    </footer>
  );
}
