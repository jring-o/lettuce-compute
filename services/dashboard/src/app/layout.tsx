import type { Metadata } from "next";
import localFont from "next/font/local";
import { Navbar } from "@/components/layout/navbar";
import "./globals.css";

// The fonts are vendored in ./fonts and loaded with next/font/local — never
// next/font/google, which downloads from fonts.googleapis.com during
// `next build` and breaks image builds on hosts without Internet egress
// (PB-24). CI blocks the Google Fonts hosts before building to enforce this.
// Font options mirror the official `geist` npm package (dist/sans.js,
// dist/mono.js) so rendering matches the previous next/font/google setup.
const geistSans = localFont({
  src: "./fonts/Geist-Variable.woff2",
  variable: "--font-geist-sans",
  weight: "100 900",
});

const geistMono = localFont({
  src: "./fonts/GeistMono-Variable.woff2",
  variable: "--font-geist-mono",
  adjustFontFallback: false,
  fallback: [
    "ui-monospace",
    "SFMono-Regular",
    "Roboto Mono",
    "Menlo",
    "Monaco",
    "Liberation Mono",
    "DejaVu Sans Mono",
    "Courier New",
    "monospace",
  ],
  weight: "100 900",
});

export const metadata: Metadata = {
  title: "Lettuce",
  description: "Distributed Volunteer Compute Platform",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body
        className={`${geistSans.variable} ${geistMono.variable} antialiased`}
      >
        <Navbar />
        <main>{children}</main>
      </body>
    </html>
  );
}
