FROM node:22-alpine AS web
RUN npm install --global pnpm@11.19.0
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /quota-watch .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 quota-watch \
    && adduser -D -u 10001 -G quota-watch quota-watch \
    && mkdir /data && chown quota-watch:quota-watch /data
COPY --from=build /quota-watch /usr/local/bin/quota-watch
USER quota-watch
ENV QUOTA_WATCH_LISTEN=0.0.0.0:8091 QUOTA_WATCH_DATA_DIR=/data
EXPOSE 8091
ENTRYPOINT ["/usr/local/bin/quota-watch"]
