"use client";

import { useEffect, useRef, useState } from "react";
import { IconCheck, IconCopy } from "@tabler/icons-react";

// The status line sits outside the button: a button's children are
// presentational, so Safari never exposes a live region placed inside one.
// It renders empty from the start, so screen readers register the region
// before its text changes.
export function CopyCommand({ command, label }) {
  const [copied, setCopied] = useState(false);
  const [status, setStatus] = useState("");
  const timer = useRef(null);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function copy() {
    clearTimeout(timer.current);
    // Emptying the region first makes a repeat of the same message announce again.
    setStatus("");
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setStatus(`Your clipboard now holds ${label}`);
      timer.current = setTimeout(() => {
        setCopied(false);
        setStatus("");
      }, 2000);
    } catch {
      // Clipboard access can be refused; the command stays selectable, and the
      // message stays until the next try.
      setCopied(false);
      setStatus("Your browser blocked the copy. Select the command and copy it.");
    }
  }

  return (
    <div className="command">
      <pre className="command__code">
        <code>{command}</code>
      </pre>
      <button type="button" className="command__copy" onClick={copy} aria-label={`Copy ${label}`}>
        {copied ? <IconCheck stroke={1.8} aria-hidden="true" /> : <IconCopy stroke={1.8} aria-hidden="true" />}
        <span>{copied ? "Copied" : "Copy"}</span>
      </button>
      <span className="visually-hidden" role="status">
        {status}
      </span>
    </div>
  );
}
