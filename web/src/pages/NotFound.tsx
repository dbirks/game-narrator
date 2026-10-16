import { Link } from "react-router";

export function NotFoundPage() {
  return (
    <main>
      <h1>Not found</h1>
      <p className="muted">That page does not exist yet.</p>
      <Link to="/diagnostics">Back to Diagnostics</Link>
    </main>
  );
}
