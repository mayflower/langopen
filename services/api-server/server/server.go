package server

import (
	"log/slog"
	"net/http"

	innerserver "langopen.dev/api-server/internal/server"
)

func NewHandler(logger *slog.Logger) (http.Handler, error) {
	s, err := innerserver.New(logger)
	if err != nil {
		return nil, err
	}
	return s.Router(), nil
}
