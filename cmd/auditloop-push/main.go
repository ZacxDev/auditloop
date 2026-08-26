// Command auditloop-push is the generic reference uploader for auditloop's P5
// plugin-push API. It reads a push-schema metadata JSON file + the referenced
// artifact files from a directory, builds the multipart body, POSTs it to
// {base}/api/plugins/runs with a bearer push token, and prints the resulting run
// URL.
//
// A harness (CI, a ux-audit loop) produces metadata.json by mapping its own
// audit output to the push schema (github.com/ZacxDev/auditloop/internal/plugin.PushPayload) and
// dropping the referenced screenshots/axe/network files into --files. This binary
// is intentionally app-agnostic — it ships in THIS repo and needs no changes to
// any harness repo.
//
// Usage:
//
//	auditloop-push --url https://auditloop.example.com \
//	  --token <PUSH_TOKEN> --meta metadata.json --files ./artifacts
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ZacxDev/auditloop/internal/plugin"

	"net/http"
)

func main() {
	base := flag.String("url", "", "auditloop base URL (e.g. https://auditloop.example.com)")
	token := flag.String("token", "", "plugin push token (or set AUDITLOOP_PUSH_TOKEN)")
	meta := flag.String("meta", "metadata.json", "path to the push-schema metadata JSON file")
	files := flag.String("files", ".", "directory containing the artifact files referenced by the metadata")
	timeout := flag.Duration("timeout", 60*time.Second, "HTTP timeout")
	flag.Parse()

	tok := *token
	if tok == "" {
		tok = os.Getenv("AUDITLOOP_PUSH_TOKEN")
	}
	if *base == "" || tok == "" {
		fmt.Fprintln(os.Stderr, "error: --url and --token (or AUDITLOOP_PUSH_TOKEN) are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: *timeout}
	res, err := plugin.UploadFromDisk(ctx, client, *base, tok, *meta, *files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "push failed:", err)
		os.Exit(1)
	}
	fmt.Println("pushed run:", res.URL)
}
