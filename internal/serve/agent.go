package serve

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/agentdefault"
)

type agentVM struct {
	Summary  string
	Revision int
	Error    string
}

func defaultAgentView(root string, now time.Time) agentVM {
	s, err := agentdefault.Read(root)
	if err != nil {
		return agentVM{Summary: "Default: unavailable", Error: err.Error()}
	}
	return agentVM{Summary: s.Summary(now), Revision: s.Revision}
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	submitted := r.FormValue("agent")
	desired := ""
	if submitted != "reset" {
		names := agent.Names()
		found := false
		for _, name := range names {
			if name == submitted {
				desired, found = name, true
				break
			}
		}
		if !found {
			http.Error(w, fmt.Sprintf("agent: unknown backend %q (registered: %v)", submitted, names), http.StatusBadRequest)
			return
		}
	}
	revision, err := strconv.Atoi(r.FormValue("revision"))
	if err != nil || revision < 0 {
		http.Error(w, "agent: revision is required", http.StatusBadRequest)
		return
	}
	_, err = agentdefault.Set(s.opts.Root, desired, &revision, time.Now)
	if errors.Is(err, agentdefault.ErrStale) {
		vm := defaultAgentView(s.opts.Root, time.Now())
		vm.Error = err.Error()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		s.render(w, r, "agent_conflict.html", vm)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
