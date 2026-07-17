# Base images are pinned by digest (BG-31): a mutable tag can be re-pointed on
# the registry side, silently changing the bytes a production build ships. To
# update, look up the new manifest-list digest for the tag and edit the pin
# deliberately (e.g. `docker buildx imagetools inspect node:20-alpine`).

# ---- Dev target (used by Docker Compose) ----
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS dev

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . .

EXPOSE 3000

CMD ["npm", "run", "dev"]

# ---- Production build ----
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS build

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

# ---- Production runtime ----
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS production

WORKDIR /app

ENV NODE_ENV=production

COPY --from=build /app/public ./public
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static

# Non-root runtime (BG-31): the stock `node` user (uid/gid 1000:1000) that ships
# with the official image. App code stays root-owned and read-only to the
# process; only the runtime-writable paths are owned by the user — /app/data
# (the dashboard_data volume mount point; pre-created so a FRESH named volume
# inherits this ownership) and .next/cache (Next.js runtime cache). NOTE for
# existing deployments: a dashboard_data volume created by an older root
# image keeps its root ownership — see the deploy notes in guides/head-setup.md.
RUN mkdir -p /app/data /app/.next/cache && chown -R node:node /app/data /app/.next/cache

USER node

EXPOSE 3000

CMD ["node", "server.js"]
