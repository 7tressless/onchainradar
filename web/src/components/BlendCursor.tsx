"use client";

import { useEffect, useRef } from "react";

// White dot (mix-blend-mode:difference) that inverts what's under it and grows over
// [data-cursor] targets. Eased toward the pointer; disabled on touch / reduced-motion.
export function BlendCursor() {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    let raf = 0;
    let tx = window.innerWidth / 2;
    let ty = window.innerHeight / 2;
    let x = tx;
    let y = ty;

    let running = false;
    const start = () => {
      if (running) return;
      running = true;
      raf = requestAnimationFrame(loop);
    };
    const onMove = (e: MouseEvent) => {
      tx = e.clientX;
      ty = e.clientY;
      const t = e.target as HTMLElement | null;
      el.dataset.big = t && t.closest("[data-cursor]") ? "1" : "0";
      start(); // wake the loop only when the pointer actually moves
    };
    const loop = () => {
      x += (tx - x) * 0.2;
      y += (ty - y) * 0.2;
      el.style.transform = `translate(${x}px, ${y}px) translate(-50%, -50%)`;
      // Once it catches up to the pointer, stop the loop; no rAF churn while idle.
      if (Math.abs(tx - x) + Math.abs(ty - y) < 0.1) {
        running = false;
        return;
      }
      raf = requestAnimationFrame(loop);
    };

    window.addEventListener("mousemove", onMove);
    start();
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("mousemove", onMove);
    };
  }, []);

  return <div ref={ref} className="blend-cursor" data-big="0" aria-hidden />;
}
