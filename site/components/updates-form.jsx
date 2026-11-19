"use client";

import { useEffect, useRef, useState } from "react";

// Posts to /api/waitlist, which stores the address encrypted (AES-256-GCM).
// The route's response keys are part of its contract: `message` on success,
// `error` on failure, and `field: "email"` when it rejects the address itself.
// THANKS matches WAITLIST_THANKS in lib/public-writes.mjs; tests/routes.test.js
// checks that.
const THANKS = "Thanks. I'll email you when a release ships.";
const FAILED = "The site couldn't save your address. Try again shortly.";

export function UpdatesForm() {
  const [email, setEmail] = useState("");
  const [website, setWebsite] = useState("");
  const [state, setState] = useState("idle");
  const [message, setMessage] = useState("");
  const [invalid, setInvalid] = useState(false);
  const field = useRef(null);
  const sending = state === "submitting";

  // When the route rejects the address, focus returns to the field, which
  // then reports itself invalid and reads the error through aria-describedby.
  useEffect(() => {
    if (invalid) {
      field.current?.focus();
    }
  }, [invalid]);

  async function submit(event) {
    event.preventDefault();
    // The button keeps focus while sending (aria-disabled, not disabled), so
    // a repeat press lands here and does nothing.
    if (sending) {
      return;
    }
    setState("submitting");
    setMessage("");
    setInvalid(false);

    try {
      const response = await fetch("/api/waitlist", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, interest: "general", website })
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) {
        setState("error");
        setMessage(payload?.error || FAILED);
        setInvalid(response.status === 400 && payload?.field === "email");
        return;
      }
      setState("success");
      setMessage(payload?.message || THANKS);
      setEmail("");
      setWebsite("");
    } catch {
      setState("error");
      setMessage(FAILED);
    }
  }

  return (
    <form className="updates" onSubmit={submit} noValidate={false}>
      <div className="updates__row">
        <label className="visually-hidden" htmlFor="updates-email">
          Email address
        </label>
        <input
          ref={field}
          id="updates-email"
          className="field"
          type="email"
          name="email"
          autoComplete="email"
          placeholder="you@example.com"
          required
          aria-invalid={invalid ? "true" : undefined}
          aria-describedby={invalid ? "updates-status" : undefined}
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
        <button type="submit" className="btn btn--quiet" aria-disabled={sending ? "true" : undefined}>
          {sending ? "Saving" : "Email me about releases"}
        </button>
      </div>
      <p id="updates-status" className="updates__status" data-state={state} role="status" aria-live="polite">
        {message}
      </p>
    </form>
  );
}
