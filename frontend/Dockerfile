# The web image: the built Vue app, served by nginx, which also forwards
# /api to the backend. One origin for the page, the REST API and the live
# stream means no CORS setup and no second public port.
#
# Base images default to AWS ECR Public's mirror of the official Docker Hub
# images (same digests, no Docker Hub login), like the backend's.
ARG NODE_IMAGE=public.ecr.aws/docker/library/node:22-alpine
ARG NGINX_IMAGE=public.ecr.aws/docker/library/nginx:1.27-alpine

FROM ${NODE_IMAGE} AS build
WORKDIR /app

# pnpm at the version package.json pins, via corepack.
RUN corepack enable

# Dependencies first so a source change does not reinstall them.
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY . .

# Vite bakes these in at build time. The API is same-origin through nginx, so
# the relative default is right for any host the image is deployed on.
ARG VITE_WALLETCONNECT_ID=
ENV VITE_API_BASE=/api/v1 \
    VITE_WALLETCONNECT_ID=${VITE_WALLETCONNECT_ID}
RUN pnpm build

FROM ${NGINX_IMAGE}
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
