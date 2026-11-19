# Single image builds both Go binaries; docker-compose selects the entrypoint
# per service via `command:`.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/demo-service ./cmd/demo-service \
 && CGO_ENABLED=0 go build -o /out/webhook-receiver ./cmd/webhook-receiver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app
COPY --from=build /out/demo-service /app/demo-service
COPY --from=build /out/webhook-receiver /app/webhook-receiver
# CMD (not ENTRYPOINT) so docker-compose `command:` can select the binary.
CMD ["/app/demo-service"]
