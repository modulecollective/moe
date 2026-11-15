package runcontrol

import "github.com/modulecollective/moe/internal/atomicfile"

// replaceFile publishes a complete generation for lock-free readers, including
// during rollback. The repository lock still serializes writers. A var so
// tests can fail a publish part-way through a rollback.
var replaceFile = func(path string, body []byte) error {
	return atomicfile.Write(path, body, 0o644)
}
