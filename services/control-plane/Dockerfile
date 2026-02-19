FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY services/control-plane ./services/control-plane
RUN cd services/control-plane && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/control-plane /app/control-plane
ENTRYPOINT ["/app/control-plane"]
