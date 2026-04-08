import css from "./style.css";
import { createRoot } from "react-dom/client";

const s = document.createElement("style");
s.textContent = css;
document.head.appendChild(s);

createRoot(document.getElementById("app")).render(
  <h1>Hello from React + esbuild!</h1>
);
