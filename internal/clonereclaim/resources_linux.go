//go:build linux

package clonereclaim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/modulecollective/moe/internal/runowner"
)

const cacheName = "dev-env.env"
const cacheLimit = 64 << 10

type Script struct {
	Path   string `json:"path"`
	Digest string `json:"sha256"`
}

// Resources retains original cache bytes, including private addresses. It is
// stored only in private local receipts and must never be rendered as output.
type Resources struct {
	Version      int      `json:"version"`
	Generation   string   `json:"generation"`
	ID           string   `json:"id"`
	Sequence     int      `json:"sequence"`
	Previous     string   `json:"previous,omitempty"`
	Cache        []byte   `json:"cache"`
	Digest       string   `json:"sha256"`
	Setup        []Script `json:"setup"`
	Teardown     []Script `json:"teardown"`
	ResourceFree bool     `json:"resource_free"`
}

type CleanupIntent struct {
	Version     int      `json:"version"`
	Generation  string   `json:"generation"`
	ID          string   `json:"id"`
	Resource    string   `json:"resource"`
	Purpose     string   `json:"purpose"`
	CacheDigest string   `json:"cache_sha256"`
	Scripts     []Script `json:"scripts"`
}

type CleanupCompletion struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Resource   string `json:"resource"`
	Attempt    string `json:"attempt"`
	ExitCode   int    `json:"exit_code"`
}

type cacheCleared struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Resource   string `json:"resource"`
	Attempt    string `json:"attempt"`
}

// HoldError names recovery records, never resource addresses or cache bytes.
type HoldError struct {
	Reason, Receipt string
	// Admission cannot establish retained evidence when registry loading fails.
	recordsLocationOnly bool
}

func (e *HoldError) Error() string {
	if e.recordsLocationOnly {
		return fmt.Sprintf("clone cleanup held: %s; recovery records location: %s", e.Reason, e.Receipt)
	}
	return fmt.Sprintf("clone cleanup held: %s; retained evidence: %s", e.Reason, e.Receipt)
}

type CleanupPlan struct {
	Resources        *Resources
	Scripts          []Script
	Purpose          string
	Receipt          string
	CompletedAttempt string
	CacheCleared     bool
	retry            bool
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// ScriptIdentities reads exact regular executable files, without following
// symlinked parents or script files. Paths remain relative to canonical root.
func ScriptIdentities(root string, paths []string) ([]Script, error) {
	if len(paths) > 1024 {
		return nil, errors.New("hook identity scan limit exceeded")
	}
	var out []Script
	for _, path := range paths {
		if !filepath.IsLocal(path) || filepath.Clean(path) != path {
			return nil, errors.New("unsafe hook script path")
		}
		parent, err := openAbsoluteDir(filepath.Join(root, filepath.Dir(path)))
		if err != nil {
			return nil, err
		}
		b, mode, err := readRegularAt(parent, filepath.Base(path), 1<<20)
		parent.Close()
		if err != nil {
			return nil, err
		}
		if mode&0o111 == 0 {
			return nil, errors.New("hook script is not executable")
		}
		out = append(out, Script{Path: path, Digest: digest(b)})
	}
	return out, nil
}

// ValidateEnvironment is admission's cache-ownership check. Neither an
// interrupted/failed cleanup nor an unrecorded cache can become a cache hit.
func (r *Registry) ValidateEnvironment(h *runowner.Handle) error {
	if err := r.CheckShells(h); err != nil {
		return err
	}
	history, err := r.resources()
	if err != nil {
		return err
	}
	if err := r.checkSetups(history); err != nil {
		return err
	}
	cache, exists, err := r.CacheBytes(h)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		if exists {
			return &HoldError{Reason: "cache lacks resource ownership", Receipt: r.dir.Name()}
		}
		return nil
	}
	snapshot := history[len(history)-1]
	complete, failed, err := r.inspectAttempts(snapshot)
	if err != nil {
		return err
	}
	cleared, err := r.readCleared(snapshot)
	if err != nil {
		return err
	}
	if cleared != nil {
		if complete == "" || complete != cleared.Attempt {
			return errors.New("cache-clear receipt lacks proven teardown success")
		}
		if exists {
			return errors.New("unrecorded cache after resource cleanup")
		}
		return nil
	}
	if complete != "" || failed {
		return &HoldError{Reason: "resource cleanup pending", Receipt: r.resourcePath(snapshot.ID)}
	}
	if !exists || !bytes.Equal(cache, snapshot.Cache) {
		return errors.New("original owned cache missing or changed")
	}
	return nil
}

func readRegularAt(parent *os.File, name string, limit int64) ([]byte, os.FileMode, error) {
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, 0, errors.New("invalid or oversized cleanup input")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(b)) > limit {
		return nil, 0, errors.New("cleanup input limit exceeded")
	}
	return b, info.Mode(), nil
}

func (r *Registry) CacheBytes(h *runowner.Handle) ([]byte, bool, error) {
	if err := r.validateOwner(h); err != nil {
		return nil, false, err
	}
	_, tree, err := physicalIdentity(h, false)
	if err != nil {
		return nil, false, err
	}
	defer tree.Close()
	local, err := childDir(tree, ".moe", false)
	if err != nil {
		return nil, false, err
	}
	defer local.Close()
	b, _, err := readRegularAt(local, cacheName, cacheLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (r *Registry) resourceDir(id string, create bool) (*os.File, error) {
	if !validRandomID(id) {
		return nil, errors.New("invalid resource receipt id")
	}
	parent, err := privateChildDir(r.dir, "resources", create)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return privateChildDir(parent, id, create)
}

func (r *Registry) resources() ([]Resources, error) {
	parent, err := privateChildDir(r.dir, "resources", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("resource receipt scan limit exceeded")
	}
	var out []Resources
	for _, entry := range entries {
		if !entry.IsDir() || !validRandomID(entry.Name()) {
			return nil, errors.New("unknown resource recovery entry")
		}
		dir, err := privateChildDir(parent, entry.Name(), false)
		if err != nil {
			return nil, err
		}
		snapshot, err := readRecord[Resources](dir, "ownership.json")
		if errors.Is(err, os.ErrNotExist) && r.unpublishedResolved(dir, entry.Name()) {
			dir.Close()
			continue
		}
		dir.Close()
		if err != nil {
			return nil, fmt.Errorf("resource ownership unresolved at %s: %w", entry.Name(), err)
		}
		if err := r.validateResources(snapshot); err != nil {
			return nil, err
		}
		if snapshot.ID != entry.Name() {
			return nil, errors.New("resource directory identity mismatch")
		}
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	previous := ""
	for i, snapshot := range out {
		if snapshot.Sequence != i+1 || snapshot.Previous != previous {
			return nil, errors.New("contradictory resource ownership history")
		}
		previous = snapshot.ID
	}
	return out, nil
}

func (r *Registry) validateResources(s Resources) error {
	if s.Version != protocolVersion || s.Generation != r.Identity.Generation || !validRandomID(s.ID) || s.Sequence < 1 || len(s.Cache) > cacheLimit || s.Digest != digest(s.Cache) {
		return errors.New("invalid resource ownership")
	}
	first, _, ok := bytes.Cut(s.Cache, []byte{'\n'})
	fields := strings.Fields(string(first))
	if !ok || len(fields) != 4 || fields[0] != "#" || fields[1] != "moe-dev-env-v1" || fields[2] != r.Identity.Project+"/"+r.Identity.Run || (fields[3] != "none" && !objectID(fields[3])) {
		return errors.New("resource cache owner/version mismatch")
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(s.Cache[len(first)+1:]), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsRune(line, 0) || seen[key] {
			return errors.New("malformed resource cache exports")
		}
		for i, c := range key {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
				return errors.New("invalid resource cache key")
			}
		}
		seen[key] = true
	}
	if s.ResourceFree && (len(s.Setup) != 0 || len(bytes.TrimSpace(s.Cache[len(first)+1:])) != 0) {
		return errors.New("nonempty environment lacks a disposal contract")
	}
	if !s.ResourceFree && len(s.Teardown) == 0 {
		return errors.New("resource ownership has no teardown contract")
	}
	for _, group := range []struct {
		scripts []Script
		event   string
	}{{s.Setup, "dev-env.d"}, {s.Teardown, "dev-env-teardown.d"}} {
		previous := ""
		prefix := filepath.Join("projects", r.Identity.Project, "hooks", group.event)
		for _, script := range group.scripts {
			if filepath.Dir(script.Path) != prefix || filepath.Clean(script.Path) != script.Path || !filepath.IsLocal(script.Path) || len(script.Digest) != 64 || !objectID(script.Digest) || script.Path <= previous {
				return errors.New("invalid ordered hook identities")
			}
			previous = script.Path
		}
	}
	return nil
}

// RecordResources publishes exact reusable-cache ownership. A later setup may
// append only after the previous snapshot's teardown and cache clear completed.
func (r *Registry) RecordResources(h *runowner.Handle, cache []byte, setup, teardown []Script, free bool) (Resources, error) {
	var snapshot Resources
	if err := r.CheckShells(h); err != nil {
		return snapshot, err
	}
	history, err := r.resources()
	if err != nil {
		return snapshot, err
	}
	if len(history) > 0 {
		previous := history[len(history)-1]
		cleared, err := r.readCleared(previous)
		if err != nil {
			return snapshot, err
		}
		if cleared == nil {
			return snapshot, &HoldError{Reason: "previous resource cleanup pending", Receipt: r.resourcePath(previous.ID)}
		}
		complete, _, err := r.inspectAttempts(previous)
		if err != nil {
			return snapshot, err
		}
		if complete == "" || cleared.Attempt != complete {
			return snapshot, errors.New("previous ownership lacks proven teardown success")
		}
		snapshot.Previous = previous.ID
	}
	snapshot.Version, snapshot.Generation, snapshot.Sequence = protocolVersion, r.Identity.Generation, len(history)+1
	snapshot.ID, err = randomID()
	if err != nil {
		return snapshot, err
	}
	snapshot.Cache = bytes.Clone(cache)
	snapshot.Digest = digest(cache)
	snapshot.Setup = setup
	snapshot.Teardown = teardown
	snapshot.ResourceFree = free
	if err := r.validateResources(snapshot); err != nil {
		return snapshot, err
	}
	actual, ok, err := r.CacheBytes(h)
	if err != nil {
		return snapshot, err
	}
	if !ok || !bytes.Equal(actual, cache) {
		return snapshot, errors.New("published cache differs from resource snapshot")
	}
	dir, err := r.resourceDir(snapshot.ID, true)
	if err != nil {
		return snapshot, err
	}
	defer dir.Close()
	if err := publishRecord(dir, "ownership.json", snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (r *Registry) resourcePath(id string) string {
	return filepath.Join(r.dir.Name(), "resources", id)
}

func (r *Registry) readCleared(snapshot Resources) (*cacheCleared, error) {
	dir, err := r.resourceDir(snapshot.ID, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	cleared, err := readRecord[cacheCleared](dir, "cache-cleared.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cleared.Version != protocolVersion || cleared.Generation != snapshot.Generation || cleared.Resource != snapshot.ID || !validRandomID(cleared.Attempt) {
		return nil, errors.New("invalid cache-clear receipt")
	}
	return &cleared, nil
}

func (r *Registry) inspectAttempts(snapshot Resources) (string, bool, error) {
	dir, err := r.resourceDir(snapshot.ID, false)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	parent, err := privateChildDir(dir, "attempts", false)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(4097)
	if err != nil && err != io.EOF {
		return "", false, err
	}
	if len(entries) > 4096 {
		return "", false, errors.New("cleanup attempt scan limit exceeded")
	}
	complete := ""
	failed := false
	for _, entry := range entries {
		if !entry.IsDir() || !validRandomID(entry.Name()) {
			return "", false, errors.New("unknown cleanup recovery entry")
		}
		attempt, err := privateChildDir(parent, entry.Name(), false)
		if err != nil {
			return "", false, err
		}
		intent, readErr := readRecord[CleanupIntent](attempt, "intent.json")
		result, resultErr := readRecord[CleanupCompletion](attempt, "teardown-complete.json")
		resolved := errors.Is(resultErr, os.ErrNotExist) && stopped(attempt, r.Identity.Generation, entry.Name()) == nil
		path := attempt.Name()
		attempt.Close()
		if readErr != nil {
			return "", false, fmt.Errorf("unreadable cleanup intent at %s: %w", path, readErr)
		}
		if intent.Version != protocolVersion || intent.Generation != snapshot.Generation || intent.Resource != snapshot.ID || intent.ID != entry.Name() || intent.CacheDigest != snapshot.Digest || !r.scriptsApproved(snapshot, intent.Scripts) || !cleanupPurpose(intent.Purpose) {
			return "", false, errors.New("cleanup intent contradicts resource ownership")
		}
		if resultErr != nil {
			if resolved {
				failed = true
				continue
			}
			if errors.Is(resultErr, os.ErrNotExist) {
				return "", false, &HoldError{Reason: "interrupted teardown needs stopped-worker recovery", Receipt: path}
			}
			return "", false, resultErr
		}
		if result.Version != protocolVersion || result.Generation != snapshot.Generation || result.Resource != snapshot.ID || result.Attempt != intent.ID || result.ExitCode < 0 || result.ExitCode > 255 {
			return "", false, errors.New("invalid teardown completion")
		}
		if result.ExitCode == 0 {
			if complete != "" {
				return "", false, errors.New("multiple successful teardown attempts")
			}
			complete = intent.ID
		} else {
			failed = true
		}
	}
	return complete, failed, nil
}

func cleanupPurpose(purpose string) bool {
	return purpose == "terminal" || purpose == "refresh" || purpose == "rebind"
}

// PlanCleanup never launches hooks or edits clone/cache contents. A confirmed
// failure requires an exact-run retry caller; an interrupted worker never does.
func (r *Registry) PlanCleanup(h *runowner.Handle, purpose string, scripts []Script, retry bool) (CleanupPlan, error) {
	plan := CleanupPlan{Purpose: purpose, Receipt: r.dir.Name(), retry: retry}
	if !cleanupPurpose(purpose) {
		return plan, errors.New("unknown clone cleanup purpose")
	}
	if err := r.CheckShells(h); err != nil {
		return plan, err
	}
	history, err := r.resources()
	if err != nil {
		return plan, err
	}
	if err := r.checkSetups(history); err != nil {
		return plan, err
	}
	cache, exists, err := r.CacheBytes(h)
	if err != nil {
		return plan, err
	}
	if len(history) == 0 {
		if exists {
			return plan, &HoldError{Reason: "cache has no recorded resource ownership", Receipt: r.dir.Name()}
		}
		return plan, nil
	}
	snapshot := history[len(history)-1]
	plan.Resources = &snapshot
	plan.Scripts = scripts
	plan.Receipt = r.resourcePath(snapshot.ID)
	complete, failed, err := r.inspectAttempts(snapshot)
	if err != nil {
		return plan, err
	}
	plan.CompletedAttempt = complete
	cleared, err := r.readCleared(snapshot)
	if err != nil {
		return plan, err
	}
	if cleared != nil {
		if complete == "" || cleared.Attempt != complete {
			return plan, errors.New("cache-clear receipt lacks matching teardown success")
		}
		if exists {
			return plan, errors.New("cache exists after its recorded ownership was cleared")
		}
		plan.CacheCleared = true
		return plan, nil
	}
	if complete != "" {
		if exists && !bytes.Equal(cache, snapshot.Cache) {
			return plan, errors.New("cache replaced after teardown completion")
		}
		return plan, nil
	}
	if !exists || !bytes.Equal(cache, snapshot.Cache) {
		return plan, errors.New("original resource cache missing or changed")
	}
	if failed && !retry {
		return plan, &HoldError{Reason: "failed teardown requires exact-run retry", Receipt: plan.Receipt}
	}
	if !r.scriptsApproved(snapshot, scripts) {
		return plan, &HoldError{Reason: "teardown scripts missing or changed", Receipt: plan.Receipt}
	}
	return plan, nil
}

func (r *Registry) beginCleanup(h *runowner.Handle, plan CleanupPlan) (CleanupIntent, *os.File, error) {
	var intent CleanupIntent
	if err := r.validateOwner(h); err != nil {
		return intent, nil, err
	}
	if plan.Resources == nil || plan.CompletedAttempt != "" || plan.CacheCleared {
		return intent, nil, errors.New("cleanup plan needs no worker")
	}
	fresh, err := r.PlanCleanup(h, plan.Purpose, plan.Scripts, plan.retry)
	if err != nil {
		return intent, nil, err
	}
	if !reflect.DeepEqual(plan, fresh) {
		return intent, nil, errors.New("cleanup plan changed before application")
	}
	intent = CleanupIntent{Version: protocolVersion, Generation: r.Identity.Generation, Resource: plan.Resources.ID, Purpose: plan.Purpose, CacheDigest: plan.Resources.Digest, Scripts: plan.Scripts}
	intent.ID, err = randomID()
	if err != nil {
		return intent, nil, err
	}
	resource, err := r.resourceDir(intent.Resource, false)
	if err != nil {
		return intent, nil, err
	}
	defer resource.Close()
	attempts, err := privateChildDir(resource, "attempts", true)
	if err != nil {
		return intent, nil, err
	}
	defer attempts.Close()
	dir, err := privateChildDir(attempts, intent.ID, true)
	if err != nil {
		return intent, nil, err
	}
	if err := publishRecord(dir, "intent.json", intent); err != nil {
		dir.Close()
		return intent, nil, err
	}
	return intent, dir, nil
}

// ClearCompletedCache never re-executes hooks. Durable teardown success must
// precede unlink, and an unlink/sync/publication failure can retry only this step.
func (r *Registry) ClearCompletedCache(h *runowner.Handle, plan CleanupPlan) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	if plan.Resources == nil {
		return nil
	}
	snapshot := *plan.Resources
	fresh, err := r.PlanCleanup(h, plan.Purpose, snapshot.Teardown, true)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Resources, plan.Resources) || fresh.CompletedAttempt != plan.CompletedAttempt {
		return errors.New("cleanup ownership changed before cache clear")
	}
	complete, _, err := r.inspectAttempts(snapshot)
	if err != nil {
		return err
	}
	if complete == "" || complete != plan.CompletedAttempt {
		return errors.New("cache clear lacks proven teardown success")
	}
	if err := r.persistCleanup(snapshot, complete); err != nil {
		return err
	}
	_, tree, err := physicalIdentity(h, false)
	if err != nil {
		return err
	}
	defer tree.Close()
	local, err := childDir(tree, ".moe", false)
	if err != nil {
		return err
	}
	defer local.Close()
	actual, _, err := readRegularAt(local, cacheName, cacheLimit)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if !bytes.Equal(actual, snapshot.Cache) {
			return errors.New("cache changed before clear")
		}
		if err := syscall.Unlinkat(int(local.Fd()), cacheName); err != nil {
			return err
		}
	}
	if err := syncRecordFile(local); err != nil {
		return err
	}
	dir, err := r.resourceDir(snapshot.ID, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	receipt := cacheCleared{Version: protocolVersion, Generation: r.Identity.Generation, Resource: snapshot.ID, Attempt: complete}
	existing, err := readRecord[cacheCleared](dir, "cache-cleared.json")
	if err == nil {
		if existing != receipt {
			return errors.New("cache-clear receipt changed")
		}
		return persistRecord(dir, "cache-cleared.json")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return publishRecord(dir, "cache-cleared.json", receipt)
}

func (r *Registry) persistCleanup(snapshot Resources, id string) error {
	resource, err := r.resourceDir(snapshot.ID, false)
	if err != nil {
		return err
	}
	defer resource.Close()
	attempts, err := privateChildDir(resource, "attempts", false)
	if err != nil {
		return err
	}
	defer attempts.Close()
	attempt, err := privateChildDir(attempts, id, false)
	if err != nil {
		return err
	}
	defer attempt.Close()
	for _, name := range []string{"intent.json", "teardown-complete.json"} {
		if err := persistRecord(attempt, name); err != nil {
			return err
		}
	}
	if err := persistRecord(resource, "ownership.json"); err != nil {
		return err
	}
	return syncRecordFile(attempts)
}

// PersistCleared re-establishes durable prior cleanup before setup can allocate
// a successor. Readable receipts from a failed fsync are never enough alone.
func (r *Registry) PersistCleared(h *runowner.Handle) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	history, err := r.resources()
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return nil
	}
	latest := history[len(history)-1]
	cleared, err := r.readCleared(latest)
	if err != nil {
		return err
	}
	if cleared == nil {
		return nil
	}
	if err := r.persistCleanup(latest, cleared.Attempt); err != nil {
		return err
	}
	resource, err := r.resourceDir(latest.ID, false)
	if err != nil {
		return err
	}
	defer resource.Close()
	return persistRecord(resource, "cache-cleared.json")
}

func persistRecord(parent *os.File, name string) error {
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if err := syncRecordFile(f); err != nil {
		return err
	}
	return syncRecordFile(parent)
}
