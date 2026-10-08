// Prints the release-email list: one line per signup, oldest first.
//
// The site stores each signup as an AES-256-GCM record in Vercel Blob
// (app/api/waitlist/route.js). This reads them back with the same key.
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

  signups.sort((a, b) => `${a.submittedAt}`.localeCompare(`${b.submittedAt}`));
  for (const signup of signups) {
    console.log(`${signup.submittedAt}\t${signup.email}\t${signup.interest}`);
  }
  console.error(`${signups.length} signup(s).`);
}

if (import.meta.main) {
  await main();
}
