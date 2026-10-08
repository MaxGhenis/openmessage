import { Post, postMetadata } from "../../../components/post";
import { Receipts } from "../../../components/receipts";
import { RELEASE } from "../../../data/facts";

const slug = "how-openmessage-added-whatsapp";

export const metadata = postMetadata(slug);

export default function WhatsAppPost() {
  return (
    <Post
      slug={slug}
      lead="WhatsApp in OpenMessage is a linked-device session that runs on your Mac. This note covers what it syncs in the current release and what it leaves out."
    >
      <p>
        This note describes version {RELEASE.version}. The April original has been replaced.
      </p>

      <h2>A linked device on your Mac</h2>
      <p>
        You scan a QR code from WhatsApp on your phone, as you would for WhatsApp Web. OpenMessage
        then holds its own linked-device session through whatsmeow, an open-source library that
        WhatsApp does not support. The session&rsquo;s keys are kept in their own SQLite file
        beside the message database.
      </p>
      <Receipts ids={["whatsmeowLibrary", "whatsappQrPairing", "whatsappSessionStore"]} />

      <h2>What it syncs</h2>
      <p>
        New messages arrive as events and are written to the same <code>messages</code> table as
        SMS and Signal. The session also handles delivery receipts, typing notices from the other
        person, and the history WhatsApp sends a newly linked device. You can send text, replies,
        attachments and reactions.
      </p>
      <Receipts ids={["whatsappReceipts", "whatsappTyping", "whatsappHistory"]} />

      <h2>What it leaves out</h2>
      <ul>
        <li>Encrypted reactions from other people are skipped on receipt.</li>
        <li>Marking a thread read in OpenMessage does not send a read receipt.</li>
        <li>There is no confirmation step before an agent&rsquo;s send goes out.</li>
      </ul>
      <Receipts ids={["whatsappEncReaction"]} />

      <h2>The account risk</h2>
      <p>
        <a href="https://www.whatsapp.com/legal/terms-of-service" rel="noopener">
          WhatsApp&rsquo;s terms
        </a>{" "}
        forbid reverse engineering its service and let it suspend an account at any time. A
        linked-device client built on whatsmeow falls on the wrong side of that. Pair an account
        you could stand to lose.
      </p>
    </Post>
  );
}
