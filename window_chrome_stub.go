//go:build !windows

package main

import webview_selector "github.com/jchv/go-webview-selector"

func initWindowChrome(w webview_selector.WebView) error {
	return nil
}
