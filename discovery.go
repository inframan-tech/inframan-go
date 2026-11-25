package inframan

// The surfaces a capability can be offered on, as CapabilityInfo.Surfaces
// says them.
const (
	// SurfaceConsole is the browser console.
	SurfaceConsole = "console"
	// SurfaceCLI is the inframan CLI.
	SurfaceCLI = "cli"
	// SurfaceAgent is an agent, over MCP or the agent gateway; a service
	// account may call only these.
	SurfaceAgent = "agent"
	// SurfaceInternal is machine plumbing.
	SurfaceInternal = "internal"
)

// CapabilityInfo describes a capability this client was generated with, as
// the instance's spec (capabilities.json) said it. The generated Capabilities
// lists every one.
type CapabilityInfo struct {
	Name       string
	Summary    string
	Method     string
	Path       string
	Permission string
	// Mutating says it changes state; ReadOverPOST, that it is a read whose
	// input is sent in a POST's body.
	Mutating     bool
	ReadOverPOST bool
	Surfaces     []string
	Params       []ParamInfo
}

// ParamInfo is one of a capability's parameters.
type ParamInfo struct {
	Name string
	// Type is "string", "integer" or "boolean".
	Type string
	// In is "path", "query" or "body".
	In       string
	Required bool
	// Enum is the closed set of values it takes, or nil for any.
	Enum []string
}

// HasSurface says whether the capability is offered on surface.
func (c CapabilityInfo) HasSurface(surface string) bool {
	for _, s := range c.Surfaces {
		if s == surface {
			return true
		}
	}
	return false
}

// Route is where the capability is served, as RegisterRoute takes it.
func (c CapabilityInfo) Route() Route {
	r := Route{Method: c.Method, Path: c.Path, In: map[string]string{}, ReadOnly: !c.Mutating}
	for _, p := range c.Params {
		r.In[p.Name] = p.In
		if p.Required {
			r.Required = append(r.Required, p.Name)
		}
	}
	return r
}

// CapabilityNamed is the capability of that name this client was generated
// with.
func CapabilityNamed(name string) (CapabilityInfo, bool) {
	for _, c := range Capabilities {
		if c.Name == name {
			return c, true
		}
	}
	return CapabilityInfo{}, false
}

// Ptr is a pointer to v, for a parameter that is optional:
// GetAgentRunParams{Limit: inframan.Ptr(50)}.
func Ptr[T any](v T) *T { return &v }
