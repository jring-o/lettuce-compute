import React from "react";
import ReactDOM from "react-dom/client";
import { App } from "./App";
import { forwardWebviewLogs } from "./lib/webview-log";
import "./styles/globals.css";

// Errors and warnings in the web view go to the app log as well as the console.
forwardWebviewLogs();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
