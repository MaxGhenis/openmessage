import { Post, postMetadata } from "../../../components/post";
import { Receipts } from "../../../components/receipts";
import { RELEASE } from "../../../data/facts";

const slug = "how-openmessage-added-whatsapp";

export const metadata = postMetadata(slug);

export default function WhatsAppPost() {
  return (
    <Post
      slug={slug}
      lead={`OpenMessage connects to WhatsApp as a linked device, from a session on your Mac. Version ${RELEASE.version} syncs new messages, receipts and the history WhatsApp sends a newly linked device.`}
    >
      <h2>A linked device on your Mac</h2>
      <p>
        You scan a QR code from WhatsApp on your phone, as you would for WhatsApp Web. OpenMessage
        then holds its own linked-device session through whatsmeow, an open-source library that
        WhatsApp does not support. OpenMessage keeps the session&rsquo;s keys in their own SQLite
        file beside the message database.
      </p>
      <Receipts ids={["whatsmeowLibrary", "whatsappQrPairing", "whatsappSessionStore"]} />

      <h2>What it syncs</h2>
      <p>
        New messages arrive as events, and OpenMessage writes them to the same{" "}
        <code>messages</code> table as SMS and Signal. OpenMessage also handles delivery receipts and
        typing notices from the other person, and imports each batch of history WhatsApp sends. You
        can send text, replies, attachments and reactions.
      </p>
      <Receipts ids={["whatsappReceipts", "whatsappTyping", "whatsappHistory"]} />

      <h2>What it leaves out</h2>
      <ul>
        <li>OpenMessage skips encrypted reactions from other people.</li>
        <li>Marking a thread read in OpenMessage does not send a read receipt.</li>
        <li>An agent&rsquo;s send goes out with no confirmation step.</li>
      </ul>
      <Receipts ids={["whatsappEncReaction"]} />

      <h2>The account risk</h2>
      <p>
        <a href="https://www.whatsapp.com/legal/terms-of-service" rel="noopener">
          WhatsApp&rsquo;s terms
        </a>{" "}
        forbid reverse engineering its service and let it suspend an account at any time. A
        linked-device client built on whatsmeow breaks those terms, so pair an account you could
        stand to lose.
      </p>
    </Post>
  );
}
