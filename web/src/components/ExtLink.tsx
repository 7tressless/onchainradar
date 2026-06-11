import type { ReactNode } from "react";

// Allow only http(s) hrefs, so a URL coming from the API can't become a
// `javascript:` or `data:` link.
export function safeHref(url: string | null | undefined): string | null {
  if (!url) return null;
  return /^https?:\/\//i.test(url) ? url : null;
}

// Shared external-link primitive: validated href + target=_blank + noopener enforced.
// Falls back to a plain styled <span> when the href is missing/unsafe.
export function ExtLink({
  href,
  className,
  title,
  children,
}: {
  href: string | null | undefined;
  className?: string;
  title?: string;
  children: ReactNode;
}) {
  const safe = safeHref(href);
  if (!safe) return <span className={className}>{children}</span>;
  return (
    <a
      data-cursor
      href={safe}
      target="_blank"
      rel="noopener noreferrer"
      className={className}
      title={title}
    >
      {children}
    </a>
  );
}
