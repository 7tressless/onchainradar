"use client";

import { useState } from "react";
import { safeHref } from "./ExtLink";

// Token icon from the API's logo_url. Falls back to a mono initial chip when the
// logo is missing, an unsafe URL, or fails to load.
export function TokenLogo({
  url,
  symbol,
  size = 20,
}: {
  url?: string | null;
  symbol?: string;
  size?: number;
}) {
  const [broken, setBroken] = useState(false);
  const safe = safeHref(url); // only http(s); blocks a data:/blob: src from API data
  const chip = (
    <span
      className="inline-flex items-center justify-center rounded-full border border-line bg-surface text-[9px] font-[700] text-muted"
      style={{ width: size, height: size }}
      aria-hidden
    >
      {(symbol ?? "?").slice(0, 1)}
    </span>
  );

  if (!safe || broken) return chip;

  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={safe}
      alt={symbol ?? ""}
      width={size}
      height={size}
      loading="lazy"
      decoding="async"
      referrerPolicy="no-referrer"
      onError={() => setBroken(true)}
      className="rounded-full border border-line"
      style={{ width: size, height: size }}
    />
  );
}
