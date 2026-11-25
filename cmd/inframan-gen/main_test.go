package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/inframan-tech/inframan-go/internal/gen"
)

// The committed zz_capabilities.go is what the committed spec generates. The
// spec changing without the SDK being regenerated, or the generator changing
// without its output, would ship methods for capabilities the instance no
// longer serves as they say.
func TestTheCommittedFileIsWhatTheSpecGenerates(t *testing.T) {
	out := filepath.Join(t.TempDir(), "zz_capabilities.go")
	// The same arguments go:generate passes, from the package's directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if err := run("spec/capabilities.json", out, "inframan"); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("zz_capabilities.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("zz_capabilities.go is not what spec/capabilities.json generates; regenerate it: cd sdk/go && GOWORK=off go generate ./...")
	}
}

func TestTheSpecCanBeReadFromAnInstance(t *testing.T) {
	spec, err := os.ReadFile("../../spec/capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/capabilities.json" {
			http.NotFound(w, r)
			return
		}
		w.Write(spec)
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "zz.go")
	if err := run(srv.URL+"/api/v1/capabilities.json", out, "inframan"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("from "+srv.URL+"/api/v1/capabilities.json; DO NOT EDIT.")) {
		t.Error("a file generated from an instance does not say which")
	}
	if err := run(srv.URL+"/nope", out, "inframan"); err == nil {
		t.Error("a 404 generated a file")
	}
}

// The generator reads every field of the spec: the capabilities as it decodes
// them, written back in the spec's canonical form (compact JSON, "<", ">" and
// "&" as themselves), hash to the spec's own hash. A field the backend adds
// that the generator does not know fails it here, rather than being dropped.
func TestTheGeneratorReadsEveryFieldOfTheSpec(t *testing.T) {
	raw, err := os.ReadFile("../../spec/capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec gen.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(spec.Capabilities); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != spec.Hash {
		t.Errorf("the capabilities as the generator reads them hash to %s; the spec says %s. "+
			"The spec has a field gen.Capability or gen.Param does not read, or reads in another order.", got, spec.Hash)
	}
}
