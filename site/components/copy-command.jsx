"use client";

import { useState } from "react";
import { IconCheck, IconCopy } from "@tabler/icons-react";

export function CopyCommand({ command, label }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard access can be refused; the command stays selectable.
    }
  }

  return (
    <div className="command">
      <pre className="command__code">
        <code>{command}</code>
      </pre>
      <button type="button" className="command__copy" onClick={copy} aria-label={`Copy ${label}`}>
        {copied ? <IconCheck stroke={1.8} aria-hidden="true" /> : <IconCopy stroke={1.8} aria-hidden="true" />}
        <span aria-live="polite">{copied ? "Copied" : "Copy"}</span>
      </button>
    </div>
  );
}
