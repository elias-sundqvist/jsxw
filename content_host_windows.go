//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"unsafe"

	webview_selector "github.com/jchv/go-webview-selector"
	"github.com/jchv/go-webview2/pkg/edge"
)

const (
	windowContentHostName = "app.jsxw.local"
	windowAssetsHostName  = "assets.jsxw.local"
)

type windowContentHost struct {
	navigateURL  string
	updateBundle func(string) error
}

func initWindowContentHost(w webview_selector.WebView, mode appMode, indexHTML string, initialBundle string) (*windowContentHost, error) {
	debugContentHostf("init start contentRoot=%q", mode.contentRoot)
	chromium, err := getChromium(w)
	if err != nil {
		debugContentHostf("init getChromium error: %v", err)
		return nil, err
	}
	debugContentHostf("init got chromium")

	wv3 := chromium.GetICoreWebView2_3()
	if wv3 == nil {
		return nil, fmt.Errorf("webview2 virtual host mapping unavailable")
	}

	bootstrapDir, bundlePath, err := writeWindowBootstrapDir(indexHTML, initialBundle)
	if err != nil {
		return nil, err
	}
	if err := wv3.SetVirtualHostNameToFolderMapping(
		windowContentHostName,
		bootstrapDir,
		edge.COREWEBVIEW2_HOST_RESOURCE_ACCESS_KIND_ALLOW,
	); err != nil {
		return nil, err
	}
	debugContentHostf("init mapped host %s to %s", windowContentHostName, bootstrapDir)

	if mode.contentRoot != "" {
		if err := wv3.SetVirtualHostNameToFolderMapping(
			windowAssetsHostName,
			mode.contentRoot,
			edge.COREWEBVIEW2_HOST_RESOURCE_ACCESS_KIND_ALLOW,
		); err != nil {
			debugContentHostf("init SetVirtualHostNameToFolderMapping error: %v", err)
		} else {
			debugContentHostf("init mapped host %s to %s", windowAssetsHostName, mode.contentRoot)
		}
	}

	debugContentHostf("init complete")
	return &windowContentHost{
		navigateURL: "https://" + windowContentHostName + "/index.html",
		updateBundle: func(bundle string) error {
			return os.WriteFile(bundlePath, []byte(bundle), 0o644)
		},
	}, nil
}

func writeWindowBootstrapDir(indexHTML, bundle string) (string, string, error) {
	dir, err := os.MkdirTemp("", "jsxw-host-*")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		return "", "", err
	}
	jsxwDir := filepath.Join(dir, "__jsxw")
	if err := os.MkdirAll(jsxwDir, 0o755); err != nil {
		return "", "", err
	}
	bundlePath := filepath.Join(jsxwDir, "bundle.js")
	if err := os.WriteFile(bundlePath, []byte(bundle), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "favicon.ico"), nil, 0o644); err != nil {
		return "", "", err
	}
	return dir, bundlePath, nil
}

func getChromium(w webview_selector.WebView) (*edge.Chromium, error) {
	value := reflect.ValueOf(w)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		return nil, fmt.Errorf("unexpected webview type %T", w)
	}
	elem := value.Elem()
	browserField := elem.FieldByName("browser")
	if !browserField.IsValid() {
		return nil, fmt.Errorf("webview browser field not found")
	}
	browserValue := reflect.NewAt(browserField.Type(), unsafe.Pointer(browserField.UnsafeAddr())).Elem().Interface()
	chromium, ok := browserValue.(*edge.Chromium)
	if !ok || chromium == nil {
		return nil, fmt.Errorf("unexpected browser implementation %T", browserValue)
	}
	return chromium, nil
}

func debugContentHostf(format string, args ...any) {
	if os.Getenv("JSXX_DEBUG_CONTENT") == "" {
		return
	}
	f, err := os.OpenFile("E:\\wgo\\content-host-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, format+"\n", args...)
}
