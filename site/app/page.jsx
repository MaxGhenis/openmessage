import Image from "next/image";

import { CommandBlock } from "../components/command-block";
import { SiteFooter } from "../components/site-footer";
import { SiteHeader } from "../components/site-header";
import { WaitlistForm } from "../components/waitlist-form";
import {
  claudeMcpCommand,
  downloadUrl,
  faqItems,
  howItWorksPoints,
  repoUrl,
  setupColumns
} from "../data/site-content";

function ActionLink({ children, href, external = false, primary = false }) {
  const className = primary
    ? "inline-flex items-center justify-center rounded-full bg-[var(--accent)] px-6 py-3.5 text-sm font-semibold text-[var(--bg-deep)] transition-transform hover:-translate-y-0.5"
    : "inline-flex items-center justify-center rounded-full border border-[var(--border)] bg-[color:rgba(8,13,24,0.72)] px-6 py-3.5 text-sm font-semibold text-[var(--text-primary)] transition-colors hover:border-[var(--border-strong)] hover:bg-[var(--bg-hover)]";
  const props = external ? { rel: "noreferrer", target: "_blank" } : {};

  return (
    <a href={href} className={className} {...props}>
      {children}
    </a>
  );
}

function SetupColumn({ column, index }) {
  return (
    <section
      className={`py-8 ${index === 0 ? "border-b border-[var(--border)] lg:border-b-0 lg:border-r lg:pr-8" : "lg:pl-8"}`}
    >
      <div className="text-[0.72rem] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
        {column.eyebrow}
      </div>
      <h3 className="mt-4 text-[2rem] font-semibold tracking-[-0.05em] text-[var(--text-primary)]">
        {column.title}
      </h3>
      <p className="mt-4 max-w-[34rem] text-base leading-7 text-[var(--text-secondary)]">
        {column.body}
      </p>

      <div className="mt-7 grid gap-4">
        {column.commands.map((command) => (
          <CommandBlock key={command.label} label={command.label}>
            {command.code}
          </CommandBlock>
        ))}
      </div>
    </section>
  );
}

export default function HomePage() {
  return (
    <main className="relative z-[1] min-h-screen">
      <SiteHeader overlay />

      {/* Hero */}
      <section className="relative overflow-hidden border-b border-[var(--border)]">
        <div className="absolute inset-0 bg-[radial-gradient(circle_at_top_left,rgba(118,137,255,0.18),transparent_34%),radial-gradient(circle_at_75%_30%,rgba(118,137,255,0.1),transparent_24%)]" />
        <div className="relative mx-auto max-w-[1520px] px-6 pb-16 pt-28 lg:px-10 lg:pb-24 lg:pt-34">
          <div className="grid gap-12 lg:grid-cols-[minmax(0,480px)_minmax(0,1fr)] lg:items-center">
            <div className="max-w-[28rem]">
              <div className="animate-fade-up flex items-center gap-3">
                <span className="rounded-full border border-[var(--border)] bg-[color:rgba(111,130,255,0.12)] px-3 py-1 text-[0.72rem] font-semibold uppercase tracking-[0.2em] text-[var(--accent-strong)]">
                  Free &amp; open source
                </span>
              </div>
              <h1 className="animate-fade-up mt-5 text-[clamp(2.6rem,5.5vw,4.4rem)] font-semibold leading-[0.94] tracking-[-0.06em] text-[var(--text-primary)] [animation-delay:60ms]">
                Google Messages, WhatsApp, and Signal in one local inbox.
              </h1>
              <p className="animate-fade-up mt-6 text-lg leading-8 text-[var(--text-secondary)] [animation-delay:120ms]">
                People stay grouped in the sidebar. Routes switch as tabs in the thread.
                Search, media, notifications, and MCP all run on the same local runtime.
              </p>

              <div className="animate-fade-up mt-8 flex flex-col gap-4 sm:flex-row [animation-delay:220ms]">
                <ActionLink href={downloadUrl} primary>
                  Download for macOS
                </ActionLink>
                <ActionLink href={repoUrl} external>
                  View the repo
                </ActionLink>
              </div>
            </div>

            <div className="relative animate-fade-up [animation-delay:180ms]">
              <div className="absolute right-[6%] top-[8%] hidden h-52 w-52 rounded-full bg-[var(--accent-glow)] blur-3xl lg:block" />
              <div className="relative overflow-hidden rounded-[2.6rem] border border-[var(--border)] bg-[color:rgba(8,13,24,0.78)] shadow-[var(--panel-shadow)]">
                <Image
                  src="/hero-product-dark.png"
                  alt="OpenMessage showing a people-first inbox with grouped routes and thread tabs"
                  width={3200}
                  height={1640}
                  priority
                  className="h-auto w-full"
                />
              </div>
            </div>
          </div>
        </div>
      </section>

      {/* Features — compact three-column grid */}
      <section id="features" className="border-b border-[var(--border)]">
        <div className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
          <div className="grid gap-10 md:grid-cols-3">
            {[
              {
                title: "People-first multi-platform",
                body: "Google Messages, WhatsApp, and Signal all land in one inbox, with one row per person and route tabs inside the thread."
              },
              {
                title: "Local-first",
                body: "Messages, contacts, and sessions stay on your machine. No cloud account required."
              },
              {
                title: "Built-in MCP",
                body: "Claude can search, draft, and send through the exact same local store you see in the app."
              }
            ].map((feature) => (
              <div key={feature.title} className="border-t border-[var(--border)] pt-6">
                <h3 className="text-[1.35rem] font-semibold tracking-[-0.04em] text-[var(--text-primary)]">
                  {feature.title}
                </h3>
                <p className="mt-3 text-base leading-7 text-[var(--text-secondary)]">
                  {feature.body}
                </p>
              </div>
            ))}
          </div>
        </div>
      </section>

      {/* MCP */}
      <section id="ai" className="border-b border-[var(--border)]">
        <div className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
          <div className="grid gap-12 lg:grid-cols-[minmax(0,520px)_minmax(0,1fr)] lg:items-center">
            <div>
              <div className="eyebrow">MCP</div>
              <h2 className="mt-5 text-[clamp(2rem,3.5vw,3rem)] font-semibold leading-[0.95] tracking-[-0.06em] text-[var(--text-primary)]">
                One runtime for the app and your assistant.
              </h2>
              <p className="mt-5 max-w-[34rem] text-base leading-7 text-[var(--text-secondary)]">
                OpenMessage exposes the same local inbox to Claude over MCP. Search threads,
                inspect contacts, draft replies, and send through the same route-aware state
                you see in the desktop app.
              </p>
            </div>
            <div className="grid gap-6">
              <div className="max-w-[44rem]">
                <CommandBlock label="Connect Claude">{claudeMcpCommand}</CommandBlock>
              </div>
              <div className="overflow-hidden rounded-[2rem] border border-[var(--border)] bg-[color:rgba(8,13,24,0.78)] shadow-[var(--panel-shadow)]">
                <Image
                  src="/hero-command-surface.png"
                  alt="OpenMessage alongside Claude using MCP to search and draft messages"
                  width={1800}
                  height={1020}
                  className="h-auto w-full"
                />
              </div>
            </div>
          </div>
        </div>
      </section>

      {/* How it works */}
      <section id="how-it-works" className="border-b border-[var(--border)]">
        <div className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
          <div className="grid gap-14 lg:grid-cols-[minmax(0,420px)_minmax(0,1fr)]">
            <div>
              <div className="eyebrow">How it works</div>
              <h2 className="mt-5 max-w-[24rem] text-[clamp(2rem,3.5vw,3rem)] font-semibold leading-[0.95] tracking-[-0.06em] text-[var(--text-primary)]">
                Local routes. One message store.
              </h2>
              <p className="mt-5 max-w-[28rem] text-base leading-7 text-[var(--text-secondary)]">
                Each platform syncs into the same local thread model, search index, notifications,
                and MCP surface instead of hiding behind separate browser mirrors or desktop wrappers.
              </p>
              <div className="mt-7">
                <ActionLink href="/blog/how-openmessage-added-whatsapp">
                  Read the technical write-up
                </ActionLink>
              </div>
            </div>

            <div className="grid gap-8 md:grid-cols-3">
              {howItWorksPoints.map((point) => (
                <div key={point.title} className="border-t border-[var(--border)] pt-6">
                  <h3 className="text-[1.22rem] font-semibold tracking-[-0.04em] text-[var(--text-primary)]">
                    {point.title}
                  </h3>
                  <p className="mt-3 text-base leading-7 text-[var(--text-secondary)]">
                    {point.body}
                  </p>
                </div>
              ))}
            </div>
          </div>
        </div>
      </section>

      {/* Setup */}
      <section id="setup" className="border-b border-[var(--border)]">
        <div className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
          <div className="eyebrow">Setup</div>
          <h2 className="mt-5 max-w-[34rem] text-[clamp(2rem,3.5vw,3rem)] font-semibold leading-[0.95] tracking-[-0.06em] text-[var(--text-primary)]">
            Native app or CLI — same product.
          </h2>

          <div className="mt-14 grid border-y border-[var(--border)] lg:grid-cols-2">
            {setupColumns.map((column, index) => (
              <SetupColumn key={column.title} column={column} index={index} />
            ))}
          </div>
        </div>
      </section>

      {/* FAQ */}
      <section id="faq" className="border-b border-[var(--border)]">
        <div className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
          <div className="grid gap-14 lg:grid-cols-[minmax(0,420px)_minmax(0,1fr)]">
            <div>
              <div className="eyebrow">FAQ</div>
              <h2 className="mt-5 max-w-[24rem] text-[clamp(2rem,3.5vw,3rem)] font-semibold leading-[0.95] tracking-[-0.06em] text-[var(--text-primary)]">
                The WhatsApp question, answered once.
              </h2>
              <p className="mt-5 max-w-[28rem] text-base leading-7 text-[var(--text-secondary)]">
                Most people want to know whether OpenMessage is live, local, and trustworthy.
                That is the right lens.
              </p>
            </div>

            <div className="grid gap-4">
              {faqItems.map((item) => (
                <details
                  key={item.question}
                  className="group rounded-[1.6rem] border border-[var(--border)] bg-[color:rgba(9,17,29,0.7)] px-6 py-5"
                >
                  <summary className="cursor-pointer list-none text-[1.05rem] font-semibold tracking-[-0.03em] text-[var(--text-primary)] marker:hidden">
                    <span className="flex items-center justify-between gap-6">
                      <span>{item.question}</span>
                      <span className="text-[var(--text-muted)] transition-transform group-open:rotate-45">
                        +
                      </span>
                    </span>
                  </summary>
                  <p className="mt-4 max-w-[44rem] text-base leading-7 text-[var(--text-secondary)]">
                    {item.answer}
                    {item.question === "Where can I read the technical details?" ? (
                      <>
                        {" "}
                        <a
                          href="/blog/how-openmessage-added-whatsapp"
                          className="text-[var(--accent-strong)] transition-colors hover:text-[var(--text-primary)]"
                        >
                          Read the post.
                        </a>
                      </>
                    ) : null}
                  </p>
                </details>
              ))}
            </div>
          </div>
        </div>
      </section>

      {/* CTA */}
      <section id="updates" className="mx-auto max-w-[1520px] px-6 py-20 lg:px-10">
        <div className="border-y border-[var(--border)] py-12">
          <div className="grid gap-10 lg:grid-cols-[minmax(0,1fr)_minmax(340px,420px)] lg:items-start">
            <div>
              <div className="eyebrow">Launch updates</div>
              <h2 className="mt-5 max-w-[30rem] text-[clamp(2rem,3.5vw,3rem)] font-semibold leading-[0.95] tracking-[-0.06em] text-[var(--text-primary)]">
                Google Messages, WhatsApp, and Signal in one local desktop app.
              </h2>
              <p className="mt-5 max-w-[34rem] text-base leading-7 text-[var(--text-secondary)]">
                Download the current build now, or leave an email if you want the next meaningful update without watching the repo every day.
              </p>
              <div className="mt-7 flex flex-col gap-4 sm:flex-row">
                <ActionLink href={downloadUrl} primary>
                  Download OpenMessage
                </ActionLink>
                <ActionLink href={repoUrl} external>
                  Read the code
                </ActionLink>
              </div>
            </div>
            <WaitlistForm />
          </div>
        </div>
      </section>

      <SiteFooter />
    </main>
  );
}
