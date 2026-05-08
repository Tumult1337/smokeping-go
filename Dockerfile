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
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gosmokeping ./cmd/gosmokeping

FROM alpine:3.20
RUN apk add --no-cache libcap ca-certificates tzdata \
    && addgroup -S gosmokeping \
    && adduser -S -G gosmokeping gosmokeping
COPY --from=build /out/gosmokeping /opt/smokeping/gosmokeping
RUN setcap cap_net_raw+ep /opt/smokeping/gosmokeping
USER gosmokeping
EXPOSE 8080
ENTRYPOINT ["/opt/smokeping/gosmokeping"]
CMD ["-config", "/opt/smokeping/config.json"]