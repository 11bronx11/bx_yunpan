ARG DOCKER_REGISTRY=docker.io
ARG NODE_VERSION=22.23.0
FROM ${DOCKER_REGISTRY}/library/node:${NODE_VERSION}-alpine AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM ${DOCKER_REGISTRY}/library/nginx:1.29-alpine
LABEL org.opencontainers.image.title="BX YunPan Web" \
      org.opencontainers.image.description="React web application and same-origin API/object-storage proxy for BX YunPan"
COPY nginx-app.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/build /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=10s --timeout=8s --start-period=5s --retries=6 \
  CMD wget -q -O /dev/null http://127.0.0.1/healthz \
    && wget -q -O /dev/null http://127.0.0.1/readyz \
    && wget -q -O /dev/null http://127.0.0.1/storage-healthz \
    || exit 1
