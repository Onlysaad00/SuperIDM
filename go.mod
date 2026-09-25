module github.com/Onlysaad00/SuperIDM

go 1.24

// SuperIDM intentionally depends on the Go standard library only.
// This keeps the binary a fully self-contained single .exe with no runtime
// dependencies and makes fully offline, reproducible cross-compilation possible:
//   GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/superidm
