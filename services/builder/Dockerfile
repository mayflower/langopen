FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY services/builder ./services/builder
RUN cd services/builder && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/builder ./cmd/builder

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/builder /app/builder
ENTRYPOINT ["/app/builder"]
