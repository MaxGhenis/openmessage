// Parses the text that the get_person_messages MCP tool returns, so the hero
// can show one captured tool result two ways (as bubbles and as raw text)
// without a second, hand-written copy of the conversation.
//
// It reads one message per line. A body containing a line break would come
// through as a message line plus plain lines; the demo test's count check
// (parsed messages == the tool's own "Total:") would fail if that ever happened.

const BLOCK = /^--- (.+?) \[(\w+)\] \(ID: (.+?)\) ---$/;
const MESSAGE = /^\[([^\]]+)\] (←|→) (.+?): «([\s\S]*)»$/;
const TOTAL = /^Total: (\d+) messages across (\d+) conversation\(s\)$/;

export function parseTranscript(text) {
  const lines = [];
  const messages = [];
  let block = null;
  let total = null;

  for (const raw of text.split("\n")) {
    const blockMatch = raw.match(BLOCK);
    if (blockMatch) {
      block = { name: blockMatch[1], platform: blockMatch[2], conversationId: blockMatch[3] };
      lines.push({ text: raw, kind: "block", conversationId: block.conversationId });
      continue;
    }

    const messageMatch = raw.match(MESSAGE);
    if (messageMatch && block) {
      const mine = messageMatch[2] === "→";
      const message = {
        timestamp: messageMatch[1],
        mine,
        sender: messageMatch[3],
        body: messageMatch[4],
        platform: block.platform,
        conversationId: block.conversationId,
        conversationName: block.name
      };
      messages.push(message);
      lines.push({ text: raw, kind: "message", mine, conversationId: block.conversationId });
      continue;
    }

    const totalMatch = raw.match(TOTAL);
    if (totalMatch) {
      total = { messages: Number(totalMatch[1]), conversations: Number(totalMatch[2]) };
    }

    if (raw === "") {
      block = null;
    }
    lines.push({ text: raw, kind: raw === "" ? "blank" : "meta" });
  }

  return { lines, messages, total };
}

// "2025-02-06T13:40:00-05:00" -> "1:40 PM". Reads the wall-clock time straight
// from the string, so the label matches the capture whatever the viewer's zone.
export function clockTime(timestamp) {
  const match = timestamp.match(/T(\d{2}):(\d{2})/);
  if (!match) {
    return "";
  }
  const hour = Number(match[1]);
  if (hour > 23 || Number(match[2]) > 59) {
    return "";
  }
  const suffix = hour >= 12 ? "PM" : "AM";
  const twelve = hour % 12 === 0 ? 12 : hour % 12;
  return `${twelve}:${match[2]} ${suffix}`;
}
