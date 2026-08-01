FROM node:24.18.1-bookworm-slim
WORKDIR /app
COPY web/package.json web/package-lock.json ./
COPY web/src/api/generated ./src/api/generated
RUN npm install --global npm@11.19.0
RUN npm ci
COPY web/ .
EXPOSE 4173
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
