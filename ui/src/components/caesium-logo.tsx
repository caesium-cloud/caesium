import { AtomLogo } from "@/components/brand/atom-logo";

interface CaesiumLogoProps {
  className?: string;
}

/**
 * @deprecated Use `AtomLogo` from `@/components/brand/atom-logo`.
 *
 * Kept as a static re-export so existing imports compile during the UI
 * refresh. Phase 1 cleanup removes this shim once every caller has migrated.
 */
export function CaesiumLogo({ className }: CaesiumLogoProps) {
  return <AtomLogo animated={false} className={className} />;
}
