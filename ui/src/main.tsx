import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
// Self-hosted variable fonts — bundled into dist by Vite, no runtime CDN.
// wght.css = normal weights only (no italic, which the UI never uses);
// unicode-range subsetting means the browser fetches just the latin file.
import "@fontsource-variable/geist/wght.css";
import "@fontsource-variable/jetbrains-mono/wght.css";
import "uplot/dist/uPlot.min.css";
import "./styles.css";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
