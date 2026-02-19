FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY services/operator ./services/operator
RUN cd services/operator && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/operator ./cmd/operator

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/operator /app/operator
USER 65532:65532
ENTRYPOINT ["/app/operator"]
