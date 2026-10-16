import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { AppNav } from "./components/AppNav";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { NotFoundPage } from "./pages/NotFound";

export function App() {
  return (
    <>
      <AppNav />
      <Routes>
        <Route path="/" element={<Navigate to="/diagnostics" replace />} />
        <Route path="/diagnostics" element={<DiagnosticsPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </>
  );
}

export function Root() {
  return (
    <BrowserRouter>
      <App />
    </BrowserRouter>
  );
}
