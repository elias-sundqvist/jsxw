package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/fsnotify/fsnotify"
	webview_selector "github.com/jchv/go-webview-selector"
)

// --- add at top of file ---
type esbuildConfig struct {
	Platform        string            `json:"platform"`
	Target          string            `json:"target"`
	LogLevel        string            `json:"logLevel"`
	Minify          bool              `json:"minify"`
	Define          map[string]string `json:"define"`
	JSX             string            `json:"jsx"`             // "automatic" | "transform"
	JSXImportSource string            `json:"jsxImportSource"` // e.g. "react"
	Loaders         map[string]string `json:"loaders"`         // ext -> "js"|"jsx"|"tsx"|"ts"|"text"
}

func readEsbuildConfig(path string) (*esbuildConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg esbuildConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func mapLogLevel(s string) api.LogLevel {
	switch strings.ToLower(s) {
	case "verbose":
		return api.LogLevelVerbose
	case "debug":
		return api.LogLevelDebug
	case "info":
		return api.LogLevelInfo
	case "warning":
		return api.LogLevelWarning
	case "silent":
		fallthrough
	default:
		return api.LogLevelSilent
	}
}

func mapPlatform(s string) api.Platform {
	switch strings.ToLower(s) {
	case "node":
		return api.PlatformNode
	case "neutral":
		return api.PlatformNeutral
	default:
		return api.PlatformBrowser
	}
}

func mapLoader(s string) api.Loader {
	switch strings.ToLower(s) {
	case "js":
		return api.LoaderJS
	case "jsx":
		return api.LoaderJSX
	case "ts":
		return api.LoaderTS
	case "tsx":
		return api.LoaderTSX
	case "json":
		return api.LoaderJSON
	case "text":
		return api.LoaderText
	default:
		return api.LoaderJS
	}
}

// ---- Embed your assets (used for production / no-DEV) ----

//go:embed web/index.html
var indexHTML string

//go:embed web/main.js
var mainJS string

//go:embed web/style.css
var styleCSS string

type appMode struct {
	entryPath       string
	inlineSource    string
	inlineExt       string
	inlineBaseDir   string
	contentRoot     string
	title           string
	watchDir        string
	showHelp        bool
	showVersion     bool
	register        bool
	setDefaultAssoc bool
	serve           bool
	serveHost       string
	servePort       int
	allowRemote     bool
}

type appBundleStore struct {
	mu   sync.RWMutex
	data string
}

func newAppBundleStore(data string) *appBundleStore {
	return &appBundleStore{data: data}
}

func (s *appBundleStore) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *appBundleStore) set(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
}

func main() {
	// macOS webviews prefer main thread
	if runtime.GOOS == "darwin" {
		runtime.LockOSThread()
	}

	appRoot, err := resolveAppRoot()
	if err != nil {
		log.Fatal(err)
	}

	mode, err := detectAppMode(appRoot, os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if mode.showHelp {
		fmt.Print(cliUsage())
		return
	}
	if mode.showVersion {
		fmt.Println(resolveVersion(appRoot))
		return
	}
	if mode.register {
		if err := runRegistration(appRoot, mode.setDefaultAssoc); err != nil {
			log.Fatal(err)
		}
		return
	}
	if mode.serve {
		if err := runServeMode(appRoot, mode); err != nil {
			log.Fatal(err)
		}
		return
	}

	var (
		finalHTML   string
		bundleStore *appBundleStore
		windowHost  *windowContentHost
	)
	if mode.entryPath != "" {
		initialBundle, err := buildAppBundle(appRoot, mode)
		if err != nil {
			log.Fatal(err)
		}
		bundleStore = newAppBundleStore(initialBundle)
		finalHTML, err = buildWindowLoaderHTML(appRoot, mode)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		finalHTML, err = buildHTML(appRoot, mode)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Serve to webview via data: URL (no server, no temp files)
	dataURL := makeDataURL(finalHTML)

	w := webview_selector.New(true)
	defer w.Destroy()
	w.SetTitle(mode.title)
	w.SetSize(900, 640, webview_selector.HintNone)
	reloadState := newReloadStateStore()
	if err := initReloadStateBridge(w, reloadState); err != nil {
		log.Println("reload state:", err)
	}
	navigateURL := dataURL
	if bundleStore != nil {
		if mode.entryPath != "" {
			windowHost, err = initWindowContentHost(w, mode, finalHTML, bundleStore.get())
			if err != nil {
				log.Println("window content host:", err)
			} else {
				navigateURL = windowHost.navigateURL
			}
		}
	}
	if err := initWindowChrome(w); err != nil {
		log.Println("window chrome:", err)
	}
	w.Navigate(navigateURL)

	if mode.watchDir != "" {
		// Always watch files and live-reload the page when something changes
		go func() {
			if err := watchAndReload(mode.watchDir, func() {
				if mode.entryPath != "" {
					bundle, err := buildAppBundle(appRoot, mode)
					if err != nil {
						log.Println("reload bundle:", err)
						return
					}
					bundleStore.set(bundle)
					if windowHost != nil {
						if err := windowHost.updateBundle(bundle); err != nil {
							log.Println("reload host bundle:", err)
							return
						}
					}

					w.Dispatch(func() {
						w.Eval(`window.__codexReloadFromHost && window.__codexReloadFromHost()`)
					})
					return
				}

				final, err := buildHTML(appRoot, mode)
				if err != nil {
					log.Println("reload:", err)
					return
				}
				url := makeDataURL(final)

				w.Dispatch(func() {
					w.Navigate(url)
				})
			}); err != nil {
				log.Println("watch:", err)
			}
		}()
	}

	w.Run()
}

func makeDataURL(html string) string {
	return "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(html))
}

func resolveAppRoot() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	exeDir := filepath.Dir(exePath)
	if hasProjectAssets(exeDir) {
		return exeDir, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if hasProjectAssets(cwd) {
		return cwd, nil
	}

	return exeDir, nil
}

func hasProjectAssets(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "node_modules")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "web", "index.html")); err != nil {
		return false
	}
	return true
}

func detectAppMode(appRoot string, args []string) (appMode, error) {
	mode := appMode{
		title:     "esbuild -> webview (no server)",
		watchDir:  filepath.Join(appRoot, "web"),
		serveHost: "127.0.0.1",
	}
	var (
		entryArg       string
		inlineCode     string
		inlineExt      = ".jsx"
		loaderExplicit bool
		useStdin       bool
		portExplicit   bool
		hostExplicit   bool
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			mode.showHelp = true
		case "-v", "--version":
			mode.showVersion = true
		case "--register":
			mode.register = true
		case "--set-default-association":
			mode.setDefaultAssoc = true
		case "--serve":
			mode.serve = true
		case "--allow-remote":
			mode.allowRemote = true
		case "--host":
			if i+1 >= len(args) {
				return appMode{}, fmt.Errorf("--host requires a hostname or IP address")
			}
			mode.serveHost = args[i+1]
			hostExplicit = true
			i++
		case "--port":
			if i+1 >= len(args) {
				return appMode{}, fmt.Errorf("--port requires a numeric value")
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil || port < 0 || port > 65535 {
				return appMode{}, fmt.Errorf("invalid port %q", args[i+1])
			}
			mode.servePort = port
			portExplicit = true
			i++
		case "-e", "--eval":
			if i+1 >= len(args) {
				return appMode{}, fmt.Errorf("%s requires an inline source string", args[i])
			}
			inlineCode = args[i+1]
			i++
		case "--loader":
			if i+1 >= len(args) {
				return appMode{}, fmt.Errorf("--loader requires one of: js, jsx, ts, tsx")
			}
			ext, err := normalizeInlineExt(args[i+1])
			if err != nil {
				return appMode{}, err
			}
			inlineExt = ext
			loaderExplicit = true
			i++
		case "--stdin":
			useStdin = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return appMode{}, fmt.Errorf("unknown argument: %s", args[i])
			}
			if entryArg != "" {
				return appMode{}, fmt.Errorf("expected a single entry path or --eval source")
			}
			entryArg = args[i]
		}
	}

	if mode.showHelp || mode.showVersion || mode.register {
		if entryArg != "" || inlineCode != "" || useStdin || loaderExplicit || mode.serve || portExplicit || hostExplicit || mode.allowRemote {
			return appMode{}, fmt.Errorf("cannot combine %s with entry, eval, stdin, or loader flags", firstMetaFlag(mode))
		}
		if mode.setDefaultAssoc && !mode.register {
			return appMode{}, fmt.Errorf("--set-default-association requires --register")
		}
		return mode, nil
	}
	if mode.setDefaultAssoc {
		return appMode{}, fmt.Errorf("--set-default-association requires --register")
	}
	if (portExplicit || hostExplicit) && !mode.serve {
		return appMode{}, fmt.Errorf("--host and --port are only valid with --serve")
	}

	if inlineCode != "" {
		if entryArg != "" || useStdin {
			return appMode{}, fmt.Errorf("cannot use an entry path or --stdin together with --eval")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return appMode{}, err
		}
		inlineCode = normalizeInlineSource(inlineCode)
		return appMode{
			inlineSource:  inlineCode,
			inlineExt:     inlineExt,
			inlineBaseDir: cwd,
			contentRoot:   cwd,
			title:         "esbuild -> webview (inline)",
			serve:         mode.serve,
			serveHost:     mode.serveHost,
			servePort:     mode.servePort,
			allowRemote:   mode.allowRemote,
		}, nil
	}

	if useStdin || (entryArg == "" && stdinHasData()) {
		if entryArg != "" {
			return appMode{}, fmt.Errorf("cannot use an entry path together with --stdin")
		}
		if useStdin && !stdinHasData() {
			return appMode{}, fmt.Errorf("--stdin requires piped input")
		}
		sourceBytes, err := readAllStdin()
		if err != nil {
			return appMode{}, err
		}
		if strings.TrimSpace(string(sourceBytes)) == "" {
			return appMode{}, fmt.Errorf("stdin did not contain any source code")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return appMode{}, err
		}
		inlineSource := normalizeInlineSource(string(sourceBytes))
		return appMode{
			inlineSource:  inlineSource,
			inlineExt:     inlineExt,
			inlineBaseDir: cwd,
			contentRoot:   cwd,
			title:         "esbuild -> webview (stdin)",
			serve:         mode.serve,
			serveHost:     mode.serveHost,
			servePort:     mode.servePort,
			allowRemote:   mode.allowRemote,
		}, nil
	}

	if entryArg == "" {
		return mode, nil
	}
	if loaderExplicit {
		return appMode{}, fmt.Errorf("--loader is only valid with --eval or --stdin")
	}

	entryPath, err := filepath.Abs(entryArg)
	if err != nil {
		return appMode{}, err
	}
	info, err := os.Stat(entryPath)
	if err != nil {
		return appMode{}, err
	}
	if info.IsDir() {
		return appMode{}, fmt.Errorf("entry path must be a file: %s", entryPath)
	}

	switch strings.ToLower(filepath.Ext(entryPath)) {
	case ".js", ".jsx", ".ts", ".tsx":
	default:
		return appMode{}, fmt.Errorf("unsupported entry extension: %s", filepath.Ext(entryPath))
	}

	return appMode{
		entryPath:   entryPath,
		contentRoot: resolveContentRoot(entryPath),
		title:       fmt.Sprintf("esbuild -> webview (%s)", filepath.Base(entryPath)),
		watchDir:    filepath.Dir(entryPath),
		serve:       mode.serve,
		serveHost:   mode.serveHost,
		servePort:   mode.servePort,
		allowRemote: mode.allowRemote,
	}, nil
}

func firstMetaFlag(mode appMode) string {
	switch {
	case mode.showHelp:
		return "--help"
	case mode.showVersion:
		return "--version"
	case mode.register:
		return "--register"
	default:
		return "flag"
	}
}

func cliUsage() string {
	return `jsxx <entry-file>
jsxx --eval "<h1>Hello</h1>"
jsxx --eval "export default function App() { return <div>Hello</div> }"
jsxx --eval "export default function App() { return <div>Hello</div> }" --loader tsx
jsxx --allow-remote .\app.jsx
jsxx --serve .\app.jsx
jsxx --serve --allow-remote .\app.jsx
jsxx --serve --port 3000 .\app.jsx
echo "<h1>Hello</h1>" | jsxx --loader jsx
echo "export default function App() { return <div>Hello</div> }" | jsxx
jsxx --register
jsxx --register --set-default-association
jsxx --version
`
}

func resolveVersion(appRoot string) string {
	packagePath := filepath.Join(appRoot, "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return "dev"
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil || pkg.Version == "" {
		return "dev"
	}
	return pkg.Version
}

func runRegistration(appRoot string, setDefaultAssoc bool) error {
	scriptPath := filepath.Join(appRoot, "scripts", "register-jsx-window.ps1")
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("registration script not found: %s", scriptPath)
	}
	args := []string{
		"-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
	}
	if setDefaultAssoc {
		args = append(args, "-SetDefaultAssociation")
	}
	cmd := exec.Command("powershell", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func stdinHasData() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

func readAllStdin() ([]byte, error) {
	return io.ReadAll(os.Stdin)
}

func normalizeInlineExt(loader string) (string, error) {
	switch strings.ToLower(loader) {
	case "js", ".js":
		return ".js", nil
	case "jsx", ".jsx":
		return ".jsx", nil
	case "ts", ".ts":
		return ".ts", nil
	case "tsx", ".tsx":
		return ".tsx", nil
	default:
		return "", fmt.Errorf("unsupported loader %q (expected js, jsx, ts, or tsx)", loader)
	}
}

func normalizeInlineSource(source string) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return source
	}
	if strings.Contains(trimmed, "export default") {
		return source
	}
	if strings.HasPrefix(trimmed, "<") || strings.HasPrefix(trimmed, "(") {
		return "export default function App() {\n  return (\n" + source + "\n  );\n}\n"
	}
	return source
}

func buildHTML(appRoot string, mode appMode) (string, error) {
	idx, err := loadIndexHTML(appRoot, mode)
	if err != nil {
		return "", err
	}

	jsBundle, err := buildAppBundle(appRoot, mode)
	if err != nil {
		return "", err
	}

	return injectJS(idx, jsBundle), nil
}

func buildHostLoaderHTML(appRoot string) (string, error) {
	idx, err := loadIndexHTML(appRoot, appMode{})
	if err != nil {
		return "", err
	}
	return injectJS(idx, hostBundleLoaderJS), nil
}

func buildWindowLoaderHTML(appRoot string, mode appMode) (string, error) {
	idx, err := loadIndexHTML(appRoot, mode)
	if err != nil {
		return "", err
	}
	idx = injectBaseHref(idx, "https://assets.jsxw.local/")
	return injectJS(idx, windowBundleLoaderJS), nil
}

func buildServeLoaderHTML(appRoot string, mode appMode) (string, error) {
	idx, err := loadIndexHTML(appRoot, mode)
	if err != nil {
		return "", err
	}
	return injectJS(idx, serveBundleLoaderJS), nil
}

func buildAppBundle(appRoot string, mode appMode) (string, error) {
	var jsBundle string
	if mode.entryPath == "" && mode.inlineSource == "" {
		jsSrc, cssSrc, err := loadDefaultSources(appRoot)
		if err != nil {
			return "", err
		}
		jsBundle, err = bundleDefaultEntry(appRoot, jsSrc, cssSrc)
		if err != nil {
			return "", err
		}
	} else if mode.entryPath != "" {
		var err error
		jsBundle, err = bundleExternalEntry(appRoot, mode.entryPath)
		if err != nil {
			return "", err
		}
	} else {
		var err error
		jsBundle, err = bundleInlineEntry(appRoot, mode)
		if err != nil {
			return "", err
		}
	}

	return jsBundle, nil
}

func loadIndexHTML(appRoot string, mode appMode) (string, error) {
	b, err := os.ReadFile(filepath.Join(appRoot, "web", "index.html"))
	if err != nil {
		return applyContentSecurityPolicy(indexHTML, mode), nil
	}
	return applyContentSecurityPolicy(string(b), mode), nil
}

var cspMetaPattern = regexp.MustCompile(`(?is)<meta\b[^>]*http-equiv=["']Content-Security-Policy["'][^>]*>`)

func applyContentSecurityPolicy(html string, mode appMode) string {
	policy := strictContentSecurityPolicy
	if mode.allowRemote {
		policy = relaxedContentSecurityPolicy
	}

	metaTag := `<meta http-equiv="Content-Security-Policy" content="` + policy + `">`
	if cspMetaPattern.MatchString(html) {
		return cspMetaPattern.ReplaceAllString(html, metaTag)
	}
	if strings.Contains(html, "<head>") {
		return strings.Replace(html, "<head>", "<head>\n    "+metaTag, 1)
	}
	return metaTag + html
}

const strictContentSecurityPolicy = `default-src 'self' 'unsafe-inline' data: blob:; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval' data: blob:; connect-src 'self' data: blob: https://assets.jsxw.local; img-src 'self' data: blob: https://assets.jsxw.local; media-src 'self' data: blob: https://assets.jsxw.local; font-src 'self' data: blob: https://assets.jsxw.local; worker-src 'self' data: blob: https://assets.jsxw.local;`

const relaxedContentSecurityPolicy = `default-src * 'unsafe-inline' data: blob:; script-src * 'unsafe-inline' 'wasm-unsafe-eval' data: blob:; style-src * 'unsafe-inline' data: blob:; connect-src * data: blob: ws: wss:; img-src * data: blob:; media-src * data: blob:; font-src * data: blob:; worker-src * data: blob:;`

// loadSources reads from disk for hot reloading
func loadDefaultSources(appRoot string) (js, css string, err error) {
	read := func(p string) (string, error) {
		b, e := os.ReadFile(p)
		return string(b), e
	}
	j, e := read(filepath.Join(appRoot, "web", "main.js"))
	if e != nil {
		j = mainJS
	}
	c, e := read(filepath.Join(appRoot, "web", "style.css"))
	if e != nil {
		c = styleCSS
	}
	return j, c, nil
}

// bundleInMemory bundles main.js and resolves ./style.css from an in-memory plugin.
// CSS is loaded with LoaderText, so it becomes an ESM string that main.js can inject.
func bundleDefaultEntry(appRoot, srcJS, srcCSS string) (string, error) {
	webDir := filepath.Join(appRoot, "web")

	result := api.Build(api.BuildOptions{
		LogLevel: api.LogLevelSilent,
		Bundle:   true,
		Write:    false,
		Outfile:  "app.js",
		Platform: api.PlatformBrowser,
		Target:   api.ES2020,

		// IMPORTANT: point to real dirs so esbuild can find node_modules
		AbsWorkingDir: appRoot,
		Stdin: &api.StdinOptions{
			Contents:   srcJS,
			Sourcefile: filepath.Join(webDir, "main.js"), // pretend this lives on disk here
			ResolveDir: webDir,
			Loader:     api.LoaderJSX,
		},

		Loader: map[string]api.Loader{
			".js":  api.LoaderJSX,
			".jsx": api.LoaderJSX,
			".ts":  api.LoaderTS,
			".tsx": api.LoaderTSX, // if you switch to TSX later
			".css": api.LoaderText,
		},

		// Nice-to-haves for React and many libs
		Define: map[string]string{
			"process.env.NODE_ENV": `"development"`, // or `"production"` for prod
			"global":               "window",        // some libs expect global
		},

		// If you plan to write JSX without importing React explicitly
		JSX:             api.JSXAutomatic,
		JSXImportSource: "react",

		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		Charset:           api.CharsetUTF8,

		Plugins: []api.Plugin{
			{
				Name: "vfs-css",
				Setup: func(p api.PluginBuild) {
					p.OnResolve(api.OnResolveOptions{Filter: `^\./style\.css$`},
						func(args api.OnResolveArgs) (api.OnResolveResult, error) {
							return api.OnResolveResult{Path: "/app/style.css", Namespace: "vfs"}, nil
						},
					)
					p.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "vfs"},
						func(args api.OnLoadArgs) (api.OnLoadResult, error) {
							if args.Path == "/app/style.css" {
								return api.OnLoadResult{Contents: &srcCSS, Loader: api.LoaderText}, nil
							}
							return api.OnLoadResult{}, fmt.Errorf("unhandled path: %s", args.Path)
						},
					)
				},
			},
		},
	})

	if len(result.Errors) > 0 {
		return "", fmt.Errorf("esbuild: %s", result.Errors[0].Text)
	}

	var outJS string
	for _, f := range result.OutputFiles {
		if strings.HasSuffix(f.Path, ".js") {
			outJS = string(f.Contents)
			break
		}
	}
	if outJS == "" {
		return "", fmt.Errorf("esbuild: no JS output (check loaders/entry)")
	}
	return outJS, nil
}

func bundleExternalEntry(appRoot, entryPath string) (string, error) {
	entryDir := filepath.Dir(entryPath)
	entryFile := "./" + filepath.Base(entryPath)
	wrapper := makeExternalEntryWrapper(entryFile)

	result := api.Build(api.BuildOptions{
		LogLevel: api.LogLevelSilent,
		Bundle:   true,
		Write:    false,
		Outfile:  "app.js",
		Platform: api.PlatformBrowser,
		Target:   api.ES2020,

		AbsWorkingDir: appRoot,
		NodePaths:     []string{filepath.Join(appRoot, "node_modules")},
		Stdin: &api.StdinOptions{
			Contents:   wrapper,
			Sourcefile: filepath.Join(entryDir, "__codex_entry__.jsx"),
			ResolveDir: entryDir,
			Loader:     api.LoaderJSX,
		},

		Loader: map[string]api.Loader{
			".js":   api.LoaderJSX,
			".jsx":  api.LoaderJSX,
			".ts":   api.LoaderTS,
			".tsx":  api.LoaderTSX,
			".css":  api.LoaderText,
			".json": api.LoaderJSON,
		},

		Define: map[string]string{
			"process.env.NODE_ENV": `"development"`,
			"global":               "window",
		},

		JSX:             api.JSXAutomatic,
		JSXImportSource: "react",

		MinifyWhitespace:  false,
		MinifyIdentifiers: false,
		MinifySyntax:      false,
		Charset:           api.CharsetUTF8,

		Plugins: []api.Plugin{
			makeUnicodeSourcePlugin(),
			makeReactPersistPlugin(appRoot, entryDir),
		},
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("esbuild: %s", result.Errors[0].Text)
	}

	var outJS string
	for _, f := range result.OutputFiles {
		if strings.HasSuffix(f.Path, ".js") {
			outJS = string(f.Contents)
			break
		}
	}
	if outJS == "" {
		return "", fmt.Errorf("esbuild: no JS output (check loaders/entry)")
	}
	return outJS, nil
}

func bundleInlineEntry(appRoot string, mode appMode) (string, error) {
	entryDir := mode.inlineBaseDir
	entryFile := "codex-inline-entry"
	wrapper := makeExternalEntryWrapper(entryFile)
	inlinePath := filepath.Join(entryDir, "__codex_inline__"+mode.inlineExt)

	result := api.Build(api.BuildOptions{
		LogLevel: api.LogLevelSilent,
		Bundle:   true,
		Write:    false,
		Outfile:  "app.js",
		Platform: api.PlatformBrowser,
		Target:   api.ES2020,

		AbsWorkingDir: appRoot,
		NodePaths:     []string{filepath.Join(appRoot, "node_modules")},
		Stdin: &api.StdinOptions{
			Contents:   wrapper,
			Sourcefile: filepath.Join(entryDir, "__codex_entry__.jsx"),
			ResolveDir: entryDir,
			Loader:     api.LoaderJSX,
		},

		Loader: map[string]api.Loader{
			".js":   api.LoaderJSX,
			".jsx":  api.LoaderJSX,
			".ts":   api.LoaderTS,
			".tsx":  api.LoaderTSX,
			".css":  api.LoaderText,
			".json": api.LoaderJSON,
		},

		Define: map[string]string{
			"process.env.NODE_ENV": `"development"`,
			"global":               "window",
		},

		JSX:             api.JSXAutomatic,
		JSXImportSource: "react",

		MinifyWhitespace:  false,
		MinifyIdentifiers: false,
		MinifySyntax:      false,
		Charset:           api.CharsetUTF8,

		Plugins: []api.Plugin{
			makeInlineSourcePlugin(mode.inlineSource, inlinePath),
			makeReactPersistPlugin(appRoot, entryDir),
		},
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("esbuild: %s", result.Errors[0].Text)
	}

	var outJS string
	for _, f := range result.OutputFiles {
		if strings.HasSuffix(f.Path, ".js") {
			outJS = string(f.Contents)
			break
		}
	}
	if outJS == "" {
		return "", fmt.Errorf("esbuild: no JS output (check loaders/entry)")
	}
	return outJS, nil
}

func makeExternalEntryWrapper(entryFile string) string {
	return fmt.Sprintf(
		`import { createRoot } from "react-dom/client";
import { __codexBootstrap } from "codex-react-persist";
import App from %q;

const host = window;
const container = document.getElementById("app");
if (!container) {
  throw new Error("Missing #app root");
}

__codexBootstrap().finally(() => {
  if (host.__codexReactRoot) {
    try {
      host.__codexReactRoot.unmount();
    } catch {}
  }

  host.__codexReactRoot = createRoot(container);
  host.__codexReactRoot.render(<App />);
});
`, entryFile)
}

func makeInlineSourcePlugin(source, sourcePath string) api.Plugin {
	return api.Plugin{
		Name: "codex-inline-source",
		Setup: func(p api.PluginBuild) {
			p.OnResolve(api.OnResolveOptions{Filter: `^codex-inline-entry$`},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					return api.OnResolveResult{Path: sourcePath, Namespace: "codex-inline-source"}, nil
				},
			)

			p.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "codex-inline-source"},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					return api.OnLoadResult{
						Contents:   &source,
						Loader:     loaderForScriptPath(args.Path),
						ResolveDir: filepath.Dir(args.Path),
					}, nil
				},
			)
		},
	}
}

func makeReactPersistPlugin(appRoot, entryDir string) api.Plugin {
	return api.Plugin{
		Name: "codex-react-persist",
		Setup: func(p api.PluginBuild) {
			p.OnResolve(api.OnResolveOptions{Filter: `^codex-react-persist$`},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					return api.OnResolveResult{Path: "codex-react-persist", Namespace: "codex-react-persist"}, nil
				},
			)

			p.OnResolve(api.OnResolveOptions{Filter: `^react$`},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					importer := filepath.ToSlash(args.Importer)
					if strings.Contains(importer, "/node_modules/") {
						return api.OnResolveResult{}, nil
					}
					return api.OnResolveResult{Path: "codex-react-persist", Namespace: "codex-react-persist"}, nil
				},
			)

			p.OnResolve(api.OnResolveOptions{Filter: `^react-original$`, Namespace: "codex-react-persist"},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					res := p.Resolve("react", api.ResolveOptions{
						Importer:   filepath.Join(appRoot, "node_modules", "__codex_react_persist__.js"),
						ResolveDir: filepath.Join(appRoot, "node_modules"),
						Kind:       api.ResolveJSImportStatement,
					})
					if len(res.Errors) > 0 {
						return api.OnResolveResult{}, fmt.Errorf("resolve react: %s", res.Errors[0].Text)
					}
					return api.OnResolveResult{
						Path:      res.Path,
						External:  res.External,
						Namespace: res.Namespace,
						Suffix:    res.Suffix,
					}, nil
				},
			)

			p.OnLoad(api.OnLoadOptions{Filter: `^codex-react-persist$`, Namespace: "codex-react-persist"},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					contents := `import ReactOriginal from "react-original";

const React = ReactOriginal;
const codexOriginalUseStateImpl = ReactOriginal.useState;
const codexOriginalUseReducerImpl = ReactOriginal.useReducer;
const codexOriginalUseRefImpl = ReactOriginal.useRef;
const codexOriginalUseEffectImpl = ReactOriginal.useEffect;

const host = typeof window !== "undefined" ? window : globalThis;
const undefinedMarker = { __codexUndefined: true };
let stateCache = {};
let bootstrapped = false;
let bootstrapPromise = null;
let nextHookId = 0;

function cloneForStorage(value) {
  if (value === undefined) {
    return undefinedMarker;
  }
  return value;
}

function unwrapStoredValue(value) {
  if (value && typeof value === "object" && value.__codexUndefined) {
    return undefined;
  }
  return value;
}

function serializeStateCache() {
  try {
    return JSON.stringify(stateCache);
  } catch {
    return "{}";
  }
}

function syncStateToHost() {
  if (typeof host.__codexSetReloadState !== "function") {
    return;
  }
  host.__codexSetReloadState(serializeStateCache()).catch(() => {});
}

function parseBootState(raw) {
  if (!raw || typeof raw !== "string") {
    return {};
  }
  try {
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

export function __codexBootstrap() {
  if (bootstrapped) {
    return Promise.resolve();
  }
  if (!bootstrapPromise) {
    bootstrapPromise = Promise.resolve(
      typeof host.__codexGetReloadState === "function" ? host.__codexGetReloadState() : "{}"
    )
      .then((raw) => {
        stateCache = parseBootState(raw);
        bootstrapped = true;
      })
      .catch(() => {
        stateCache = {};
        bootstrapped = true;
      });
  }
  return bootstrapPromise;
}

function usePersistentHookKey() {
  const keyRef = codexOriginalUseRefImpl(null);
  if (keyRef.current === null) {
    keyRef.current = String(nextHookId++);
  }
  return keyRef.current;
}

function resolveInitialValue(initialValue) {
  return typeof initialValue === "function" ? initialValue() : initialValue;
}

function persistedUseState(initialValue) {
  const hookKey = usePersistentHookKey();
  const [state, setState] = codexOriginalUseStateImpl(() => {
    if (Object.prototype.hasOwnProperty.call(stateCache, hookKey)) {
      return unwrapStoredValue(stateCache[hookKey]);
    }
    return resolveInitialValue(initialValue);
  });

  codexOriginalUseEffectImpl(() => {
    stateCache[hookKey] = cloneForStorage(state);
    syncStateToHost();
  }, [hookKey, state]);

  function persistedSetState(update) {
    setState((previous) => {
      const nextValue = typeof update === "function" ? update(previous) : update;
      stateCache[hookKey] = cloneForStorage(nextValue);
      syncStateToHost();
      return nextValue;
    });
  }

  return [state, persistedSetState];
}

function persistedUseReducer(reducer, initialArg, init) {
  const hookKey = usePersistentHookKey();
  const [state, dispatch] = codexOriginalUseReducerImpl(
    reducer,
    initialArg,
    (arg) => {
      if (Object.prototype.hasOwnProperty.call(stateCache, hookKey)) {
        return unwrapStoredValue(stateCache[hookKey]);
      }
      return typeof init === "function" ? init(arg) : arg;
    }
  );

  codexOriginalUseEffectImpl(() => {
    stateCache[hookKey] = cloneForStorage(state);
    syncStateToHost();
  }, [hookKey, state]);

  return [state, dispatch];
}

host.__codexFlushReloadState = syncStateToHost;
if (typeof window !== "undefined" && !host.__codexReloadStateUnloadHookInstalled) {
  host.__codexReloadStateUnloadHookInstalled = true;
  window.addEventListener("beforeunload", syncStateToHost);
}

export const Children = React.Children;
export const Component = React.Component;
export const Fragment = React.Fragment;
export const Profiler = React.Profiler;
export const PureComponent = React.PureComponent;
export const StrictMode = React.StrictMode;
export const Suspense = React.Suspense;
export const __CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE = React.__CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE;
export const __COMPILER_RUNTIME = React.__COMPILER_RUNTIME;
export const act = React.act;
export const cache = React.cache;
export const captureOwnerStack = React.captureOwnerStack;
export const cloneElement = React.cloneElement;
export const createContext = React.createContext;
export const createElement = React.createElement;
export const createRef = React.createRef;
export const forwardRef = React.forwardRef;
export const isValidElement = React.isValidElement;
export const lazy = React.lazy;
export const memo = React.memo;
export const startTransition = React.startTransition;
export const unstable_useCacheRefresh = React.unstable_useCacheRefresh;
export const use = React.use;
export const useActionState = React.useActionState;
export const useCallback = React.useCallback;
export const useContext = React.useContext;
export const useDebugValue = React.useDebugValue;
export const useDeferredValue = React.useDeferredValue;
export const useEffect = React.useEffect;
export const useId = React.useId;
export const useImperativeHandle = React.useImperativeHandle;
export const useInsertionEffect = React.useInsertionEffect;
export const useLayoutEffect = React.useLayoutEffect;
export const useMemo = React.useMemo;
export const useOptimistic = React.useOptimistic;
export const useRef = React.useRef;
export const useState = persistedUseState;
export const useSyncExternalStore = React.useSyncExternalStore;
export const useTransition = React.useTransition;
export const useReducer = persistedUseReducer;
export const version = React.version;
const PersistedReact = { ...React, useState: persistedUseState, useReducer: persistedUseReducer };
export default PersistedReact;`

					return api.OnLoadResult{
						Contents:   &contents,
						Loader:     api.LoaderJS,
						ResolveDir: entryDir,
					}, nil
				},
			)
		},
	}
}

func makeUnicodeSourcePlugin() api.Plugin {
	return api.Plugin{
		Name: "codex-unicode-source",
		Setup: func(p api.PluginBuild) {
			p.OnLoad(api.OnLoadOptions{Filter: `\.(js|jsx|ts|tsx)$`},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					sourcePath := filepath.ToSlash(args.Path)
					if strings.Contains(sourcePath, "/node_modules/") {
						return api.OnLoadResult{}, nil
					}

					b, err := os.ReadFile(args.Path)
					if err != nil {
						return api.OnLoadResult{}, err
					}
					contents := decodeUnicodeEscapes(string(b))

					return api.OnLoadResult{
						Contents:   &contents,
						Loader:     loaderForScriptPath(args.Path),
						ResolveDir: filepath.Dir(args.Path),
					}, nil
				},
			)
		},
	}
}

func loaderForScriptPath(path string) api.Loader {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	default:
		return api.LoaderJS
	}
}

func decodeUnicodeEscapes(input string) string {
	var out strings.Builder
	out.Grow(len(input))

	for i := 0; i < len(input); {
		if input[i] == '\\' && i+5 < len(input) && input[i+1] == 'u' {
			unit, ok := parseHexUint16(input[i+2 : i+6])
			if ok {
				if utf16.IsSurrogate(rune(unit)) && i+11 < len(input) && input[i+6] == '\\' && input[i+7] == 'u' {
					nextUnit, nextOK := parseHexUint16(input[i+8 : i+12])
					if nextOK {
						decoded := utf16.DecodeRune(rune(unit), rune(nextUnit))
						if decoded != unicodeReplacementRune {
							out.WriteRune(decoded)
							i += 12
							continue
						}
					}
				}

				out.WriteRune(rune(unit))
				i += 6
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(input[i:])
		out.WriteRune(r)
		i += size
	}

	return out.String()
}

const unicodeReplacementRune = '\uFFFD'

func parseHexUint16(s string) (uint16, bool) {
	if len(s) != 4 {
		return 0, false
	}

	var value uint16
	for _, ch := range s {
		value <<= 4
		switch {
		case ch >= '0' && ch <= '9':
			value |= uint16(ch - '0')
		case ch >= 'a' && ch <= 'f':
			value |= uint16(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			value |= uint16(ch-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func injectJS(html, js string) string {
	const marker = "<!--JS-->"
	tag := `<script type="module">` + js + `</script>`
	if strings.Contains(html, marker) {
		return strings.Replace(html, marker, tag, 1)
	}
	// Fallback: append before </body>
	return strings.Replace(html, "</body>", tag+"</body>", 1)
}

func injectBaseHref(html, href string) string {
	baseTag := `<base href="` + href + `">`
	if strings.Contains(strings.ToLower(html), "<base ") {
		return html
	}
	if strings.Contains(html, "<head>") {
		return strings.Replace(html, "<head>", "<head>\n    "+baseTag, 1)
	}
	return html
}

func initAppBundleBridge(w webview_selector.WebView, store *appBundleStore) error {
	return w.Bind("__codexGetAppBundle", func() string {
		return store.get()
	})
}

const hostBundleLoaderJS = `(() => {
  let loadToken = 0;
  let previousBundleURL = null;

  async function executeBundle(bundle) {
    const bundleURL = URL.createObjectURL(new Blob([bundle], { type: "text/javascript" }));
    try {
      await import(bundleURL);
    } finally {
      if (previousBundleURL) {
        URL.revokeObjectURL(previousBundleURL);
      }
      previousBundleURL = bundleURL;
    }
  }

  async function loadBundleFromHost() {
    const token = ++loadToken;

    if (typeof window.__codexFlushReloadState === "function") {
      try {
        await window.__codexFlushReloadState();
      } catch {}
    }

    if (typeof window.__codexGetAppBundle !== "function") {
      return;
    }

    const bundle = await window.__codexGetAppBundle();
    if (token !== loadToken || !bundle) {
      return;
    }

    try {
      await executeBundle(bundle);
    } catch (error) {
      console.error("Failed to evaluate host bundle", error);
    }
  }

  window.__codexReloadFromHost = loadBundleFromHost;

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => {
      loadBundleFromHost().catch((error) => {
        console.error("Failed to load host bundle", error);
      });
    }, { once: true });
  } else {
    loadBundleFromHost().catch((error) => {
      console.error("Failed to load host bundle", error);
    });
  }
})();`

const windowBundleLoaderJS = `(() => {
  let loadToken = 0;
  let previousBundleURL = null;

  async function executeBundle(bundle) {
    const bundleURL = URL.createObjectURL(new Blob([bundle], { type: "text/javascript" }));
    try {
      await import(bundleURL);
    } finally {
      if (previousBundleURL) {
        URL.revokeObjectURL(previousBundleURL);
      }
      previousBundleURL = bundleURL;
    }
  }

  async function loadBundleFromHost() {
    const token = ++loadToken;

    if (typeof window.__codexFlushReloadState === "function") {
      try {
        await window.__codexFlushReloadState();
      } catch {}
    }

    const response = await fetch("https://app.jsxw.local/__jsxw/bundle.js?ts=" + Date.now(), {
      cache: "no-store",
      credentials: "same-origin",
    });
    if (!response.ok) {
      throw new Error("Failed to fetch bundle: " + response.status);
    }

    const bundle = await response.text();
    if (token !== loadToken || !bundle) {
      return;
    }

    try {
      await executeBundle(bundle);
    } catch (error) {
      console.error("Failed to evaluate window bundle", error);
    }
  }

  window.__codexReloadFromHost = loadBundleFromHost;

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => {
      loadBundleFromHost().catch((error) => {
        console.error("Failed to load window bundle", error);
      });
    }, { once: true });
  } else {
    loadBundleFromHost().catch((error) => {
      console.error("Failed to load window bundle", error);
    });
  }
})();`

const serveBundleLoaderJS = `(() => {
  let loadToken = 0;
  let previousBundleURL = null;

  async function executeBundle(bundle) {
    const bundleURL = URL.createObjectURL(new Blob([bundle], { type: "text/javascript" }));
    try {
      await import(bundleURL);
    } finally {
      if (previousBundleURL) {
        URL.revokeObjectURL(previousBundleURL);
      }
      previousBundleURL = bundleURL;
    }
  }

  async function loadBundleFromHost() {
    const token = ++loadToken;
    const response = await fetch("/__jsxw/bundle.js?ts=" + Date.now(), {
      cache: "no-store",
      credentials: "same-origin",
    });
    if (!response.ok) {
      throw new Error("Failed to fetch bundle: " + response.status);
    }

    const bundle = await response.text();
    if (token !== loadToken || !bundle) {
      return;
    }

    try {
      await executeBundle(bundle);
    } catch (error) {
      console.error("Failed to evaluate served bundle", error);
    }
  }

  window.__codexReloadFromHost = loadBundleFromHost;

  if (typeof EventSource === "function") {
    const events = new EventSource("/__jsxw/events");
    events.onmessage = () => {
      loadBundleFromHost().catch((error) => {
        console.error("Failed to reload served bundle", error);
      });
    };
    events.onerror = () => {};
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => {
      loadBundleFromHost().catch((error) => {
        console.error("Failed to load served bundle", error);
      });
    }, { once: true });
  } else {
    loadBundleFromHost().catch((error) => {
      console.error("Failed to load served bundle", error);
    });
  }
})();`

type serveReloadBroker struct {
	mu        sync.Mutex
	listeners map[chan struct{}]struct{}
}

func newServeReloadBroker() *serveReloadBroker {
	return &serveReloadBroker{
		listeners: make(map[chan struct{}]struct{}),
	}
}

func (b *serveReloadBroker) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.listeners[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *serveReloadBroker) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	if _, ok := b.listeners[ch]; ok {
		delete(b.listeners, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *serveReloadBroker) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.listeners {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func runServeMode(appRoot string, mode appMode) error {
	debugServef("serve: content root %q", mode.contentRoot)
	debugServef("serve: building initial bundle")
	initialBundle, err := buildAppBundle(appRoot, mode)
	if err != nil {
		return err
	}
	debugServef("serve: building loader html")
	indexHTML, err := buildServeLoaderHTML(appRoot, mode)
	if err != nil {
		return err
	}

	bundleStore := newAppBundleStore(initialBundle)
	reloads := newServeReloadBroker()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/" {
			if tryServeStaticAsset(w, r, mode.contentRoot) {
				return
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, indexHTML)
	})
	mux.HandleFunc("/__jsxw/bundle.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, bundleStore.get())
	})
	mux.HandleFunc("/__jsxw/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")

		ch := reloads.subscribe()
		defer reloads.unsubscribe(ch)

		_, _ = io.WriteString(w, ": connected\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				_, _ = io.WriteString(w, "data: reload\n\n")
				flusher.Flush()
			}
		}
	})

	debugServef("serve: creating listener on %s:%d", mode.serveHost, mode.servePort)
	listener, err := net.Listen("tcp", net.JoinHostPort(mode.serveHost, strconv.Itoa(mode.servePort)))
	if err != nil {
		return err
	}
	defer listener.Close()
	debugServef("serve: listener ready on %s", listener.Addr().String())

	if mode.watchDir != "" {
		go func() {
			if err := watchAndReload(mode.watchDir, func() {
				bundle, err := buildAppBundle(appRoot, mode)
				if err != nil {
					log.Println("reload bundle:", err)
					return
				}
				bundleStore.set(bundle)
				reloads.notify()
			}); err != nil {
				log.Println("watch:", err)
			}
		}()
	}

	url := "http://" + listener.Addr().String()
	fmt.Println(url)
	log.Printf("Serving %s", url)

	server := &http.Server{Handler: mux}
	return server.Serve(listener)
}

func resolveContentRoot(entryPath string) string {
	dir := filepath.Dir(entryPath)
	for current := dir; ; current = filepath.Dir(current) {
		if hasContentRootMarker(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if filepath.Base(dir) == "src" {
		return filepath.Dir(dir)
	}
	return dir
}

func hasContentRootMarker(dir string) bool {
	for _, name := range []string{".git", "package.json", "node_modules"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func tryServeStaticAsset(w http.ResponseWriter, r *http.Request, root string) bool {
	fullPath, ok := resolveStaticContentPath(root, r.URL.Path)
	if !ok {
		debugServef("serve asset: unresolved path %s (root=%q)", r.URL.Path, root)
		return false
	}
	debugServef("serve asset: serving %s -> %s", r.URL.Path, fullPath)
	http.ServeFile(w, r, fullPath)
	return true
}

func readStaticAsset(root, requestPath string) ([]byte, string, bool) {
	fullPath, ok := resolveStaticContentPath(root, requestPath)
	if !ok {
		return nil, "", false
	}
	body, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, "", false
	}
	return body, contentTypeForPath(fullPath, body), true
}

func resolveStaticContentPath(root, requestPath string) (string, bool) {
	if root == "" {
		return "", false
	}
	cleanPath := path.Clean("/" + requestPath)
	if cleanPath == "/" || strings.HasPrefix(cleanPath, "/__jsxw/") {
		return "", false
	}
	relativePath := strings.TrimPrefix(cleanPath, "/")
	fullPath := filepath.Join(root, filepath.FromSlash(relativePath))
	rel, err := filepath.Rel(root, fullPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		return "", false
	}
	return fullPath, true
}

func contentTypeForPath(fullPath string, body []byte) string {
	if extType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fullPath))); extType != "" {
		return extType
	}
	return http.DetectContentType(body)
}

func debugServef(format string, args ...any) {
	if os.Getenv("JSXX_DEBUG_SERVE") == "" {
		return
	}
	message := fmt.Sprintf(format, args...)
	log.Println(message)
	logPath := filepath.Join(os.TempDir(), "jsxx-serve-debug.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339Nano), message)
}

// watchAndReload watches a directory and triggers cb on debounced changes to .js/.css/.html.
func watchAndReload(dir string, cb func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// Watch the directory
	if err := watcher.Add(dir); err != nil {
		return err
	}

	// Debounce due to editor “save twice” behavior
	var (
		pending  bool
		timer    *time.Timer
		debounce = 120 * time.Millisecond
		trigger  = func() {
			pending = false
			cb()
		}
		reset = func() {
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, trigger)
		}
	)

	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Only react to files we care about
			if !hasInterestingExt(ev.Name) {
				continue
			}
			// React on write/create/rename/remove
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				if !pending {
					pending = true
				}
				reset()
			}
		case err := <-watcher.Errors:
			// Keep going, just log
			log.Println("watch error:", err)
		}
	}
}

func hasInterestingExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".jsx", ".ts", ".tsx", ".css", ".html":
		return true
	default:
		return false
	}
}
