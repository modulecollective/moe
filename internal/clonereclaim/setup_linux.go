//go:build linux

package clonereclaim

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"

	"github.com/modulecollective/moe/internal/runowner"
)

type SetupIntent struct {
	Version    int      `json:"version"`
	Generation string   `json:"generation"`
	ID         string   `json:"id"`
	Setup      []Script `json:"setup"`
	Teardown   []Script `json:"teardown"`
	Cache      []byte   `json:"cache"`
	Digest     string   `json:"sha256"`
}

type setupComplete struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	ID         string `json:"id"`
	Resource   string `json:"resource"`
}

func (r *Registry) setupDir(id string, create bool) (*os.File, error) {
	if !validRandomID(id) {
		return nil, errors.New("invalid setup attempt id")
	}
	parent, err := privateChildDir(r.dir, "setups", create)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return privateChildDir(parent, id, create)
}

// BeginSetup records collected exports immediately before cache publication.
// Pre-cache hook failures keep their existing synchronous rollback; allocations
// never reported by setup remain outside this protocol. The immutable original
// bytes survive even if subsequent ownership publication or rollback fails.
func (r *Registry) BeginSetup(h *runowner.Handle, cache []byte, setup, teardown []Script) (SetupIntent, error) {
	var intent SetupIntent
	if err := r.ValidateEnvironment(h); err != nil {
		return intent, err
	}
	if _, exists, err := r.CacheBytes(h); err != nil {
		return intent, err
	} else if exists {
		return intent, errors.New("setup cannot overwrite a cached environment")
	}
	if len(setup) > 0 && len(teardown) == 0 {
		return intent, errors.New("setup lacks a teardown contract")
	}
	intent = SetupIntent{Version: protocolVersion, Generation: r.Identity.Generation, Setup: setup, Teardown: teardown, Cache: bytes.Clone(cache), Digest: digest(cache)}
	if len(cache) > cacheLimit {
		return intent, errors.New("setup cache size limit exceeded")
	}
	var err error
	intent.ID, err = randomID()
	if err != nil {
		return intent, err
	}
	dir, err := r.setupDir(intent.ID, true)
	if err != nil {
		return intent, err
	}
	defer dir.Close()
	if err := publishRecord(dir, "intent.json", intent); err != nil {
		return intent, err
	}
	return intent, nil
}

// CompleteSetup follows durable ownership publication. Failure here retains
// the owned cache; it must not trigger rollback of an already-owned environment.
func (r *Registry) CompleteSetup(h *runowner.Handle, intent SetupIntent, snapshot Resources) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	dir, err := r.setupDir(intent.ID, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	recorded, err := readRecord[SetupIntent](dir, "intent.json")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(intent, recorded) || intent.Version != protocolVersion || intent.Generation != r.Identity.Generation || snapshot.Generation != intent.Generation || !reflect.DeepEqual(intent.Setup, snapshot.Setup) || !reflect.DeepEqual(intent.Teardown, snapshot.Teardown) || !bytes.Equal(intent.Cache, snapshot.Cache) || intent.Digest != snapshot.Digest {
		return errors.New("setup completion differs from admitted scripts/ownership")
	}
	resource, err := r.resourceDir(snapshot.ID, false)
	if err != nil {
		return err
	}
	defer resource.Close()
	saved, err := readRecord[Resources](resource, "ownership.json")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(saved, snapshot) {
		return errors.New("setup resource snapshot changed")
	}
	return publishRecord(dir, "complete.json", setupComplete{Version: protocolVersion, Generation: intent.Generation, ID: intent.ID, Resource: snapshot.ID})
}

func (r *Registry) checkSetups(history []Resources) error {
	parent, err := privateChildDir(r.dir, "setups", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(4097)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 4096 {
		return errors.New("setup receipt scan limit exceeded")
	}
	resources := make(map[string]Resources, len(history))
	for _, resource := range history {
		resources[resource.ID] = resource
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || !validRandomID(entry.Name()) {
			return errors.New("unknown setup recovery entry")
		}
		dir, err := privateChildDir(parent, entry.Name(), false)
		if err != nil {
			return err
		}
		intent, intentErr := readRecord[SetupIntent](dir, "intent.json")
		complete, completeErr := readRecord[setupComplete](dir, "complete.json")
		path := dir.Name()
		dir.Close()
		if intentErr != nil {
			return intentErr
		}
		if completeErr != nil {
			if errors.Is(completeErr, os.ErrNotExist) {
				return &HoldError{Reason: "setup ownership publication unfinished", Receipt: path}
			}
			return completeErr
		}
		resource, ok := resources[complete.Resource]
		if !ok || seen[complete.Resource] || intent.Version != protocolVersion || complete.Version != protocolVersion || intent.ID != entry.Name() || intent.Generation != r.Identity.Generation || complete.Generation != intent.Generation || complete.ID != intent.ID || !reflect.DeepEqual(intent.Setup, resource.Setup) || !reflect.DeepEqual(intent.Teardown, resource.Teardown) || intent.Digest != resource.Digest || !bytes.Equal(intent.Cache, resource.Cache) {
			return errors.New("setup receipt contradicts resource ownership")
		}
		seen[complete.Resource] = true
	}
	return nil
}
