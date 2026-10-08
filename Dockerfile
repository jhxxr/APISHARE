# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/apishare .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app \
    && mkdir -p /data \
    && chown app:app /data
COPY --from=build /out/apishare /apishare
ENV PORT=8080 \
    API_DB=/data/apishare.db
VOLUME /data
EXPOSE 8080
USER app
ENTRYPOINT ["/apishare"]
