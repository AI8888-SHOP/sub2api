# syntax=docker/dockerfile:1
FROM mcr.microsoft.com/playwright:v1.55.0-noble
WORKDIR /app
COPY tools/prism-login-worker/package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY tools/prism-login-worker/server.mjs ./
ENV NODE_ENV=production PORT=8090 HEADLESS=true
EXPOSE 8090
CMD ["node", "server.mjs"]
