// Shared action styles as plain constants (no imports, no cycles): one button, one link.

// Boxed action (verify / retry / pool swap+liquidity). Light hover (bg-surface, not
// bg-ink) so the difference-blend cursor doesn't invert a dark fill over the label.
export const BTN =
  "mono inline-flex items-center justify-center gap-1 border border-line px-2 py-[5px] text-[11px] uppercase tracking-[0.06em] text-ink transition-colors duration-150 hover:border-ink hover:bg-surface";

// External link, opens MantleScan / a DEX in a new tab (attest / tx / wallet / on-dex).
export const LINK =
  "mono text-[11px] text-accent underline decoration-dotted underline-offset-2 transition-colors duration-150 hover:text-ink";
