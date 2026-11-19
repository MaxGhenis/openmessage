import path from "node:path";
import { fileURLToPath } from "node:url";

const dirname = path.dirname(fileURLToPath(import.meta.url));

// The CSP sets no script-src or default-src. The App Router inlines its RSC
// payload scripts beside the theme script in app/layout.jsx, and nonces would
// force dynamic rendering and drop the homepage's revalidation. The
// directives it does set block plugins, <base> rewrites, form posts to other
// origins and framing (the last matching X-Frame-Options).
const CONTENT_SECURITY_POLICY = "object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'";

const securityHeaders = [
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
  { key: "Content-Security-Policy", value: CONTENT_SECURITY_POLICY }
];

const nextConfig = {
  turbopack: {
    root: dirname
  },
  async headers() {
    return [{ source: "/:path*", headers: securityHeaders }];
  },
  async redirects() {
    return [
      // The investor memo that used to live here was marked "not for public
      // distribution"; send old links to the homepage instead of a 404.
      { source: "/thesis", destination: "/", permanent: false }
    ];
  }
};

export default nextConfig;
