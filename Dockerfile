FROM node:24-bookworm-slim
WORKDIR /app
COPY package.json pnpm-lock.yaml ./
RUN corepack enable && pnpm install --prod --frozen-lockfile
COPY src ./src
COPY public ./public
ENV DBSPHERE_HOST=0.0.0.0 DBSPHERE_PORT=8080 DBSPHERE_DATA_DIR=/var/lib/dbsphere
RUN mkdir -p /var/lib/dbsphere && chown node:node /var/lib/dbsphere
VOLUME /var/lib/dbsphere
EXPOSE 8080
USER node
CMD ["node", "src/server.js"]
