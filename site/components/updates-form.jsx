"use client";

import { useState } from "react";

// Posts to /api/waitlist, which stores the address encrypted (AES-256-GCM).
// The route's response keys (`error`, `message`) are part of its contract.
export function UpdatesForm() {
  const [email, setEmail] = useState("");
  const [website, setWebsite] = useState("");
  const [state, setState] = useState("idle");
  const [message, setMessage] = useState("");

  async function submit(event) {
    event.preventDefault();
    if (state === "submitting") {
      return;
    }
    setState("submitting");
    setMessage("");

    try {
      const response = await fetch("/api/waitlist", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, interest: "general", website })
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) {
        throw new Error(payload?.error || "Could not save your email. Try again shortly.");
      }
      setState("success");
      setMessage("Saved. You'll get an email when a release ships.");
      setEmail("");
      setWebsite("");
    } catch (error) {
      setState("error");
      setMessage(error.message || "Could not save your email. Try again shortly.");
    }
  }

  return (
    <form className="updates" onSubmit={submit} noValidate={false}>
      <div className="updates__row">
        <label className="visually-hidden" htmlFor="updates-email">
          Email address
        </label>
        <input
          id="updates-email"
          className="field"
          type="email"
          name="email"
          autoComplete="email"
          placeholder="you@example.com"
          required
          value={email}
          onChange={(event) => setEmail(event.target.value)}
        />
        {/* Honeypot: people never see or fill this; the route discards any submission that does. */}
        <div className="hp" aria-hidden="true">
          <label htmlFor="updates-website">Website</label>
          <input
            id="updates-website"
            type="text"
            name="website"
            tabIndex={-1}
            autoComplete="off"
            value={website}
            onChange={(event) => setWebsite(event.target.value)}
          />
        </div>
        <button type="submit" className="btn btn--quiet" disabled={state === "submitting"}>
          {state === "submitting" ? "Saving" : "Email me releases"}
        </button>
      </div>
      <p className="updates__status" data-state={state} role="status" aria-live="polite">
        {message}
      </p>
    </form>
  );
}
