"use client";

import { useEffect } from "react";
import { BTN } from "@/components/actions";

// Route-level error boundary. Auto-retries after a few seconds so an unattended
// display recovers without intervention.
export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error("dashboard render error:", error); // log the cause; the UI shows a generic message
    const t = setTimeout(reset, 8000);
    return () => clearTimeout(t);
  }, [reset, error]);

  return (
    // cursor:auto: body sets cursor:none for the blend cursor, which isn't mounted on
    // the error boundary, so without this the pointer would be invisible here.
    <div className="flex h-screen flex-col items-center justify-center gap-5 bg-bg px-8 text-center" style={{ cursor: "auto" }}>
      <div className="font-display text-[clamp(32px,5vw,64px)] font-[800] uppercase leading-[0.85] tracking-[-0.02em] text-ink">
        Something
        <br />
        broke
      </div>
      <p className="mono max-w-md text-[12px] leading-snug text-muted">
        An unexpected error occurred while rendering the dashboard. Retrying…
      </p>
      <button data-cursor onClick={reset} className={`${BTN} px-4 py-2`}>
        retry
      </button>
    </div>
  );
}
