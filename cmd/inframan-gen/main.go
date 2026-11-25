// Command inframan-gen writes the SDK's typed capabilities (zz_capabilities.go)
// from an instance's capability spec: the committed copy, or any instance's
// own, read from its GET /api/v1/capabilities.json.
//
//	go run ./cmd/inframan-gen                       # spec/capabilities.json → zz_capabilities.go
//	go run ./cmd/inframan-gen -spec https://<console domain>/api/v1/capabilities.json
//
// In the Inframan monorepo the SDK is outside go.work: run it with
// GOWORK=off, or through `GOWORK=off go generate ./...`.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inframan-tech/inframan-go/internal/gen"
)

func main() {
	spec := flag.String("spec", "spec/capabilities.json", "the capability spec: a file, or an http(s) URL")
	out := flag.String("out", "zz_capabilities.go", "the file to write")
	pkg := flag.String("package", "inframan", "the generated file's package")
	flag.Parse()
	if err := run(*spec, *out, *pkg); err != nil {
		fmt.Fprintln(os.Stderr, "inframan-gen:", err)
		os.Exit(1)
	}
}

func run(spec, out, pkg string) error {
	raw, source, err := read(spec)
	if err != nil {
		return err
	}
	src, err := gen.Generate(raw, gen.Options{Package: pkg, Source: source})
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

// read is the spec's bytes and how the file's header names it: a URL as
// given, a file by its slash-separated path as given.
func read(spec string) ([]byte, string, error) {
	if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		h := &http.Client{Timeout: 60 * time.Second}
		resp, err := h.Get(spec)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("GET %s answered %d", spec, resp.StatusCode)
		}
		return raw, spec, nil
	}
	raw, err := os.ReadFile(spec)
	if err != nil {
		return nil, "", err
	}
	return raw, filepath.ToSlash(spec), nil
}
