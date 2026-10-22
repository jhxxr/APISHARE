# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/apishare/ ./cmd/apishare/
COPY internal/ ./internal/
COPY LICENSE internal/bunhttp/NOTICE ./licenses/
COPY cmd/apishare/third_party/cliproxy/LICENSE ./licenses/CLIProxyAPI/LICENSE
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/apishare ./cmd/apishare

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app \
    && mkdir -p /data \
    && chown app:app /data
COPY --from=build /out/apishare /apishare
COPY --from=build /src/licenses/ /usr/share/licenses/apishare/
ENV PORT=8080 \
    API_DB=/data/apishare.db
VOLUME /data
EXPOSE 8080
USER app
ENTRYPOINT ["/apishare"]
