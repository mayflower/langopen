FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY services/control-plane ./services/control-plane
RUN cd services/control-plane && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/control-plane ./cmd/control-plane

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/control-plane /app/control-plane
USER 65532:65532
ENTRYPOINT ["/app/control-plane"]
