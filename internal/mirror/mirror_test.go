package mirror

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const sampleHTML = `
<html><head>
<link rel="stylesheet" href="/assets/40532.a622829debe8c2c13f16.css" integrity="sha256-x">
<link rel="icon" href="/assets/ec2c34cadd4b5f4594415127380a85e6.ico">
<link rel="prefetch" as="script" href="/assets/96e49199e582769cb4c4.js"></link>
<link rel="prefetch" as="script" href="/assets/b4499d2a6b9046b1b402.js"></link>
</head><body>
<div id="app-mount"></div>
<script src="/assets/0a3803dc6cfbed7def84.js" integrity="sha256-y"></script>
<script src="/assets/dbdfce4658fd57810b7b.js"></script>
<script nonce="x">window.GLOBAL_ENV = {}</script>
</body></html>
`

func TestExtractEntryResources(t *testing.T) {
	styles, scripts := ExtractEntryResources(sampleHTML)
	if len(styles) != 1 || styles[0] != "/assets/40532.a622829debe8c2c13f16.css" {
		t.Fatalf("styles: %v", styles)
	}
	wantScripts := []string{
		"/assets/96e49199e582769cb4c4.js",
		"/assets/b4499d2a6b9046b1b402.js",
		"/assets/0a3803dc6cfbed7def84.js",
		"/assets/dbdfce4658fd57810b7b.js",
	}
	if len(scripts) != len(wantScripts) {
		t.Fatalf("scripts: %v", scripts)
	}
	sorted := append([]string(nil), scripts...)
	sort.Strings(sorted)
	sortedWant := append([]string(nil), wantScripts...)
	sort.Strings(sortedWant)
	for i := range sortedWant {
		if sorted[i] != sortedWant[i] {
			t.Fatalf("script[%d] = %s, want %s", i, sorted[i], sortedWant[i])
		}
	}
}

func TestExtractEntryResourcesDedupAndExternal(t *testing.T) {
	html := `<script src="https://cdn.example.com/x.js"></script>
	         <script src="/assets/a.js"></script><script src="/assets/a.js"></script>`
	_, scripts := ExtractEntryResources(html)
	if len(scripts) != 1 || scripts[0] != "/assets/a.js" {
		t.Fatalf("scripts: %v", scripts)
	}
}

func TestExtractChunkVariants(t *testing.T) {
	js := `{1:"abcdef1234567890",2: "fedcba0987654321", "3" : "aaaaaabbbbbb1"}`
	variants := ExtractChunkVariants(js)
	if len(variants) != 3 {
		t.Fatalf("expected 3 hashes, got %d: %v", len(variants), variants)
	}
	want := []string{"/assets/1.abcdef1234567890.js", "/assets/abcdef1234567890.js", "/assets/abcdef1234567890.css"}
	got := variants["abcdef1234567890"]
	if len(got) != len(want) {
		t.Fatalf("variants: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("variant[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestExtractChunkVariantsShortHashes(t *testing.T) {
	js := `{1:"short"}`
	if v := ExtractChunkVariants(js); len(v) != 0 {
		t.Fatalf("short hash should be ignored: %v", v)
	}
}

func TestExtractAssetURLs(t *testing.T) {
	css := `body{background:url(/assets/fonts/abc.woff2) format("woff2")}
	        .x{background-image:url("/assets/img/sprite.png")}
	        .y{background:url(data:image/png;base64,AAA)}`
	urls := ExtractAssetURLs(css)
	if len(urls) != 2 {
		t.Fatalf("urls: %v", urls)
	}
	if urls[0] != "/assets/fonts/abc.woff2" || urls[1] != "/assets/img/sprite.png" {
		t.Fatalf("urls: %v", urls)
	}
}

func TestRunLocalFilesystem(t *testing.T) {
	// Serve a tiny fake build from an httptest server and mirror it.
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app", `<!doctype html><head>
		<link rel="stylesheet" href="/assets/main.css">
		<script src="/assets/root.js"></script></head><body></body>`)
	write("assets/main.css", `body{font-face:url(/assets/f.woff2)}`)
	write("assets/root.js", `{5:"chunkhash12345"} console.log(1)`)
	write("assets/5.chunkhash12345.js", `module.exports=2`)
	write("assets/f.woff2", "font")

	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	out := t.TempDir()
	err := Run(Options{
		Base: srv.URL,
		HTML: "app",
		Out:  out,
		Log:  func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(out, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(got)
	want := []string{"app", "assets/5.chunkhash12345.js", "assets/f.woff2", "assets/main.css", "assets/root.js"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mirrored files = %v, want %v", got, want)
	}
}
