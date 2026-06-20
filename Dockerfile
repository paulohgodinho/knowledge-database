FROM node:20-alpine AS assets
WORKDIR /app

COPY package.json ./
COPY tailwind.config.js ./
RUN npm install

COPY web ./web
RUN npm run build:css

FROM golang:1.23-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY web ./web
COPY --from=assets /app/web/static/output.css /app/web/static/output.css

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /app/server ./cmd/server

FROM alpine:3.20
WORKDIR /app

COPY --from=builder /app/server /app/server
COPY --from=builder /app/web /app/web

EXPOSE 8080
CMD ["/app/server"]
