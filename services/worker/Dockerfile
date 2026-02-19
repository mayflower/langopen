FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY services/worker ./services/worker
RUN cd services/worker && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/worker ./cmd/worker

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/worker /app/worker
USER 65532:65532
ENTRYPOINT ["/app/worker"]
