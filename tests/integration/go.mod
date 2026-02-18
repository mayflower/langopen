module langopen.dev/tests-integration

go 1.25.0

require langopen.dev/builder v0.0.0

require langopen.dev/control-plane v0.0.0

require (
	github.com/go-chi/chi/v5 v5.2.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.8.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	langopen.dev/pkg v0.0.0 // indirect
)

replace langopen.dev/builder => ../../services/builder

replace langopen.dev/control-plane => ../../services/control-plane

replace langopen.dev/pkg => ../../pkg
