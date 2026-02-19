FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY contracts ./contracts
COPY db ./db
COPY services/api-server ./services/api-server
RUN cd services/api-server && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/api-server ./cmd/api-server

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/api-server /app/api-server
COPY --from=build /src/contracts /app/contracts
COPY --from=build /src/db /app/db
USER 65532:65532
ENTRYPOINT ["/app/api-server"]
