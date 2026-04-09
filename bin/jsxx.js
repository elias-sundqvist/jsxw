#!/usr/bin/env node

const { spawn } = require("child_process");
const fs = require("fs");
const path = require("path");

const exePath = path.resolve(__dirname, "..", "jsx-window.exe");
const registerScriptPath = path.resolve(
  __dirname,
  "..",
  "scripts",
  "register-jsx-window.ps1"
);
const args = process.argv.slice(2);

if (!fs.existsSync(exePath)) {
  console.error(`Missing executable: ${exePath}`);
  process.exit(1);
}

if (args.includes("--help") || args.includes("-h")) {
  console.log(`jsxx <entry-file>
jsxx --eval "<h1>Hello</h1>"
jsxx --eval "export default function App() { return <div>Hello</div> }"
jsxx --eval "export default function App() { return <div>Hello</div> }" --loader tsx
jsxx --serve .\\app.jsx
jsxx --serve --port 3000 .\\app.jsx
echo "<h1>Hello</h1>" | jsxx --loader jsx
echo "export default function App() { return <div>Hello</div> }" | jsxx
jsxx --register
jsxx --register --set-default-association
jsxx --version`);
  process.exit(0);
}

if (args.includes("--version") || args.includes("-v")) {
  const packageJson = require(path.resolve(__dirname, "..", "package.json"));
  console.log(packageJson.version);
  process.exit(0);
}

if (args.includes("--register")) {
  if (!fs.existsSync(registerScriptPath)) {
    console.error(`Missing registration script: ${registerScriptPath}`);
    process.exit(1);
  }

  const registerArgs = [
    "-ExecutionPolicy",
    "Bypass",
    "-File",
    registerScriptPath,
  ];

  if (args.includes("--set-default-association")) {
    registerArgs.push("-SetDefaultAssociation");
  }

  const registerProcess = spawn("powershell", registerArgs, {
    stdio: "inherit",
    windowsHide: true,
  });

  registerProcess.on("exit", (code) => {
    process.exit(code ?? 0);
  });

  registerProcess.on("error", (error) => {
    console.error(error.message);
    process.exit(1);
  });

  return;
}

if (args.length === 0) {
  if (process.stdin.isTTY) {
    console.error("Usage: jsxx <entry-file>");
    process.exit(1);
  }
}

main().catch((error) => {
  console.error(error.message);
  process.exit(1);
});

async function main() {
  const stdinData = await maybeReadPipedInput(args);
  const childArgs = stdinData === null ? args : [...args, "--stdin"];
  const child = spawn(exePath, childArgs, {
    detached: true,
    stdio: stdinData === null ? "ignore" : ["pipe", "ignore", "ignore"],
  });

  child.on("error", (error) => {
    console.error(error.message);
    process.exit(1);
  });

  if (stdinData !== null) {
    child.stdin.on("error", () => {});
    child.stdin.end(stdinData, () => {
      child.unref();
    });
    return;
  }

  child.unref();
}

async function maybeReadPipedInput(argv) {
  if (process.stdin.isTTY) {
    return null;
  }

  let entryArgCount = 0;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--eval" || arg === "-e" || arg === "--register") {
      return null;
    }
    if (arg === "--loader") {
      i++;
      continue;
    }
    if (arg === "--version" || arg === "-v" || arg === "--help" || arg === "-h") {
      return null;
    }
    if (!arg.startsWith("-")) {
      entryArgCount++;
    }
  }

  if (entryArgCount > 0) {
    return null;
  }

  return await readStdin();
}

function readStdin() {
  return new Promise((resolve, reject) => {
    const chunks = [];
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => chunks.push(chunk));
    process.stdin.on("end", () => resolve(chunks.join("")));
    process.stdin.on("error", reject);
  });
}
