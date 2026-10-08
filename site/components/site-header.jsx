import Link from "next/link";
import { IconBrandGithub } from "@tabler/icons-react";

import { DOWNLOAD_PATH, REPO_URL } from "../data/facts";
import { Logo } from "./logo";
import { ThemeToggle } from "./theme-toggle";

export function SiteHeader() {
  return (
    <header className="masthead">
      <div className="wrap masthead__inner">
        <Link className="wordmark" href="/">
          <Logo />
          OpenMessage
        </Link>
        <nav className="nav" aria-label="Primary">
          <Link className="nav__link nav__link--wide" href="/#app">
            App
          </Link>
          <Link className="nav__link nav__link--wide" href="/#agents">
            Agents
          </Link>
          <Link className="nav__link nav__link--wide" href="/#status">
            Status
          </Link>
          <Link className="nav__link nav__link--wide" href="/blog">
            Blog
          </Link>
          <a className="icon-btn" href={REPO_URL} rel="noopener" aria-label="OpenMessage on GitHub">
            <IconBrandGithub stroke={1.7} aria-hidden="true" />
          </a>
          <ThemeToggle />
          <a className="btn btn--yours btn--small" href={`${DOWNLOAD_PATH}?source=site_header`}>
            Download
          </a>
        </nav>
      </div>
    </header>
  );
}
