# syntax=docker/dockerfile:1.7

FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gosmokeping ./cmd/gosmokeping

FROM alpine:3.20
RUN apk add --no-cache libcap ca-certificates tzdata \
    && addgroup -S gosmokeping \
    && adduser -S -G gosmokeping gosmokeping
COPY --from=build /out/gosmokeping /usr/local/bin/gosmokeping
RUN setcap cap_net_raw+ep /usr/local/bin/gosmokeping
USER gosmokeping
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gosmokeping"]
CMD ["-config", "/etc/gosmokeping/config.json"]