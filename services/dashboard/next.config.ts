import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "standalone",
  async redirects() {
    return [
      {
        source: "/projects/:path*",
        destination: "/leafs/:path*",
        permanent: true,
      },
      {
        source: "/projects",
        destination: "/leafs",
        permanent: true,
      },
      {
        source: "/dashboard/projects/:path*",
        destination: "/dashboard/leafs/:path*",
        permanent: true,
      },
      {
        source: "/dashboard/projects",
        destination: "/dashboard/leafs",
        permanent: true,
      },
    ];
  },
  async headers() {
    const sharedHeaders = [
      { key: "X-Content-Type-Options", value: "nosniff" },
      { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
      {
        key: "Strict-Transport-Security",
        value: "max-age=63072000; includeSubDomains; preload",
      },
      {
        key: "Permissions-Policy",
        value: "camera=(), microphone=(), geolocation=()",
      },
    ];
    return [
      {
        source: "/(.*)",
        headers: [
          ...sharedHeaders,
          { key: "X-Frame-Options", value: "DENY" },
        ],
      },
    ];
  },
};

export default nextConfig;
