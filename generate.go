package inframan

// zz_capabilities.go is generated from the capability spec. In the Inframan
// monorepo, regenerate the spec first (in inframan-backend,
// go run ./cmd/capability-spec -o ../sdk/go/spec/capabilities.json), then run
// GOWORK=off go generate ./... here.

//go:generate go run ./cmd/inframan-gen -spec spec/capabilities.json -out zz_capabilities.go
