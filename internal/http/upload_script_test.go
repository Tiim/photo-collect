package http_test

import (
	"os/exec"
	"testing"
)

// TestUploadScriptBacksOffOnRateLimit runs the real upload script against a fake
// DOM (needs node; skipped when it is not installed).
func TestUploadScriptBacksOffOnRateLimit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "testdata/upload_retry.js", "../../web/static/app.js").CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
}
