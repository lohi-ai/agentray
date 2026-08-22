FROM node:24-alpine AS deps
WORKDIR /src
RUN corepack enable pnpm
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

# Build under Node, not Bun: Bun's musl/baseline build segfaults during
# `next build` (TypeScript phase) on Cloud Build. Bun is install-only.
FROM node:24-alpine AS builder
WORKDIR /src
ARG NEXT_PUBLIC_AGENTRAY_API_URL=http://localhost:8088
ENV NEXT_PUBLIC_AGENTRAY_API_URL=$NEXT_PUBLIC_AGENTRAY_API_URL
# The absolute origin this instance is served from. metadataBase, canonical,
# robots.txt and sitemap.xml cannot be relative, and they are baked at build
# time like every other NEXT_PUBLIC_ value. Left empty it falls back to the API
# URL, which is correct wherever web and the API share a hostname — they do on
# both deploy targets. See web/lib/brand.ts `siteOrigin()`.
ARG NEXT_PUBLIC_AGENTRAY_SITE_URL=
ENV NEXT_PUBLIC_AGENTRAY_SITE_URL=$NEXT_PUBLIC_AGENTRAY_SITE_URL
COPY --from=deps /src/node_modules ./node_modules
COPY . .
RUN ./node_modules/.bin/next build

FROM node:24-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=3200
ENV HOSTNAME=0.0.0.0
COPY --from=builder /src/.next/standalone ./
COPY --from=builder /src/.next/static ./.next/static
COPY --from=builder /src/public ./public
EXPOSE 3200
CMD ["node", "server.js"]
