//go:build !windows

package main

import (
	"fmt"

	webview_selector "github.com/jchv/go-webview-selector"
)

type windowContentHost struct {
	navigateURL  string
	updateBundle func(string) error
}

func initWindowContentHost(_ webview_selector.WebView, _ appMode, _ string, _ string) (*windowContentHost, error) {
	return nil, fmt.Errorf("window content host is only implemented on windows")
}
