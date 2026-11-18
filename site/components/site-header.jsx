import Link from "next/link";
import { IconBrandGithub } from "@tabler/icons-react";

import { DOWNLOAD_PATH, REPO_URL } from "../data/facts";
import { Logo } from "./logo";
import { ThemeToggle } from "./theme-toggle";

// `current` names the section a page belongs to. On the Blog index the Blog
// link points at the page itself ("page"); on a post it points at the section
// that holds the post ("true").
function blogCurrent(current) {
  if (current === "blog") {
    return "page";
  }
  return current === "post" ? "true" : undefined;
}

export function SiteHeader({ current }) {
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
          <Link className="nav__link nav__link--wide" href="/blog" aria-current={blogCurrent(current)}>
            Blog
          </Link>
          <a className="icon-btn nav__repo" href={REPO_URL} rel="noopener" aria-label="OpenMessage on GitHub">
            <IconBrandGithub stroke={1.7} aria-hidden="true" />
          </a>
          <ThemeToggle />
          <a className="btn btn--yours btn--small" href={DOWNLOAD_PATH}>
            Download
          </a>
        </nav>
      </div>
    </header>
  );
}
