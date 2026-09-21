// Command voidweb serves the scraped Discord web client against a
// voidbar bouncer. Assets come from a local cache synced out of the
// discord-scraping archive (git history of that repo holds every
// build ever published, so the client can be pinned to an old,
// bouncer-friendly version), /api is reverse-proxied to the bouncer,
// and the boot config (window.GLOBAL_ENV in index.html) is rewritten
// to point at the bouncer. No client code is patched, so the bundles'
// SRI integrity hashes stay valid.
//
// The client connects to the gateway directly (wss to the bouncer
// host) and only REST flows through this server.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	bouncer := flag.String("bouncer", "https://vb.doesnmlab.xyz", "bouncer base URL")
	listen := flag.String("listen", "127.0.0.1:8090", "listen address")
	channel := flag.String("channel", "stable", "discord-scraping release channel branch")
	auth := flag.String("auth", os.Getenv("VOIDWEB_AUTH"), "bouncer credentials login:password (or set VOIDWEB_AUTH). Seeds the session token into the served page so the client boots straight into the logged-in path - the archive never captured the anonymous login screens, and their chunks exist nowhere in it")
	at := flag.String("at", "", "pin the client build to the last scrape commit on or before this date (YYYY-MM-DD; empty tracks the branch head - the most complete scrapes). Scrapes are only trustworthy from 2022-07-09 on: the scraper learned to force-load lazy chunks 2022-04-17 and to survive individual chunk failures 2022-07-09 - earlier captures miss chunks the client cannot boot without")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	base, err := url.Parse(*bouncer)
	if err != nil || base.Scheme == "" || base.Host == "" {
		logger.Error("bad bouncer URL", "bouncer", *bouncer)
		os.Exit(1)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		logger.Error("cache dir", "err", err)
		os.Exit(1)
	}
	cacheDir := filepath.Join(cache, "voidweb")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sha, err := syncAssets(ctx, logger, cacheDir, *channel, *at)
	if err != nil {
		logger.Error("asset sync", "err", err)
		os.Exit(1)
	}
	build, hash := readManifest(cacheDir)
	logger.Info("client synced", "commit", sha, "build", build, "version_hash", hash)

	index := rewriteIndex(cacheDir, base.Host)
	// Modules referenced by the captured bundles but defined nowhere
	// in them live in chunks no scrape ever captured. Their absent
	// factories crash the first synchronous require of the boot path;
	// pre-registering proxy modules (every export is a component that
	// renders nothing) keeps the app alive with those features
	// blanked out instead. 950001 is the asset-miss stub (see
	// assetLoaderMiss in sync.go): a data-URL string in place of any
	// asset module the archive's tables never mapped.
	ghosts := ghostModules(filepath.Join(cacheDir, "assets"))
	ids := make([]string, len(ghosts))
	for i, id := range ghosts {
		ids[i] = strconv.Quote(id)
	}
	seed := `<script>(function(){var f=function(e){e.exports=new Proxy(function(){return null},{get:function(t,k){if(k==="__esModule")return!0;if(k===Symbol.toPrimitive)return function(){return 0};return function(){return null}}})};var m={950001:function(e){e.exports="data:,"}};[` + strings.Join(ids, ",") + `].forEach(function(i){m[i]=f});(self.webpackChunkdiscord_app=self.webpackChunkdiscord_app||[]).push([[999999001],m])})();</script>`
	index = bytes.Replace(index, []byte("<body>"), append([]byte("<body>"), []byte(seed)...), 1)
	if len(ghosts) > 0 {
		logger.Info("ghost modules stubbed", "count", len(ghosts))
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = base.Host
			pr.SetXForwarded()
		},
	}

	// The session token, fetched from the bouncer with -auth
	// credentials and seeded into every served page (see serveIndex).
	var session atomic.Value
	tryLogin := func() {
		if session.Load() != nil {
			return
		}
		login, pass, ok := strings.Cut(*auth, ":")
		if !ok || login == "" || pass == "" {
			logger.Error("bad -auth", "want", "login:password")
			return
		}
		body, err := json.Marshal(map[string]string{"login": login, "password": pass})
		if err != nil {
			return
		}
		resp, err := http.Post(base.String()+"/api/v9/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			logger.Error("bouncer login", "err", err)
			return
		}
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&out) == nil && out.Token != "" {
			session.Store(out.Token)
			logger.Info("bouncer login ok - token seeded into served pages")
			return
		}
		logger.Error("bouncer login failed", "status", resp.StatusCode)
	}
	if *auth != "" {
		tryLogin()
	}

	assets := &assetHandler{
		dir:     filepath.Join(cacheDir, "assets"),
		channel: *channel,
		logger:  logger,
		donors:  loadDonors(cacheDir),
	}
	assets.buildChunkIDs()
	mux := http.NewServeMux()
	mux.Handle("/assets/", assets)
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pinned client may speak an older API version (v8) while
		// the bouncer only routes v9; the shapes it needs are the same.
		r.URL.Path = apiVersion.ReplaceAllString(r.URL.Path, "/api/v9/")
		proxy.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/cdn-cgi/", func(w http.ResponseWriter, r *http.Request) {
		// The scraped page still carries Cloudflare's injected beacon
		// (bot-management api.js). It cannot run here; answer with an
		// empty script so the console stays clean instead of a
		// MIME-refused 404.
		w.Header().Set("Content-Type", "application/javascript")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *auth != "" {
			tryLogin()
		}
		page := index
		if tok, _ := session.Load().(string); tok != "" {
			// Before any client script: the archived client was
			// captured logged-in, so only that boot path is complete.
			seed := []byte(`<script>try{localStorage.setItem("token",` + strconv.Quote(tok) + `)}catch(e){}</script>`)
			page = bytes.Replace(page, []byte("<body>"), append([]byte("<body>"), seed...), 1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("voidweb listening", "addr", *listen, "bouncer", base.Host)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}

var (
	apiVersion    = regexp.MustCompile(`^/api/v[0-9]+/`)
	integrityAttr = regexp.MustCompile(`\s+integrity="[^"]*"`)

	errorSink = `<script>(function(){function v(t){var d=document.getElementById("__voidweb_errs");if(!d){d=document.createElement("div");d.id="__voidweb_errs";d.style.display="none";(document.body||document.documentElement).appendChild(d)}if(d.textContent.length>16000)return;d.textContent+=t+"\n"}window.__verr=v;["log","info","warn","error","debug"].forEach(function(k){var o=console[k]?console[k].bind(console):function(){};console[k]=function(){try{v(k+": "+[].map.call(arguments,function(a){if(a instanceof Error)return a.name+": "+a.message+"\n"+String(a.stack).slice(0,900);if(typeof a==="object")return JSON.stringify(a).slice(0,300);return String(a)}).join(" ").slice(0,1000))}catch(e){}o.apply(null,arguments)}});var of=window.fetch;window.fetch=function(u,o){var us=(u&&u.url!==undefined)?u.url:String(u);v("fetch "+us);return of.apply(this,arguments).then(function(r){v("fetch "+us+" -> "+r.status);return r},function(e){v("fetch "+us+" ERR "+e);throw e})};var oo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){this.__u=u;return oo.apply(this,arguments)};var os=XMLHttpRequest.prototype.send;XMLHttpRequest.prototype.send=function(){var x=this;x.addEventListener("loadend",function(){v("xhr "+x.__u+" -> "+x.status)});return os.apply(this,arguments)};var OW=window.WebSocket;window.WebSocket=function(u,p){v("ws dial "+u);var w=p!==undefined?new OW(u,p):new OW(u);w.addEventListener("open",function(){v("ws OPEN "+u)});w.addEventListener("close",function(e){v("ws close "+u+" code="+e.code)});w.addEventListener("error",function(){v("ws ERR "+u)});return w};window.WebSocket.prototype=OW.prototype;Object.assign(window.WebSocket,OW);window.onerror=function(m,s,l,c){v("onerror: "+m+" @"+(s||"")+":"+l+":"+c);return false};window.addEventListener("unhandledrejection",function(e){var r=e.reason;v("rejection: "+(r instanceof Error?r.name+": "+r.message+"\n"+String(r.stack).slice(0,900):r))});v("sink alive")})();</script>`
)

// logRequests mirrors the bouncer's http access log format so client
// debugging flows the same way on both sides.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"dur", time.Since(start).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// rewriteIndex rewrites the GLOBAL_ENV boot config inside the scraped
// index.html: API same-origin (proxied), gateway and CDN straight at
// the bouncer host (the bouncer serves /gateway and its own /avatars
// CDN surface), webapp/assets same-origin. Keyed by config key so the
// exact original values (which drifted across Discord builds) do not
// matter.
func rewriteIndex(cacheDir, bouncerHost string) []byte {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "index.html"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "voidweb: read index.html: %v\n", err)
		os.Exit(1)
	}
	replacements := []struct{ key, value string }{
		{"API_ENDPOINT", "/api"},
		{"GATEWAY_ENDPOINT", "wss://" + bouncerHost + "/gateway"},
		{"WEBAPP_ENDPOINT", ""},
		{"CDN_HOST", bouncerHost},
		{"ASSET_ENDPOINT", ""},
		{"WIDGET_ENDPOINT", ""},
		{"DEVELOPERS_ENDPOINT", ""},
		{"MARKETING_ENDPOINT", ""},
		// The QR-login client concat's "wss:" onto this value at boot;
		// empty made the WebSocket URL invalid and rejected. The
		// bouncer has no remote-auth endpoint - the dial fails - but
		// it fails gracefully after a valid handshake attempt.
		{"REMOTE_AUTH_ENDPOINT", "//" + bouncerHost + "/remote-auth"},
		{"NETWORKING_ENDPOINT", ""},
	}
	page := string(raw)
	// The scraping archive stores beautified js/css (js-beautify for
	// readability), so bytes never match the build-time SRI hashes in
	// the integrity attributes - strip them, or the browser blocks
	// every script and stylesheet.
	page = integrityAttr.ReplaceAllString(page, "")
	// A hidden in-DOM error sink: with the app failing silently (white
	// screen, empty console) this keeps a trace reachable from a plain
	// view-source/DOM dump, including in headless runs.
	page = strings.Replace(page, "<body>", "<body>"+errorSink, 1)
	page = strings.Replace(page, "</body>", `<script>window.__verr("tail: chunks="+((window.webpackChunkdiscord_app||[]).length)+" defined="+String(Object.keys(window).length)+" ls-token="+function(){try{var t=localStorage.getItem("token");return t===null?"NULL":t.slice(0,12)}catch(e){return"ERR:"+e.message}}()+" ws="+String(window._ws!==undefined));(async function(){try{var dbs=await indexedDB.databases();var out=[];for(var d of dbs.slice(0,5)){await new Promise(function(res){var rq=indexedDB.open(d.name);rq.onsuccess=function(){var db=rq.result;var names=Array.from(db.objectStoreNames);db.close();out.push(d.name+"["+names.join(",")+"]");res()};rq.onerror=function(){out.push(d.name+"[?]");res()}})}window.__verr("idb: "+out.join(" | "))}catch(e){window.__verr("idb ERR: "+e.message)}})()</script></body>`, 1)
	for _, rep := range replacements {
		pattern := regexp.MustCompile(`(` + rep.key + `:\s*)('[^']*'|"[^"]*")`)
		if !pattern.MatchString(page) {
			continue
		}
		page = pattern.ReplaceAllString(page, `${1}'`+rep.value+`'`)
	}
	return []byte(page)
}

// ghostModules returns module ids the cached bundles reference but
// no cached chunk defines. Ids only ever appear in this build as 5-7
// digit numbers inside short-name require calls (webpack's minified
// n(...)/r(...)), .bind(_, id) entry references and .e(id) chunk
// loads; defined ids sit at module positions in each bundle.
func ghostModules(assetsDir string) []string {
	defined := map[string]bool{}
	referenced := map[string]bool{}
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		return nil
	}
	defRe := regexp.MustCompile(`(?m)^\s*(\d{5,7}): (?:\(|function|\w+ =>)`)
	refRe := regexp.MustCompile(`(?:[a-zA-Z_$]{1,2}\.e|[a-zA-Z_$]{1,2}\.bind\(this,|[a-zA-Z_$]{1,2}|\w\.bind\(\w+,)\(\s*(\d{4,7})\s*\)`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(assetsDir, e.Name()))
		if err != nil {
			continue
		}
		page := string(body)
		for _, m := range defRe.FindAllStringSubmatch(page, -1) {
			defined[m[1]] = true
		}
		for _, m := range refRe.FindAllStringSubmatch(page, -1) {
			referenced[m[1]] = true
		}
	}
	var ghosts []string
	for id := range referenced {
		if !defined[id] {
			ghosts = append(ghosts, id)
		}
	}
	sort.Strings(ghosts)
	return ghosts
}

// readManifest pulls the build number and version hash out of the
// scrape manifest for startup logging.
func readManifest(cacheDir string) (build, hash string) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "manifest.json"))
	if err != nil {
		return "?", "?"
	}
	var m struct {
		Build int    `json:"build"`
		Hash  string `json:"versionHash"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "?", "?"
	}
	return strconv.Itoa(m.Build), m.Hash
}
