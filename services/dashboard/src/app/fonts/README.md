# Vendored fonts

Geist and Geist Mono (variable-weight woff2), vendored so the dashboard builds
without network access to Google Fonts (PB-24): `next/font/google` downloads
font files from fonts.googleapis.com during `next build`, which makes the
Docker image build fail on any host without Internet egress. These files are
loaded through `next/font/local` in `src/app/layout.tsx` instead.

Provenance: copied unmodified from the official `geist` npm package, version
1.7.2 (`dist/fonts/geist-sans/Geist-Variable.woff2` and
`dist/fonts/geist-mono/GeistMono-Variable.woff2`), published by Vercel. The
fonts are licensed under the SIL Open Font License 1.1 — see `LICENSE.txt` in
this directory, which must stay next to the font files. To update, copy the
same files from a newer `geist` package release.
