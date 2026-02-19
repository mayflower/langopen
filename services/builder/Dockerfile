FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY services/builder ./services/builder
RUN cd services/builder && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/builder ./cmd/builder

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/builder /app/builder
USER 65532:65532
ENTRYPOINT ["/app/builder"]
