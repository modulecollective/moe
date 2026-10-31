package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/modulecollective/moe/internal/run"
)

// TranscriptSessionID streams native records so large tool output does not
// impose a scanner token limit. Every identity-bearing record must agree.
func (Agent) TranscriptSessionID(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("claude: open transcript %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var sessionID string
	for {
		var record struct {
			SessionID string `json:"sessionId"`
		}
		err := dec.Decode(&record)
		if errors.Is(err, io.EOF) {
			return sessionID, nil
		}
		if err != nil {
			return "", fmt.Errorf("claude: decode transcript %s: %w", path, err)
		}
		id := record.SessionID
		if id == "" {
			continue
		}
		if !run.ValidSessionID(id) {
			return "", fmt.Errorf("claude: invalid session ID %q in %s", id, path)
		}
		if sessionID != "" && sessionID != id {
			return "", fmt.Errorf("claude: conflicting session IDs %q and %q in %s", sessionID, id, path)
		}
		sessionID = id
	}
}
