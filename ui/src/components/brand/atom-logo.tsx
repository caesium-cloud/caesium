import { useId } from "react";
import { cn } from "@/lib/utils";
import { useReducedMotion } from "@/hooks/useReducedMotion";

interface AtomLogoProps {
  /** Pixel size of the SVG square. Defaults to `40`. */
  size?: number;
  /**
   * Whether to animate the orbits + nucleus. Defaults to `true`.
   * Always renders static when the user prefers reduced motion.
   */
  animated?: boolean;
  className?: string;
  /** Test hook only. Forces the reduced-motion branch. */
  forceReducedMotion?: boolean;
}

/**
 * The Caesium atom: a stable nucleus with three deterministic orbits and
 * three gold satellites at fixed positions. The animated variant spins each
 * orbit at a different period (one reversed) and pulses the nucleus.
 */
export function AtomLogo({
  size = 40,
  animated = true,
  className,
  forceReducedMotion = false,
}: AtomLogoProps) {
  const reducedMotion = useReducedMotion();
  const motionOff = forceReducedMotion || reducedMotion || !animated;

  // Stable per-instance gradient id — keeps multiple <AtomLogo>s from sharing.
  const gradId = `atom-nuc-glow-${useId().replace(/[^a-zA-Z0-9_-]/g, "")}`;

  const orbitClass = motionOff ? "" : "atom-orbit";
  const nucleusClass = motionOff ? "" : "atom-nucleus";

  return (
    <svg
      viewBox="0 0 512 512"
      width={size}
      height={size}
      className={cn("block", className)}
      role="img"
      aria-label="Caesium"
      data-reduced-motion={motionOff ? "true" : "false"}
    >
      <defs>
        <radialGradient id={gradId} cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="hsl(var(--cyan-glow))" stopOpacity="0.55" />
          <stop offset="100%" stopColor="hsl(var(--cyan-glow))" stopOpacity="0" />
        </radialGradient>
      </defs>
      <circle cx="256" cy="256" r="76" fill={`url(#${gradId})`} />
      <g
        className={orbitClass}
        style={
          motionOff
            ? undefined
            : {
                transformOrigin: "256px 256px",
                animation: "orbit-spin 22s linear infinite",
              }
        }
      >
        <ellipse
          cx="256"
          cy="256"
          rx="210"
          ry="70"
          stroke="hsl(var(--cyan))"
          strokeWidth="3.5"
          opacity="0.9"
          fill="none"
          transform="rotate(-60 256 256)"
        />
      </g>
      <g
        className={orbitClass}
        style={
          motionOff
            ? undefined
            : {
                transformOrigin: "256px 256px",
                animation: "orbit-spin 30s linear infinite reverse",
              }
        }
      >
        <ellipse
          cx="256"
          cy="256"
          rx="210"
          ry="70"
          stroke="hsl(var(--cyan))"
          strokeWidth="3.5"
          opacity="0.85"
          fill="none"
        />
      </g>
      <g
        className={orbitClass}
        style={
          motionOff
            ? undefined
            : {
                transformOrigin: "256px 256px",
                animation: "orbit-spin 38s linear infinite",
              }
        }
      >
        <ellipse
          cx="256"
          cy="256"
          rx="210"
          ry="70"
          stroke="hsl(var(--cyan))"
          strokeWidth="3.5"
          opacity="0.9"
          fill="none"
          transform="rotate(60 256 256)"
        />
      </g>
      <circle
        cx="256"
        cy="256"
        r="20"
        fill="hsl(var(--cyan))"
        className={nucleusClass}
        style={
          motionOff
            ? undefined
            : {
                transformOrigin: "256px 256px",
                animation: "nucleus-pulse 2.4s ease-in-out infinite",
              }
        }
      />
      <circle cx="361" cy="74" r="10" fill="hsl(var(--gold))" />
      <circle cx="46" cy="256" r="10" fill="hsl(var(--gold))" />
      <circle cx="361" cy="438" r="10" fill="hsl(var(--gold))" />
    </svg>
  );
}
