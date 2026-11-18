// Prints the release-email list: one line per address, oldest first.
//
// The site stores each signup as an AES-256-GCM record in Vercel Blob
// (lib/public-writes.mjs), one per address per UTC day. This reads them back
// with the same key and keeps each address's latest record.
//
//   vercel env pull .env.local --environment production --scope max-ghenis-projects
//   bun --env-file=.env.local scripts/read-waitlist.mjs
//
// It only reads. Output contains email addresses; keep it off shared screens.

import { createDecipheriv } from "node:crypto";

import { list } from "@vercel/blob";

export function decryptRecord(record, keyHex) {
  const key = Buffer.from(keyHex, "hex");
  if (key.length !== 32) {
    throw new Error("WAITLIST_ENCRYPTION_KEY must be 32 bytes (64 hex characters).");
  }
  if (record?.version !== 1 || record.algorithm !== "aes-256-gcm") {
    throw new Error("Unrecognized waitlist record format.");
  }
  const decipher = createDecipheriv("aes-256-gcm", key, Buffer.from(record.iv, "base64"));
  decipher.setAuthTag(Buffer.from(record.tag, "base64"));
  const plaintext = Buffer.concat([
    decipher.update(Buffer.from(record.ciphertext, "base64")),
    decipher.final()
  ]);
  return JSON.parse(plaintext.toString("utf8"));
}

// One record per address: the latest, so a later sign-up's interest wins.
// Sorted by that record's time, oldest first.
export function latestPerAddress(signups) {
  const latest = new Map();
  for (const signup of signups) {
    const kept = latest.get(signup.email);
    if (!kept || Date.parse(signup.submittedAt) > Date.parse(kept.submittedAt)) {
      latest.set(signup.email, signup);
    }
  }
  return [...latest.values()].sort((a, b) => Date.parse(a.submittedAt) - Date.parse(b.submittedAt));
}

// Replaces each control or format character (terminal escapes, bidi
// overrides, zero-width characters) with "?", so a stored address can't hide
// output, rewrite the window title or plant a link in the terminal that
// prints it. The route rejects such addresses; this covers older records.
export function printable(value) {
  return `${value ?? ""}`.replace(/[\p{Cc}\p{Cf}]/gu, "?");
}

async function main() {
  const token = process.env.BLOB_READ_WRITE_TOKEN;
  const keyHex = process.env.WAITLIST_ENCRYPTION_KEY;
  if (!token || !keyHex) {
    console.error("Set BLOB_READ_WRITE_TOKEN and WAITLIST_ENCRYPTION_KEY (see the header of this file).");
    process.exit(1);
  }

  const signups = [];
  let cursor;
  do {
    const page = await list({ prefix: "waitlist/", cursor, token });
    for (const blob of page.blobs) {
      const response = await fetch(blob.url);
      if (!response.ok) {
        console.error(`Could not read ${blob.pathname}: HTTP ${response.status}`);
        continue;
      }
      try {
        signups.push(decryptRecord(await response.json(), keyHex));
      } catch (error) {
        console.error(`Could not decrypt ${blob.pathname}: ${error.message}`);
      }
    }
    cursor = page.hasMore ? page.cursor : undefined;
  } while (cursor);

  const addresses = latestPerAddress(signups);
  for (const signup of addresses) {
    console.log([signup.submittedAt, signup.email, signup.interest].map(printable).join("\t"));
  }
  console.error(`${addresses.length} address(es) from ${signups.length} record(s).`);
}

if (import.meta.main) {
  await main();
}
