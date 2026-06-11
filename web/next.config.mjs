/** @type {import('next').NextConfig} */
const nextConfig = {
  // Emit a self-contained server bundle (.next/standalone) so the Docker runtime
  // image carries only the files it needs, no dev deps or source.
  output: "standalone",
  reactStrictMode: true,
  devIndicators: false,
  // Same-origin data plane: the browser only talks to this Next server; /api/* is
  // proxied to the OCR backend (afterFiles → our own app routes like /api/chain/*
  // win first). Deploy-agnostic: point OCR_API_ORIGIN at the backend (dev tunnel or
  // production host); nothing backend-specific is baked into the client bundle.
  async rewrites() {
    const origin = process.env.OCR_API_ORIGIN ?? "http://127.0.0.1:7070";
    return {
      afterFiles: [{ source: "/api/:path*", destination: `${origin}/api/:path*` }],
    };
  },
  async headers() {
    // CSP backs the React output-escaping with an enforced policy and constrains
    // img/connect/frame for an untrusted upstream API. Next needs 'unsafe-inline'
    // (and 'unsafe-eval' in dev) for its runtime; img-src https: allows remote token logos.
    const csp = [
      "default-src 'self'",
      "img-src 'self' https: data:",
      "connect-src 'self'",
      "font-src 'self' data:",
      "style-src 'self' 'unsafe-inline'",
      "script-src 'self' 'unsafe-inline' 'unsafe-eval'",
      "frame-ancestors 'none'",
      "object-src 'none'",
      "base-uri 'self'",
      "form-action 'self'",
    ].join("; ");
    return [
      {
        source: "/(.*)",
        headers: [
          { key: "Content-Security-Policy", value: csp },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
          { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
        ],
      },
    ];
  },
};

export default nextConfig;
