package httpapi

import (
	"log/slog"
	"net/http"

	inner "langopen.dev/control-plane/internal/httpapi"
)

func NewHandler(logger *slog.Logger) http.Handler {
	return inner.New(logger).Router()
}
