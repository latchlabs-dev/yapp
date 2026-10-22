# Build the Tailscale-only web portal (Vite/React) and serve it via nginx.
# The build stage runs on the native build arch (output is just static JS);
# only the nginx runtime below is resolved per target arch.
FROM --platform=$BUILDPLATFORM node:22.23.4-alpine AS build
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@10.23.0 --activate

# Install deps from the frozen lockfile (no network beyond pinned versions).
COPY package.json pnpm-lock.yaml .npmrc ./
RUN pnpm install --frozen-lockfile

COPY . .
RUN pnpm build

# Runtime: static files + reverse-proxy /web/v1 -> the shared data-plane listener.
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
