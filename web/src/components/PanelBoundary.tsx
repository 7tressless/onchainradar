"use client";

import { Component, type ReactNode } from "react";
import { ErrorState } from "./states";

// Per-panel error boundary: an unexpected data shape crashes only its own panel; the
// others keep running. Clears the error when `resetKey` changes (a fresh data tick),
// and offers a manual retry.
export class PanelBoundary extends Component<
  { children: ReactNode; resetKey?: unknown },
  { error: Error | null }
> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error) {
    console.error("panel render crashed:", error);
  }

  componentDidUpdate(prev: { resetKey?: unknown }) {
    // Fresh data arrived → drop the error and try rendering again.
    if (this.state.error && prev.resetKey !== this.props.resetKey) {
      this.setState({ error: null });
    }
  }

  render() {
    if (this.state.error) {
      return (
        <div className="p-3">
          <ErrorState message="panel error, recovering" onRetry={() => this.setState({ error: null })} />
        </div>
      );
    }
    return this.props.children;
  }
}
