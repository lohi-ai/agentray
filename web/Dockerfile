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
