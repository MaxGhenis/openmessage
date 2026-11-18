import { Post, postMetadata } from "../../../components/post";
import { Receipts } from "../../../components/receipts";
import { RELEASE } from "../../../data/facts";

const slug = "how-openmessage-added-google-messages";

export const metadata = postMetadata(slug);

export default function GoogleMessagesPost() {
  return (
    <Post
      slug={slug}
      lead="OpenMessage connected to Google Messages first. Google Messages carries SMS and RCS from an Android phone, and the way OpenMessage handles it set the pattern for WhatsApp and Signal."
    >
      <p>
        In April I wrote that the Google connection recovered cleanly when something went wrong.
        Version {RELEASE.version} cannot renew an expired Google session, so you pair again when
        one lapses.
      </p>

      <h2>Pairing</h2>
      <p>
        OpenMessage links to your phone the way Google Messages for web does. It uses a fork of
        the open-source mautrix-gmessages library, which Google does not support. You can pair
        by scanning a QR code with the Google Messages app. You can also give OpenMessage the
        cookies your browser holds for <code>messages.google.com</code>, then tap a matching emoji
        on your phone.
      </p>
      <Receipts
        ids={["gmessagesFork", "googleQrPairing", "googleCookiePairing", "googleEmojiConfirm"]}
      />
      <p>
        <a
          href="https://support.google.com/messages/answer/9077245?co=GENIE.Platform%3DDesktop&hl=en"
          rel="noopener"
        >
          Google no longer offers QR pairing
        </a>{" "}
        in the US, so there you pair with cookies. Code on <code>main</code> drops QR setup and
        sets up Google Messages through your Google account instead.
      </p>
      <Receipts ids={["googleQrRetired"]} />

      <h2>The live event path</h2>
      <p>
        After you pair, the backend holds a session with Google and receives events as they happen.
        It writes each incoming message to the local database and tells the app which
        conversation changed, so the thread redraws.
      </p>
      <Receipts ids={["googleHandleMessage", "googleStoreMessage", "googleNotifyUi"]} />

      <h2>One table for every network</h2>
      <p>
        OpenMessage writes Google Messages rows to the same <code>messages</code> table that
        WhatsApp and Signal use, and tags each row with its network. Search, the sidebar and the MCP tools read
        that table, so one query or one tool call covers all three.
      </p>
      <Receipts ids={["messagesDb", "searchIndex", "toolRegistry"]} />

      <h2>What breaks</h2>
      <p>
        The cookies behind a Google account session rotate, and a session can expire after the
        backend has been offline for a while. Code on <code>main</code> refreshes the session from
        your local Chrome profile and reconnects with backoff; it has not shipped in a release
        yet.
      </p>
      <Receipts ids={["googleRefresh", "supervisorBackoff"]} />
      <p>
        On <code>main</code>, OpenMessage compares each network&rsquo;s silence with its own recent
        traffic and flags a network that has gone quiet for longer than that history explains.
      </p>
      <Receipts ids={["silenceDefaults"]} />
    </Post>
  );
}
