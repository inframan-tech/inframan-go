// Package gen writes the SDK's typed methods from an instance's capability
// spec (capabilities.json, docs/plans/rak-platform.md C2): an init that
// registers every capability's route, a method and a params struct for each,
// and the Capabilities slice. cmd/inframan-gen runs it; the same spec always
// gives the same file.
package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"strconv"
	"strings"
	"unicode"
)

// SupportedSpecVersion is the spec_version this generator reads.
const SupportedSpecVersion = 1

// Spec is the document, as far as the generator reads it.
type Spec struct {
	SpecVersion  int          `json:"spec_version"`
	Hash         string       `json:"hash"`
	BasePath     string       `json:"base_path"`
	Capabilities []Capability `json:"capabilities"`
}

// Capability is one capability of the spec.
type Capability struct {
	Name         string   `json:"name"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description"`
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	Permission   string   `json:"permission"`
	Mutating     bool     `json:"mutating"`
	ReadOverPOST bool     `json:"read_over_post"`
	Surfaces     []string `json:"surfaces"`
	Params       []Param  `json:"params"`
}

// Param is one parameter of a capability.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	In          string   `json:"in"`
	Enum        []string `json:"enum"`
}

// Options are what the file is generated as.
type Options struct {
	// Package is the file's package, "inframan" unless set.
	Package string
	// Source names the spec in the file's header.
	Source string
}

// reservedMethods are the Client's own methods, which no capability's may be.
var reservedMethods = map[string]bool{"Call": true, "Pages": true}

// Generate is the Go source of the spec's capabilities, gofmt'd.
func Generate(raw []byte, opts Options) ([]byte, error) {
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("read the spec: %w", err)
	}
	if spec.SpecVersion != SupportedSpecVersion {
		return nil, fmt.Errorf("the spec is spec_version %d; this generator reads %d", spec.SpecVersion, SupportedSpecVersion)
	}
	if opts.Package == "" {
		opts.Package = "inframan"
	}
	g := &generator{spec: spec, opts: opts, taken: map[string]string{}}
	if err := g.plan(); err != nil {
		return nil, err
	}
	src := g.write()
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("format the generated file: %w\n%s", err, src)
	}
	return out, nil
}

type generator struct {
	spec  Spec
	opts  Options
	caps  []plannedCap
	taken map[string]string // a top-level or method name → what took it
}

type plannedCap struct {
	Capability
	method string // Go method name
	params string // params struct name
	fields []plannedField
	enums  []plannedEnum
	paged  string // "cursor" or "offset": it is paged, and has a <method>Pages helper
}

type plannedField struct {
	Param
	field string // Go field name
	typ   string // Go type of the value (enum type, string, int, bool)
	enum  *plannedEnum
}

type plannedEnum struct {
	typ    string
	consts []plannedConst
}

type plannedConst struct {
	name, value string
}

func (g *generator) take(name, what string) error {
	if prev, ok := g.taken[name]; ok {
		return fmt.Errorf("%s and %s both generate %s", prev, what, name)
	}
	g.taken[name] = what
	return nil
}

func (g *generator) plan() error {
	seen := map[string]bool{}
	for _, name := range []string{"SpecHash", "SpecVersion", "Capabilities"} {
		if err := g.take(name, "the file's own "+name); err != nil {
			return err
		}
	}
	for _, c := range g.spec.Capabilities {
		if c.Name == "" {
			return fmt.Errorf("a capability of the spec has no name")
		}
		if seen[c.Name] {
			return fmt.Errorf("the spec lists %s twice", c.Name)
		}
		seen[c.Name] = true
		if c.Method == "" || c.Path == "" {
			return fmt.Errorf("%s has no method or path", c.Name)
		}
		pc := plannedCap{Capability: c, method: Camel(c.Name)}
		if pc.method == "" || !unicode.IsLetter(rune(pc.method[0])) {
			return fmt.Errorf("%s gives no Go name", c.Name)
		}
		if reservedMethods[pc.method] {
			return fmt.Errorf("%s would be the Client's own %s", c.Name, pc.method)
		}
		pc.params = pc.method + "Params"
		if err := g.take(pc.method, c.Name+"'s method"); err != nil {
			return err
		}
		if err := g.take(pc.params, c.Name+"'s params"); err != nil {
			return err
		}
		fieldNames := map[string]string{}
		for _, p := range c.Params {
			f := plannedField{Param: p, field: Camel(p.Name)}
			if f.field == "" || !unicode.IsLetter(rune(f.field[0])) {
				return fmt.Errorf("%s's parameter %q gives no Go name", c.Name, p.Name)
			}
			if prev, ok := fieldNames[f.field]; ok {
				return fmt.Errorf("%s's parameters %q and %q both generate %s", c.Name, prev, p.Name, f.field)
			}
			fieldNames[f.field] = p.Name
			switch p.In {
			case "path", "query", "body":
			default:
				return fmt.Errorf("%s's parameter %q is in %q, not path, query or body", c.Name, p.Name, p.In)
			}
			switch p.Type {
			case "string", "":
				f.typ = "string"
			case "integer":
				f.typ = "int"
			case "boolean":
				f.typ = "bool"
			default:
				return fmt.Errorf("%s's parameter %q is of type %q, which the generator does not know", c.Name, p.Name, p.Type)
			}
			if len(p.Enum) > 0 {
				if f.typ != "string" {
					return fmt.Errorf("%s's parameter %q has an enum but is a %s", c.Name, p.Name, p.Type)
				}
				e := plannedEnum{typ: pc.method + f.field}
				if err := g.take(e.typ, c.Name+"'s "+p.Name); err != nil {
					return err
				}
				names := map[string]bool{}
				for i, v := range p.Enum {
					suffix := Camel(v)
					if suffix == "" || !unicode.IsLetter(rune(suffix[0])) {
						suffix = "V" + suffix
					}
					if suffix == "V" {
						suffix = "Value" + strconv.Itoa(i)
					}
					name := e.typ + suffix
					for n := 2; names[name]; n++ {
						name = e.typ + suffix + strconv.Itoa(n)
					}
					names[name] = true
					if err := g.take(name, c.Name+"'s "+p.Name+" "+strconv.Quote(v)); err != nil {
						return err
					}
					e.consts = append(e.consts, plannedConst{name: name, value: v})
				}
				f.typ = e.typ
				pc.enums = append(pc.enums, e)
				f.enum = &pc.enums[len(pc.enums)-1]
			}
			switch p.Name {
			case "cursor":
				pc.paged = "cursor"
			case "offset":
				if pc.paged == "" {
					pc.paged = "offset"
				}
			}
			pc.fields = append(pc.fields, f)
		}
		if pc.paged != "" {
			if err := g.take(pc.method+"Pages", c.Name+"'s pages"); err != nil {
				return err
			}
		}
		g.caps = append(g.caps, pc)
	}
	return nil
}

func (g *generator) write() []byte {
	var b bytes.Buffer
	src := g.opts.Source
	if src == "" {
		src = "a capability spec"
	}
	fmt.Fprintf(&b, "// Code generated by inframan-gen from %s; DO NOT EDIT.\n\n", src)
	fmt.Fprintf(&b, "package %s\n\n", g.opts.Package)
	b.WriteString("import (\n\t\"context\"\n\t\"encoding/json\"\n)\n\n")

	b.WriteString("// SpecHash is the hash of the capability spec this file was generated from:\n")
	b.WriteString("// the ETag (in quotes) an instance's GET /v1/capabilities.json answers while\n")
	b.WriteString("// it serves the same capabilities.\n")
	fmt.Fprintf(&b, "const SpecHash = %s\n\n", strconv.Quote(g.spec.Hash))
	b.WriteString("// SpecVersion is the spec_version of the spec this file was generated from.\n")
	fmt.Fprintf(&b, "const SpecVersion = %d\n\n", g.spec.SpecVersion)

	b.WriteString("func init() {\n\tfor _, c := range Capabilities {\n\t\tRegisterRoute(c.Name, c.Route())\n\t}\n}\n\n")

	fmt.Fprintf(&b, "// Capabilities lists the %d capabilities this client was generated with,\n", len(g.caps))
	b.WriteString("// sorted by name.\n")
	b.WriteString("var Capabilities = []CapabilityInfo{\n")
	for _, c := range g.caps {
		fmt.Fprintf(&b, "\t{\n\t\tName: %s,\n\t\tSummary: %s,\n\t\tMethod: %s,\n\t\tPath: %s,\n\t\tPermission: %s,\n",
			strconv.Quote(c.Name), strconv.Quote(c.Summary), strconv.Quote(c.Method), strconv.Quote(c.Path), strconv.Quote(c.Permission))
		if c.Mutating {
			b.WriteString("\t\tMutating: true,\n")
		}
		if c.ReadOverPOST {
			b.WriteString("\t\tReadOverPOST: true,\n")
		}
		fmt.Fprintf(&b, "\t\tSurfaces: %s,\n", stringSlice(c.Surfaces))
		if len(c.fields) > 0 {
			b.WriteString("\t\tParams: []ParamInfo{\n")
			for _, f := range c.fields {
				typ := f.Type
				if typ == "" {
					typ = "string"
				}
				fmt.Fprintf(&b, "\t\t\t{Name: %s, Type: %s, In: %s", strconv.Quote(f.Name), strconv.Quote(typ), strconv.Quote(f.In))
				if f.Required {
					b.WriteString(", Required: true")
				}
				if len(f.Enum) > 0 {
					fmt.Fprintf(&b, ", Enum: %s", stringSlice(f.Enum))
				}
				b.WriteString("},\n")
			}
			b.WriteString("\t\t},\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")

	for _, c := range g.caps {
		g.writeCap(&b, c)
	}
	return b.Bytes()
}

func stringSlice(s []string) string {
	if len(s) == 0 {
		return "nil"
	}
	q := make([]string, len(s))
	for i, v := range s {
		q[i] = strconv.Quote(v)
	}
	return "[]string{" + strings.Join(q, ", ") + "}"
}

func (g *generator) writeCap(b *bytes.Buffer, c plannedCap) {
	for _, e := range c.enums {
		writeDoc(b, "", []string{fmt.Sprintf("%s is a value %s's %s takes.", e.typ, c.Name, fieldOfEnum(c, e).Name)})
		fmt.Fprintf(b, "type %s string\n\n", e.typ)
		fmt.Fprintf(b, "// The values %s takes.\nconst (\n", e.typ)
		for _, k := range e.consts {
			fmt.Fprintf(b, "\t%s %s = %s\n", k.name, e.typ, strconv.Quote(k.value))
		}
		b.WriteString(")\n\n")
	}

	// The params struct.
	writeDoc(b, "", []string{fmt.Sprintf("%s are %s's parameters.", c.params, c.Name)})
	fmt.Fprintf(b, "type %s struct {\n", c.params)
	for i, f := range c.fields {
		if i > 0 {
			b.WriteString("\n")
		}
		req := "optional"
		if f.Required {
			req = "required"
		}
		head := fmt.Sprintf("%s is the %s parameter, %s, in the %s.", f.field, f.Name, req, inWords(f.In))
		writeDoc(b, "\t", []string{head, f.Description})
		typ := f.typ
		tag := f.Name
		if !f.Required {
			typ = "*" + typ
			tag += ",omitempty"
		}
		fmt.Fprintf(b, "\t%s %s `json:%s`\n", f.field, typ, strconv.Quote(tag))
	}
	b.WriteString("}\n\n")

	// args.
	fmt.Fprintf(b, "func (p %s) args() map[string]any {\n", c.params)
	if len(c.fields) == 0 {
		b.WriteString("\treturn nil\n}\n\n")
	} else {
		b.WriteString("\ta := map[string]any{}\n")
		for _, f := range c.fields {
			val := "p." + f.field
			if !f.Required {
				val = "*p." + f.field
			}
			if f.enum != nil {
				val = "string(" + val + ")"
			}
			if f.Required {
				fmt.Fprintf(b, "\ta[%s] = %s\n", strconv.Quote(f.Name), val)
			} else {
				fmt.Fprintf(b, "\tif p.%s != nil {\n\t\ta[%s] = %s\n\t}\n", f.field, strconv.Quote(f.Name), val)
			}
		}
		b.WriteString("\treturn a\n}\n\n")
	}

	// The method.
	lines := []string{fmt.Sprintf("%s calls %s: %s %s.", c.method, c.Name, c.Method, c.Path)}
	if s := strings.TrimSpace(c.Summary); s != "" {
		lines = append(lines, s)
	}
	if d := strings.TrimSpace(c.Description); d != "" {
		lines = append(lines, d)
	}
	lines = append(lines, about(c.Capability))
	writeDoc(b, "", lines)
	fmt.Fprintf(b, "func (c *Client) %s(ctx context.Context, p %s) (json.RawMessage, error) {\n", c.method, c.params)
	fmt.Fprintf(b, "\treturn c.invoke(ctx, %s, p.args())\n}\n\n", strconv.Quote(c.Name))

	if c.paged != "" {
		doc := fmt.Sprintf("%sPages calls %s page after page, from p's cursor, each answer's next_cursor sent as the next call's cursor, handing every answer to page until one carries no next_cursor or page returns an error (StopPaging stops with none).", c.method, c.Name)
		if c.paged == "offset" {
			doc = fmt.Sprintf("%sPages calls %s page after page, from p's offset, each answer's next_offset sent as the next call's offset, handing every answer to page until next_offset is null or negative or page returns an error (StopPaging stops with none). Client.Pages says where next_offset is read.", c.method, c.Name)
		}
		writeDoc(b, "", []string{doc})
		fmt.Fprintf(b, "func (c *Client) %sPages(ctx context.Context, p %s, page func(json.RawMessage) error) error {\n", c.method, c.params)
		fmt.Fprintf(b, "\treturn c.Pages(ctx, %s, p.args(), page)\n}\n\n", strconv.Quote(c.Name))
	}
}

func fieldOfEnum(c plannedCap, e plannedEnum) plannedField {
	for _, f := range c.fields {
		if f.enum != nil && f.enum.typ == e.typ {
			return f
		}
	}
	return plannedField{}
}

func inWords(in string) string {
	switch in {
	case "query":
		return "query string"
	}
	return in
}

// about is the last paragraph of a method's comment: who may call it and
// what it does to state.
func about(c Capability) string {
	var parts []string
	if c.Permission != "" {
		parts = append(parts, "It needs the "+c.Permission+" permission")
	} else {
		parts = append(parts, "Any signed-in caller may call it")
	}
	switch {
	case c.Mutating:
		parts = append(parts, "changes state")
	case c.ReadOverPOST:
		parts = append(parts, "changes nothing (a read sent as a POST)")
	default:
		parts = append(parts, "changes nothing")
	}
	surfaces := "surfaces: " + strings.Join(c.Surfaces, ", ")
	if contains(c.Surfaces, "agent") {
		parts = append(parts, "and is offered to agents and service accounts ("+surfaces+")")
	} else {
		parts = append(parts, "and is not offered to agents or service accounts ("+surfaces+")")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " " + parts[len(parts)-1] + "."
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// writeDoc writes paragraphs as a doc comment, each wrapped at 77 columns
// and a blank comment line between them. A paragraph's own line breaks are
// kept; its lines' leading spaces are not, so none reads as code.
func writeDoc(b *bytes.Buffer, indent string, paragraphs []string) {
	first := true
	for _, para := range paragraphs {
		for _, chunk := range strings.Split(strings.ReplaceAll(para, "\r\n", "\n"), "\n\n") {
			chunk = strings.TrimSpace(chunk)
			if chunk == "" {
				continue
			}
			if !first {
				b.WriteString(indent + "//\n")
			}
			first = false
			for _, line := range strings.Split(chunk, "\n") {
				for _, w := range wrap(strings.TrimSpace(line), 77-len(indent)) {
					b.WriteString(indent + "// " + w + "\n")
				}
			}
		}
	}
}

// wrap breaks s into lines of at most width characters at spaces; a word
// longer than width is a line of its own.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len([]rune(line))+1+len([]rune(w)) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}

// initialisms are the words written in capitals in a Go name, and the few
// with a case of their own.
var initialisms = map[string]string{
	"acl": "ACL", "ai": "AI", "api": "API", "aws": "AWS", "ca": "CA", "cicd": "CICD", "cidr": "CIDR",
	"cli": "CLI", "cpu": "CPU", "css": "CSS", "db": "DB", "dns": "DNS", "eks": "EKS",
	"gcp": "GCP", "gke": "GKE", "html": "HTML", "http": "HTTP", "https": "HTTPS",
	"iam": "IAM", "iap": "IAP", "id": "ID", "ip": "IP", "json": "JSON", "kms": "KMS",
	"mcp": "MCP", "pr": "PR", "rpc": "RPC", "sa": "SA", "sdk": "SDK", "smtp": "SMTP",
	"sql": "SQL", "ssh": "SSH", "tcp": "TCP", "tls": "TLS", "ttl": "TTL", "ui": "UI",
	"uid": "UID", "uri": "URI", "url": "URL", "utc": "UTC", "uuid": "UUID", "vm": "VM",
	"wif": "WIF", "xml": "XML", "yaml": "YAML",
	"github": "GitHub", "oauth": "OAuth", "openai": "OpenAI",
}

// Camel is the Go name of a snake_case name: each word capitalised, an
// initialism in capitals ("service_id" is ServiceID, "pipeline_ids"
// PipelineIDs), anything but a letter or a digit a break between words.
func Camel(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var b strings.Builder
	for _, w := range words {
		lw := strings.ToLower(w)
		if v, ok := initialisms[lw]; ok {
			b.WriteString(v)
			continue
		}
		if strings.HasSuffix(lw, "s") {
			if v, ok := initialisms[strings.TrimSuffix(lw, "s")]; ok && strings.ToUpper(v) == v {
				b.WriteString(v + "s")
				continue
			}
		}
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}
