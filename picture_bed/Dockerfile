# syntax=docker/dockerfile:1.7

ARG NODE_VERSION=22.23.0
FROM node:${NODE_VERSION}-alpine AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.29-alpine
LABEL org.opencontainers.image.title="BX YunPan Web" \
      org.opencontainers.image.description="React web application and same-origin API/object-storage proxy for BX YunPan"
COPY nginx-app.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/build /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=6 \
  CMD wget -q -O - http://127.0.0.1/healthz || exit 1
