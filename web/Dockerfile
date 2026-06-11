# Multi-stage build for the Next.js dashboard. The final image carries only the
# standalone server bundle (no dev deps, no source) and talks to the backend over
# the compose network.

FROM node:20-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM node:20-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# next.config rewrites() are evaluated at build time, so the backend origin must be
# present now (not just at runtime) or it bakes in the default 127.0.0.1.
ARG OCR_API_ORIGIN=http://ocr:7070
ENV OCR_API_ORIGIN=$OCR_API_ORIGIN
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
RUN addgroup -g 1001 -S nodejs && adduser -S nextjs -u 1001
COPY --from=builder /app/public ./public
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static
USER nextjs
EXPOSE 3000
# bind to all interfaces inside the container; the host publish maps it to 127.0.0.1 only
ENV PORT=3000 HOSTNAME=0.0.0.0
CMD ["node", "server.js"]
