package cli

import (
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// fakeBuildCanvasesSnippet is the shell a fake agent runs to play a
// build turn: it reads the canvas paths the prompt names under "Your
// canvases for this document are these files" and overwrites each with
// a stamped canvas — a ready gate and one filled section carrying the
// marker — so the derived build gate lets the run advance exactly as a
// real, finished build turn would. $prompt must already hold the
// --append-system-prompt payload. Exposes the first path as
// $build_canvas for scripts that key a failure on it.
func fakeBuildCanvasesSnippet(marker string) string {
	return `build_files=$(printf '%s' "$prompt" | awk '/Your canvases for this document are these files, all writable:/ {f=1; next} f && /^ +\// {gsub(/^ +| +$/, ""); print; next} f {exit}')
build_canvas=
for bf in $build_files; do
  [ -n "$build_canvas" ] || build_canvas=$bf
  printf '# Build\n\n## Gate\n\n` + "```" + `json\n{"status":"ready"}\n` + "```" + `\n\n## Notes\n\n%s\n' '` + marker + `' > "$bf"
done
`
}

// stampedBuildCanvas is a build content file the derived gate accepts:
// a ready gate and one filled section.
const stampedBuildCanvas = "# Build\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\n## Notes\n\nfinished and verified\n"

// writeStampedBuild writes both declared build canvases, stamped, under
// root — a run whose build has finished and may advance.
func writeStampedBuild(t *testing.T, root string, md *run.Metadata) {
	t.Helper()
	for _, name := range []string{"code.md", "test.md"} {
		writeBuildCanvas(t, root, md, name, stampedBuildCanvas)
	}
}
