FROM mcr.microsoft.com/playwright:v1.62.1-noble
WORKDIR /work/web
COPY web/package.json web/package-lock.json ./
COPY web/src/api/generated ./src/api/generated
RUN npm ci
COPY web/e2e ./e2e
COPY web/playwright.config.ts ./playwright.config.ts
ENV PLAYWRIGHT_BASE_URL=http://web:4173
CMD ["npx", "playwright", "test"]
