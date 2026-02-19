package api

import (
	"log/slog"
	"net/http"

	innerapi "langopen.dev/builder/internal/api"
)

func NewHandler(logger *slog.Logger) http.Handler {
	return innerapi.New(logger).Router()
}
