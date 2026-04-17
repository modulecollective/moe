// Package managed builds request payloads for Anthropic's Managed
// Agents API, the hosted alternative to running `claude` locally.
//
// The design is documented in designs/managed-agents.md. The short
// version: when an operator fires a `moe dispatch` turn, Anthropic
// provisions a per-session container, clones the project repo (and,
// read-only, the bureaucracy) into it via proxy-injected tokens, and
// runs the agent loop server-side. moe's job is to assemble the
// session-create body — agent/environment ids, prompt, and the
// resource list, including one github_repository per pinned submodule.
//
// Today this package only emits the JSON; it does not POST it. That
// lets `moe dispatch --dry-run` validate the payload shape against a
// real bureaucracy without real API credentials. Adding an HTTP client
// that actually calls /v1/sessions is a follow-up.
package managed

import (
	"encoding/json"
	"fmt"
)

// Session is the body of POST /v1/sessions. Fields mirror the
// documented schema (see managed-agents-api-reference.md in the
// anthropics/skills repo) and marshal directly to the wire format.
type Session struct {
	AgentID       string            `json:"agent_id"`
	EnvironmentID string            `json:"environment_id"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Resources     []Resource        `json:"resources"`
	Prompt        string            `json:"prompt,omitempty"`
}

// Resource is one entry in the session's resources array. Today moe
// only emits type="github_repository"; the struct is shaped to tolerate
// additional kinds (e.g. "file") without a breaking change.
type Resource struct {
	Type               string    `json:"type"`
	URL                string    `json:"url,omitempty"`
	AuthorizationToken string    `json:"authorization_token,omitempty"`
	MountPath          string    `json:"mount_path,omitempty"`
	Checkout           *Checkout `json:"checkout,omitempty"`
}

// Checkout pins the ref the sandbox should start on. Exactly one of
// Name (branch / tag) or SHA is meaningful per type.
type Checkout struct {
	Type string `json:"type"`           // "branch" | "tag" | "commit"
	Name string `json:"name,omitempty"` // set for branch/tag
	SHA  string `json:"sha,omitempty"`  // set for commit
}

// Params is the caller's view: the high-level knobs that turn into a
// Session. Keeping the indirection means `moe dispatch` doesn't need
// to know the wire field names or how submodules expand into resources.
type Params struct {
	// AgentID and EnvironmentID are the persistent template ids moe
	// reuses across sessions. `moe dispatch` resolves them from config.
	AgentID       string
	EnvironmentID string

	// ProjectRepo is the target repo URL the agent will edit.
	ProjectRepo string
	// ProjectBranch is the branch the agent works on; typically
	// "moe/<request-id>" so its pushes can't collide with main.
	ProjectBranch string
	// ProjectMountPath is where the project lands inside the sandbox.
	// Default is "/workspace/repo".
	ProjectMountPath string
	// ProjectToken authorizes reads and pushes on the project repo.
	// Treated opaquely — this package never inspects or logs it.
	ProjectToken string

	// BureaucracyRepo is the bureaucracy's git URL. Mounted read-only
	// (via a token without push scope) so the agent can browse other
	// documents without being able to rewrite the journal.
	BureaucracyRepo string
	// BureaucracyMountPath defaults to "/workspace/bureaucracy".
	BureaucracyMountPath string
	// BureaucracySHA pins the bureaucracy mount to a specific commit,
	// so sibling `moe` runs that land in the bureaucracy mid-session
	// don't change what the agent sees.
	BureaucracySHA string
	// BureaucracyToken is a read-only token for the bureaucracy.
	BureaucracyToken string

	// Submodules expands into one github_repository resource each,
	// mounted under ProjectMountPath at their submodule paths. Used
	// because the github_repository schema has no `recursive` flag
	// and submodule-init behavior inside the sandbox is undocumented
	// — mounting each one explicitly is the safe path.
	Submodules []Submodule

	// Prompt is the assembled system prompt passed to the agent.
	Prompt string

	// Metadata is free-form moe bookkeeping (request id, doc id, etc.)
	// so the session can be cross-referenced from the bureaucracy.
	Metadata map[string]string
}

// Submodule is one submodule of the project repo, pinned to a SHA.
type Submodule struct {
	// Path is the submodule's location relative to the project root
	// (what .gitmodules calls `path`). Joined onto ProjectMountPath
	// to produce the resource's mount_path.
	Path string
	// URL is the submodule's remote URL (what .gitmodules calls `url`).
	URL string
	// SHA is the commit the parent repo has pinned this submodule at,
	// read from `git ls-tree HEAD <path>`.
	SHA string
	// Token authorizes reads on this submodule. Usually the same PAT
	// as the parent project if they share an org; callers can pass a
	// different token per submodule when needed.
	Token string
}

// BuildSession turns Params into a Session with the primary project
// first, submodules next (so mount order is predictable in the output),
// and the bureaucracy last as a read-only reference.
func BuildSession(p Params) (*Session, error) {
	if p.ProjectRepo == "" {
		return nil, fmt.Errorf("managed: ProjectRepo is required")
	}
	if p.ProjectBranch == "" {
		return nil, fmt.Errorf("managed: ProjectBranch is required")
	}

	projectMount := p.ProjectMountPath
	if projectMount == "" {
		projectMount = "/workspace/repo"
	}
	bureauMount := p.BureaucracyMountPath
	if bureauMount == "" {
		bureauMount = "/workspace/bureaucracy"
	}

	resources := []Resource{{
		Type:               "github_repository",
		URL:                p.ProjectRepo,
		AuthorizationToken: p.ProjectToken,
		MountPath:          projectMount,
		Checkout: &Checkout{
			Type: "branch",
			Name: p.ProjectBranch,
		},
	}}
	for _, sm := range p.Submodules {
		if sm.Path == "" || sm.URL == "" || sm.SHA == "" {
			return nil, fmt.Errorf("managed: submodule entry missing path/url/sha: %+v", sm)
		}
		resources = append(resources, Resource{
			Type:               "github_repository",
			URL:                sm.URL,
			AuthorizationToken: sm.Token,
			MountPath:          joinMount(projectMount, sm.Path),
			Checkout: &Checkout{
				Type: "commit",
				SHA:  sm.SHA,
			},
		})
	}
	if p.BureaucracyRepo != "" {
		ck := &Checkout{Type: "branch", Name: "main"}
		if p.BureaucracySHA != "" {
			ck = &Checkout{Type: "commit", SHA: p.BureaucracySHA}
		}
		resources = append(resources, Resource{
			Type:               "github_repository",
			URL:                p.BureaucracyRepo,
			AuthorizationToken: p.BureaucracyToken,
			MountPath:          bureauMount,
			Checkout:           ck,
		})
	}

	return &Session{
		AgentID:       p.AgentID,
		EnvironmentID: p.EnvironmentID,
		Metadata:      p.Metadata,
		Resources:     resources,
		Prompt:        p.Prompt,
	}, nil
}

// MarshalIndent is a convenience for `moe dispatch --dry-run` so the
// caller doesn't need to import encoding/json.
func (s *Session) MarshalIndent() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// joinMount appends rel onto base using forward slashes regardless of
// host OS. Mount paths are sandbox-side (Linux container), never host
// paths, so filepath.Join would produce wrong separators on Windows.
func joinMount(base, rel string) string {
	if base == "" {
		return "/" + rel
	}
	if rel == "" {
		return base
	}
	if base[len(base)-1] == '/' {
		return base + rel
	}
	return base + "/" + rel
}
