# syntax=docker/dockerfile:1
#
# Build the Vue SPA with Node, serve the static bundle with unprivileged nginx.
# Node, node_modules and sources stay in the build stage.

ARG NODE_VERSION=22
ARG NGINX_VERSION=1.29

# ---- build -----------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY . .
# `npm run build` type-checks (vue-tsc) before bundling.
RUN npm run build

# ---- runtime ---------------------------------------------------------------
FROM nginxinc/nginx-unprivileged:${NGINX_VERSION}-alpine
# Templates are rendered by the image entrypoint with envsubst (API_UPSTREAM,
# NGINX_LOCAL_RESOLVERS); only variables listed in NGINX_ENVSUBST_FILTER are replaced.
ENV API_UPSTREAM=api:8080 \
    NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1 \
    NGINX_ENVSUBST_FILTER="^(API_UPSTREAM|NGINX_LOCAL_RESOLVERS)$"
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --chmod=0755 nginx/90-require-config.sh /docker-entrypoint.d/90-require-config.sh
COPY --from=build /src/dist/ /usr/share/nginx/html/
EXPOSE 8080
