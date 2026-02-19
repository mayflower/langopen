FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY services/operator ./services/operator
RUN cd services/operator && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/operator ./cmd/operator

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/operator /app/operator
ENTRYPOINT ["/app/operator"]
