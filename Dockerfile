# syntax=docker/dockerfile:1

# Build the application from source
FROM golang:1.19 AS build-stage

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /prometheus-tapo-exporter

# Deploy the application binary into a lean image
FROM alpine:latest AS build-release-stage

WORKDIR /

COPY --from=build-stage /prometheus-tapo-exporter /prometheus-tapo-exporter

EXPOSE 8080

ENTRYPOINT ["/prometheus-tapo-exporter"]