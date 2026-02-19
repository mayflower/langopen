FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off
COPY pkg ./pkg
COPY contracts ./contracts
COPY db ./db
COPY services/api-server ./services/api-server
RUN cd services/api-server && go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/api-server ./cmd/api-server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api-server /app/api-server
COPY --from=build /src/contracts /app/contracts
COPY --from=build /src/db /app/db
ENTRYPOINT ["/app/api-server"]
