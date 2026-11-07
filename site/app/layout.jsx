import { Atkinson_Hyperlegible_Mono, Atkinson_Hyperlegible_Next } from "next/font/google";

import { SITE_URL } from "../data/facts";
import "./globals.css";

// One family in two registers: Next for what people write, Mono for what the
// agent and the database see. next/font downloads both at build time and serves
// them from this origin, so the page makes no font request to Google.
const sans = Atkinson_Hyperlegible_Next({
  subsets: ["latin"],
  variable: "--font-atkinson",
  display: "swap",
  adjustFontFallback: false
});

const mono = Atkinson_Hyperlegible_Mono({
  subsets: ["latin"],
  variable: "--font-atkinson-mono",
  display: "swap",
  adjustFontFallback: false
});

const title = "OpenMessage: a message database for your agent";
const description =
  "OpenMessage keeps your SMS, RCS, WhatsApp and Signal messages in one SQLite database on your Mac, with a desktop app and a local MCP server on the same file.";

export const metadata = {
  metadataBase: new URL(SITE_URL),
  title: {
    default: title,
    template: "%s | OpenMessage"
  },
  description,
  alternates: { canonical: "/" },
  openGraph: {
    title,
    description,
    url: SITE_URL,
    siteName: "OpenMessage",
    locale: "en_US",
    type: "website"
  },
  twitter: {
    card: "summary_large_image",
    title,
    description
  }
};

export const viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f4f6fa" },
    { media: "(prefers-color-scheme: dark)", color: "#09111c" }
  ]
};

// Applies a stored theme choice before first paint, so a visitor who chose
// light on a dark system (or the reverse) never sees a flash of the other.
const themeScript = `try{var t=localStorage.getItem("om-theme");if(t==="dark"||t==="light")document.documentElement.setAttribute("data-theme",t)}catch(e){}`;

export default function RootLayout({ children }) {
  return (
    <html lang="en" className={`${sans.variable} ${mono.variable}`} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body>
        <a className="skip" href="#main">
          Skip to content
        </a>
        {children}
      </body>
    </html>
  );
}
