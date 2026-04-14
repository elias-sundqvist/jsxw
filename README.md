# jsxx

`jsxx` runs `.jsx`, `.tsx`, `.js`, and `.ts` files as native Windows windows.

It packages a small Go + WebView2 host that transpiles your entry file with esbuild, opens it in a desktop window, supports live reload for external files, and preserves common React state across reloads.

## Install

```powershell
npm i -g jsx-window
```

Or run it without a global install:

```powershell
bunx jsx-window .\app.jsx
```

## Requirements

- Windows 11 or Windows 10 with the Microsoft Edge WebView2 runtime installed
- Node.js 18+

## Usage

```powershell
jsxx .\app.jsx
jsxx .\app.tsx
jsxx C:\path\to\widget.ts
jsxx --eval "<h1>Hello</h1>"
jsxx --eval "export default function App() { return <div>Hello</div> }"
jsxx --eval "export default function App() { return <div>Hello</div> }" --loader jsx
jsxx --allow-remote .\app.jsx
jsxx --serve .\app.jsx
jsxx --serve --allow-remote .\app.jsx
jsxx --serve --port 3000 .\app.jsx
"<h1>Hello</h1>" | jsxx --loader jsx
"export default function App() { return <div>Hello</div> }" | jsxx
"export default function App() { return <div>Hello</div> }" | jsxx --loader jsx
bunx jsx-window .\app.jsx
bunx jsx-window --eval "<h1>Hello</h1>"
bunx jsx-window --serve .\app.jsx
```

## What The Entry File Should Look Like

The entry file should default-export a React component. `jsxx` mounts that component into the window root.

```jsx
import { useState } from "react";

export default function App() {
  const [count, setCount] = useState(0);

  return (
    <main
      style={{
        minHeight: "100vh",
        margin: 0,
        padding: 32,
        background: "#111827",
        color: "#f9fafb",
      }}
    >
      <h1>Hello from jsxx</h1>
      <p>Count: {count}</p>
      <button onClick={() => setCount((value) => value + 1)}>
        Increment
      </button>
    </main>
  );
}
```

### Expectations

- Default-export a component: `export default function App() { ... }`
- React hooks such as `useState` and `useReducer` work, and their state is preserved across hot reloads in common cases
- Relative imports work for `.js`, `.jsx`, `.ts`, `.tsx`, and `.json`
- Plain asset paths like `./src/assets/example.png` can be used from JSX in desktop-window mode; they are resolved through an internal fake origin instead of a real localhost server
- The file is transpiled with esbuild, so TypeScript syntax is fine, but full `tsc` type-checking is not run
- Inline mode is available through `--eval` / `-e`; it uses the current working directory as the base for relative imports
- For `--eval` and stdin, a bare JSX expression like `<h1>Hello</h1>` is automatically wrapped into a default component
- Piped stdin is also supported; it is treated like inline source and also resolves relative imports from the current working directory
- `--serve` starts a local HTTP server instead of opening a desktop window and prints the URL to stdout
- `--port` and `--host` can be used with `--serve`; by default it serves on `127.0.0.1` and picks an available port
- `--allow-remote` relaxes the host page CSP so remote scripts, styles, fetches, workers, WASM downloads, fonts, images, and media can load

### Current Limitations

- External `.css` imports are not loaded as real stylesheets in the current host path, so prefer inline styles, CSS-in-JS, or self-contained component styling
- The entry is treated as a React app root, so a plain script file with side effects but no exported component is not the intended shape
- String asset paths are document-relative to the project root, not module-relative to the JSX file they appear in
- Inline mode is not file-watched, since there is no source file to reload from
- Stdin mode is also not file-watched
- `--serve` hot-reloads by fetching the newest bundle into the page; it does not preserve React state the way the desktop host can
- By default, desktop windows use a restrictive CSP. If your app needs CDN imports, remote fetches, or cross-origin WASM/assets, launch it with `--allow-remote`

## Host Integration

`jsxx` exposes a few host behaviors that your app can use:

- `document.documentElement.requestFullscreen()` maps to native window fullscreen
- `meta[name="theme-color"]`, `--window-caption`, `--window-border`, and `--window-text` can influence the native title bar colors
- Manual reload and file-watcher reload fetch the latest bundled source from the host, rather than reusing stale startup code

## File Association

Register a right-click action for `.jsx` files:

```powershell
jsxx --register
```

Make `jsxx` the default handler for `.jsx` files under your user account:

```powershell
jsxx --register --set-default-association
```

## Features

- Runs `.js`, `.jsx`, `.ts`, and `.tsx` entry files
- Uses a native Windows GUI executable, so Explorer launches do not open a terminal
- Preserves common React `useState` and `useReducer` state across hot reloads
- Supports DOM-triggered fullscreen in the host window
- Dynamically colors the native title bar from page theme data

## Notes

- TypeScript support uses esbuild transpilation, not full `tsc` type-checking
- `.jsx` is also used by Adobe ExtendScript, so default file association may conflict with existing workflows
- This package is Windows-only
