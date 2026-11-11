"use client";

import { useRef, useState } from "react";

// One captured tool result, read two ways. The left pane draws the selected
// route's messages as a thread; the right pane is the tool's text, with the
// same route's lines brought forward. Both come from the same parsed lines, so
// they cannot disagree.
export function TwoReaders({ person, call, routes, lines, initialRoute }) {
  const [selected, setSelected] = useState(initialRoute);
  const resultRef = useRef(null);
  const tabRefs = useRef({});
  const active = routes.find((route) => route.platform === selected) ?? routes[0];

  function select(platform, { focus = false } = {}) {
    setSelected(platform);
    if (focus) {
      tabRefs.current[platform]?.focus();
    }
    const route = routes.find((candidate) => candidate.platform === platform);
    const pane = resultRef.current;
    const first = pane?.querySelector(`[data-conversation="${CSS.escape(route.conversationId)}"]`);
    if (pane && first) {
      const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      pane.scrollTo({ top: Math.max(first.offsetTop - pane.offsetTop - 16, 0), behavior: reduce ? "auto" : "smooth" });
    }
  }

  function onKeyDown(event) {
    const index = routes.findIndex((route) => route.platform === selected);
    const step = { ArrowRight: 1, ArrowLeft: -1 }[event.key];
    if (step) {
      event.preventDefault();
      select(routes[(index + step + routes.length) % routes.length].platform, { focus: true });
    } else if (event.key === "Home") {
      event.preventDefault();
      select(routes[0].platform, { focus: true });
    } else if (event.key === "End") {
      event.preventDefault();
      select(routes[routes.length - 1].platform, { focus: true });
    }
  }

  return (
    <div className="readers__frame">
      <section className="readers__pane" aria-label="The thread as you see it in the app">
        <div className="readers__head">
          <span className="readers__who">You, in the app</span>
          <div className="routes" role="tablist" aria-label={`Routes to ${person}`} onKeyDown={onKeyDown}>
            {routes.map((route) => (
              <button
                key={route.platform}
                ref={(node) => {
                  tabRefs.current[route.platform] = node;
                }}
                type="button"
                role="tab"
                id={`route-tab-${route.platform}`}
                className="routes__tab"
                aria-selected={route.platform === active.platform}
                aria-controls="route-panel"
                tabIndex={route.platform === active.platform ? 0 : -1}
                onClick={() => select(route.platform)}
              >
                {route.label}
              </button>
            ))}
          </div>
        </div>
        <div className="thread" role="tabpanel" id="route-panel" aria-labelledby={`route-tab-${active.platform}`}>
          <p className="thread__person">{person}</p>
          {active.messages.map((message, index) => (
            <p key={index} className={message.mine ? "bubble bubble--mine" : "bubble"}>
              <span className="visually-hidden">{message.mine ? "You: " : `${person}: `}</span>
              {message.body}
              <span className="bubble__time">{message.time}</span>
            </p>
          ))}
        </div>
      </section>

      <section className="readers__pane" aria-label="The same messages as an MCP tool returns them">
        <div className="readers__head">
          <span className="readers__who">Your agent, over MCP</span>
          <code className="readers__call">{call}</code>
        </div>
        <pre className="result" ref={resultRef} tabIndex={0} aria-label="Tool result">
          {lines.map((line, index) => {
            const inRoute = line.conversationId === active.conversationId;
            const names = ["result__line"];
            if (line.kind === "meta") {
              names.push("result__line--always");
            }
            if (inRoute) {
              names.push("result__line--on");
            }
            if (line.mine) {
              names.push("result__line--mine");
            }
            return (
              <span
                key={index}
                className={names.join(" ")}
                data-conversation={line.kind === "block" ? line.conversationId : undefined}
              >
                {line.text}
                {"\n"}
              </span>
            );
          })}
        </pre>
      </section>
    </div>
  );
}
