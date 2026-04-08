//go:build windows

package main

import (
	"fmt"
	"unsafe"

	webview_selector "github.com/jchv/go-webview-selector"
	"golang.org/x/sys/windows"
)

const (
	dwmwaUseImmersiveDarkMode = 20
	dwmwaBorderColor          = 34
	dwmwaCaptionColor         = 35
	dwmwaTextColor            = 36
	dwmColorNone              = 0xFFFFFFFE

	wsExDlgModalFrame       = 0x00000001
	wsOverlappedWindow      = 0x00CF0000
	wmSetIcon               = 0x0080
	iconSmall               = 0
	iconBig                 = 1
	monitorDefaultToNearest = 0x00000002
	swpNoSize               = 0x0001
	swpNoMove               = 0x0002
	swpNoZOrder             = 0x0004
	swpNoActivate           = 0x0010
	swpFrameChanged         = 0x0020
)

const gwlStyle = ^uintptr(15)
const gwlExstyle = ^uintptr(19)
const gclpHIcon = ^uintptr(13)
const gclpHIconSm = ^uintptr(33)

var (
	dwmapiDLL                 = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapiDLL.NewProc("DwmSetWindowAttribute")
	user32DLL                 = windows.NewLazySystemDLL("user32.dll")
	procCreateIcon            = user32DLL.NewProc("CreateIcon")
	procGetWindowLongPtrW     = user32DLL.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW     = user32DLL.NewProc("SetWindowLongPtrW")
	procGetWindowPlacement    = user32DLL.NewProc("GetWindowPlacement")
	procSetWindowPlacement    = user32DLL.NewProc("SetWindowPlacement")
	procMonitorFromWindow     = user32DLL.NewProc("MonitorFromWindow")
	procGetMonitorInfoW       = user32DLL.NewProc("GetMonitorInfoW")
	procSetClassLongPtrW      = user32DLL.NewProc("SetClassLongPtrW")
	procSendMessageW          = user32DLL.NewProc("SendMessageW")
	procSetWindowPos          = user32DLL.NewProc("SetWindowPos")
	blankIconHandle           uintptr
)

type rgbColor struct {
	R int `json:"r"`
	G int `json:"g"`
	B int `json:"b"`
}

type windowChromeTheme struct {
	Caption *rgbColor `json:"caption"`
	Border  *rgbColor `json:"border"`
	Text    *rgbColor `json:"text"`
}

type winPoint struct {
	X int32
	Y int32
}

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type windowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    winPoint
	PtMaxPosition    winPoint
	RcNormalPosition winRect
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor winRect
	RcWork    winRect
	DwFlags   uint32
}

type windowFullscreenState struct {
	enabled   bool
	style     uintptr
	exStyle   uintptr
	placement windowPlacement
	theme     windowChromeTheme
}

func initWindowChrome(w webview_selector.WebView) error {
	hwnd := uintptr(w.Window())
	if hwnd == 0 {
		return fmt.Errorf("missing native window handle")
	}
	fullscreen := &windowFullscreenState{}

	// Keep the native frame, but remove the caption text and icon for a cleaner look.
	w.SetTitle("")
	_ = hideWindowCaptionIcon(hwnd)

	// Start from a neutral dark frame so the initial white caption flash is minimized.
	fullscreen.theme = windowChromeTheme{
		Caption: &rgbColor{R: 10, G: 14, B: 23},
		Border:  &rgbColor{R: 30, G: 41, B: 59},
		Text:    &rgbColor{R: 226, G: 232, B: 240},
	}
	_ = applyWindowChromeTheme(hwnd, fullscreen.theme)

	if err := w.Bind("__codexSetWindowChrome", func(theme windowChromeTheme) error {
		fullscreen.theme = theme
		return applyWindowChromeTheme(hwnd, theme)
	}); err != nil {
		return err
	}

	if err := w.Bind("__codexSetNativeFullscreen", func(enabled bool) error {
		return setWindowFullscreen(hwnd, fullscreen, enabled)
	}); err != nil {
		return err
	}

	w.Init(windowChromeBridgeJS)
	return nil
}

func setWindowFullscreen(hwnd uintptr, state *windowFullscreenState, enabled bool) error {
	if err := user32DLL.Load(); err != nil {
		return err
	}

	if enabled {
		if state.enabled {
			return nil
		}

		style, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle))
		exStyle, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlExstyle))
		placement := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
		r1, _, getPlacementErr := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&placement)))
		if r1 == 0 {
			if getPlacementErr != windows.ERROR_SUCCESS && getPlacementErr != nil {
				return getPlacementErr
			}
			return fmt.Errorf("GetWindowPlacement failed")
		}

		monitor, _, _ := procMonitorFromWindow.Call(hwnd, monitorDefaultToNearest)
		if monitor == 0 {
			return fmt.Errorf("MonitorFromWindow failed")
		}

		info := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		r1, _, getMonitorErr := procGetMonitorInfoW.Call(monitor, uintptr(unsafe.Pointer(&info)))
		if r1 == 0 {
			if getMonitorErr != windows.ERROR_SUCCESS && getMonitorErr != nil {
				return getMonitorErr
			}
			return fmt.Errorf("GetMonitorInfoW failed")
		}

		state.style = style
		state.exStyle = exStyle
		state.placement = placement
		state.enabled = true

		procSetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle), style&^uintptr(wsOverlappedWindow))
		procSetWindowLongPtrW.Call(hwnd, uintptr(gwlExstyle), exStyle&^uintptr(wsExDlgModalFrame))
		borderNone := uint32(dwmColorNone)
		_ = dwmSetWindowAttribute(hwnd, dwmwaBorderColor, unsafe.Pointer(&borderNone), uint32(unsafe.Sizeof(borderNone)))
		procSetWindowPos.Call(
			hwnd,
			0,
			uintptr(info.RcMonitor.Left),
			uintptr(info.RcMonitor.Top),
			uintptr(info.RcMonitor.Right-info.RcMonitor.Left),
			uintptr(info.RcMonitor.Bottom-info.RcMonitor.Top),
			swpNoZOrder|swpNoActivate|swpFrameChanged,
		)
		return nil
	}

	if !state.enabled {
		return nil
	}

	procSetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle), state.style)
	procSetWindowLongPtrW.Call(hwnd, uintptr(gwlExstyle), state.exStyle)
	if state.theme.Caption != nil {
		_ = applyWindowChromeTheme(hwnd, state.theme)
	}
	state.placement.Length = uint32(unsafe.Sizeof(windowPlacement{}))
	procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&state.placement)))
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
	state.enabled = false
	return nil
}

func applyWindowChromeTheme(hwnd uintptr, theme windowChromeTheme) error {
	if theme.Caption == nil {
		return nil
	}

	darkMode := int32(0)
	if isDarkColor(*theme.Caption) {
		darkMode = 1
	}
	_ = dwmSetWindowAttribute(hwnd, dwmwaUseImmersiveDarkMode, unsafe.Pointer(&darkMode), uint32(unsafe.Sizeof(darkMode)))

	caption := toColorRef(*theme.Caption)
	if err := dwmSetWindowAttribute(hwnd, dwmwaCaptionColor, unsafe.Pointer(&caption), uint32(unsafe.Sizeof(caption))); err != nil {
		return err
	}

	if theme.Border != nil {
		border := toColorRef(*theme.Border)
		_ = dwmSetWindowAttribute(hwnd, dwmwaBorderColor, unsafe.Pointer(&border), uint32(unsafe.Sizeof(border)))
	}

	textColor := pickTextColor(*theme.Caption)
	if theme.Text != nil {
		textColor = *theme.Text
	}
	text := toColorRef(textColor)
	_ = dwmSetWindowAttribute(hwnd, dwmwaTextColor, unsafe.Pointer(&text), uint32(unsafe.Sizeof(text)))

	return nil
}

func dwmSetWindowAttribute(hwnd uintptr, attr uint32, value unsafe.Pointer, size uint32) error {
	if err := dwmapiDLL.Load(); err != nil {
		return err
	}
	r1, _, _ := procDwmSetWindowAttribute.Call(hwnd, uintptr(attr), uintptr(value), uintptr(size))
	if int32(r1) < 0 {
		return windows.Errno(r1)
	}
	return nil
}

func hideWindowCaptionIcon(hwnd uintptr) error {
	if err := user32DLL.Load(); err != nil {
		return err
	}

	exStyle, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlExstyle))
	newStyle := exStyle | uintptr(wsExDlgModalFrame)
	if newStyle != exStyle {
		procSetWindowLongPtrW.Call(hwnd, uintptr(gwlExstyle), newStyle)
	}

	icon := getBlankIconHandle()
	procSetClassLongPtrW.Call(hwnd, gclpHIcon, icon)
	procSetClassLongPtrW.Call(hwnd, gclpHIconSm, icon)
	procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, icon)
	procSendMessageW.Call(hwnd, wmSetIcon, iconBig, icon)
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
	return nil
}

func getBlankIconHandle() uintptr {
	if blankIconHandle != 0 {
		return blankIconHandle
	}

	// A fully transparent 16x16 monochrome icon prevents Windows from showing the
	// generic executable placeholder in the caption area.
	andMask := [32]byte{}
	for i := range andMask {
		andMask[i] = 0xff
	}
	xorMask := [32]byte{}

	h, _, _ := procCreateIcon.Call(
		0,
		16,
		16,
		1,
		1,
		uintptr(unsafe.Pointer(&andMask[0])),
		uintptr(unsafe.Pointer(&xorMask[0])),
	)
	blankIconHandle = h
	return blankIconHandle
}

func toColorRef(c rgbColor) uint32 {
	return uint32(c.R&0xff) | uint32(c.G&0xff)<<8 | uint32(c.B&0xff)<<16
}

func isDarkColor(c rgbColor) bool {
	return (299*c.R + 587*c.G + 114*c.B) < 140000
}

func pickTextColor(c rgbColor) rgbColor {
	if isDarkColor(c) {
		return rgbColor{R: 245, G: 245, B: 245}
	}
	return rgbColor{R: 20, G: 20, B: 20}
}

const windowChromeBridgeJS = `(function () {
  const bridgeName = "__codexSetWindowChrome";
  if (typeof window[bridgeName] !== "function") {
    return;
  }

  const fallback = { r: 10, g: 14, b: 23 };
  let lastSent = "";
  let scheduled = false;
  let fullscreenElement = null;
  let nativeFullscreen = false;

  function normalizeColor(input) {
    if (!input || typeof input !== "string") {
      return null;
    }

    const probe = document.createElement("div");
    probe.style.color = "";
    probe.style.color = input.trim();
    if (!probe.style.color) {
      return null;
    }

    (document.body || document.documentElement).appendChild(probe);
    const resolved = getComputedStyle(probe).color;
    probe.remove();

    const match = resolved.match(/rgba?\(\s*(\d+),\s*(\d+),\s*(\d+)(?:,\s*([.\d]+))?\s*\)/i);
    if (!match) {
      return null;
    }

    if (match[4] !== undefined && Number(match[4]) === 0) {
      return null;
    }

    return {
      r: Number(match[1]),
      g: Number(match[2]),
      b: Number(match[3]),
    };
  }

  function firstColor(values) {
    for (const value of values) {
      const color = normalizeColor(value);
      if (color) {
        return color;
      }
    }
    return null;
  }

  function pickTextColor(background) {
    const luminance = 299 * background.r + 587 * background.g + 114 * background.b;
    return luminance < 140000
      ? { r: 245, g: 245, b: 245 }
      : { r: 20, g: 20, b: 20 };
  }

  function mixColors(a, b, ratio) {
    const clamp = (value) => Math.max(0, Math.min(255, Math.round(value)));
    return {
      r: clamp(a.r + (b.r - a.r) * ratio),
      g: clamp(a.g + (b.g - a.g) * ratio),
      b: clamp(a.b + (b.b - a.b) * ratio),
    };
  }

  function colorToCSS(color) {
    return "rgb(" + color.r + ", " + color.g + ", " + color.b + ")";
  }

  function applyHostTheme(theme) {
    const root = document.documentElement;
    if (!root) {
      return;
    }

    const caption = theme.caption || fallback;
    const border = theme.border || caption;
    const text = theme.text || pickTextColor(caption);
    const track = mixColors(caption, { r: 0, g: 0, b: 0 }, 0.18);
    const thumb = mixColors(border, text, 0.18);
    const thumbHover = mixColors(thumb, text, 0.2);

    root.style.setProperty("--codex-host-bg", colorToCSS(caption));
    root.style.setProperty("--codex-host-border", colorToCSS(border));
    root.style.setProperty("--codex-host-text", colorToCSS(text));
    root.style.setProperty("--codex-scrollbar-track", colorToCSS(track));
    root.style.setProperty("--codex-scrollbar-thumb", colorToCSS(thumb));
    root.style.setProperty("--codex-scrollbar-thumb-hover", colorToCSS(thumbHover));
  }

  function computeTheme() {
    const root = document.documentElement;
    if (!root) {
      return {
        caption: fallback,
        border: fallback,
        text: pickTextColor(fallback),
      };
    }
    const app = document.getElementById("app");
    const content = app && app.firstElementChild;
    const rootStyle = getComputedStyle(root);
    const bodyStyle = document.body ? getComputedStyle(document.body) : null;
    const contentStyle = content ? getComputedStyle(content) : null;
    const themeMeta = document.querySelector('meta[name="theme-color"]');

    const caption = firstColor([
      rootStyle.getPropertyValue("--window-caption"),
      contentStyle && contentStyle.getPropertyValue("--window-caption"),
      themeMeta && themeMeta.getAttribute("content"),
      contentStyle && contentStyle.backgroundColor,
      bodyStyle && bodyStyle.backgroundColor,
      rootStyle.backgroundColor,
    ]) || fallback;

    const border = firstColor([
      rootStyle.getPropertyValue("--window-border"),
      contentStyle && contentStyle.getPropertyValue("--window-border"),
      themeMeta && themeMeta.getAttribute("content"),
      contentStyle && contentStyle.backgroundColor,
      bodyStyle && bodyStyle.backgroundColor,
    ]) || caption;

    const text = firstColor([
      rootStyle.getPropertyValue("--window-text"),
      contentStyle && contentStyle.getPropertyValue("--window-text"),
    ]) || pickTextColor(caption);

    return { caption, border, text };
  }

  function sendTheme() {
    scheduled = false;
    const theme = computeTheme();
    applyHostTheme(theme);
    const payload = JSON.stringify(theme);
    if (payload === lastSent) {
      return;
    }
    lastSent = payload;
    window[bridgeName](theme).catch(() => {});
  }

  function scheduleThemeSync() {
    if (scheduled) {
      return;
    }
    scheduled = true;
    requestAnimationFrame(sendTheme);
  }

  function dispatchFullscreenEvent(type, target) {
    const event = new Event(type, { bubbles: false, cancelable: false });
    (target || document).dispatchEvent(event);
  }

  if (typeof window.__codexSetNativeFullscreen === "function" && typeof Element !== "undefined") {
    const requestHostFullscreen = (element) => {
      return window.__codexSetNativeFullscreen(true).then(() => {
        fullscreenElement = element || document.documentElement;
        nativeFullscreen = true;
        dispatchFullscreenEvent("fullscreenchange", document);
      }).catch((error) => {
        dispatchFullscreenEvent("fullscreenerror", element || document);
        throw error;
      });
    };

    const exitHostFullscreen = () => {
      return window.__codexSetNativeFullscreen(false).then(() => {
        fullscreenElement = null;
        nativeFullscreen = false;
        dispatchFullscreenEvent("fullscreenchange", document);
      }).catch((error) => {
        dispatchFullscreenEvent("fullscreenerror", document);
        throw error;
      });
    };

    Object.defineProperty(document, "fullscreenEnabled", {
      configurable: true,
      get() {
        return true;
      },
    });

    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      get() {
        return fullscreenElement;
      },
    });

    Object.defineProperty(document, "webkitFullscreenElement", {
      configurable: true,
      get() {
        return fullscreenElement;
      },
    });

    Element.prototype.requestFullscreen = function () {
      return requestHostFullscreen(this);
    };

    document.exitFullscreen = function () {
      if (!nativeFullscreen) {
        return Promise.resolve();
      }
      return exitHostFullscreen();
    };
  }

  document.addEventListener("DOMContentLoaded", scheduleThemeSync);
  window.addEventListener("load", () => {
    scheduleThemeSync();
    setTimeout(scheduleThemeSync, 80);
    setTimeout(scheduleThemeSync, 250);
    setTimeout(scheduleThemeSync, 800);
  });

  function attachObserver() {
    const root = document.documentElement;
    if (!root) {
      return false;
    }
    const observer = new MutationObserver(scheduleThemeSync);
    observer.observe(root, {
      attributes: true,
      childList: true,
      subtree: true,
      attributeFilter: ["style", "class", "content"],
    });
    return true;
  }

  scheduleThemeSync();
  if (!attachObserver()) {
    document.addEventListener("DOMContentLoaded", attachObserver, { once: true });
  }
})();`
