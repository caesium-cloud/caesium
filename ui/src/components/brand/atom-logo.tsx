import { useId } from "react";
import { cn } from "@/lib/utils";
import { useReducedMotion } from "@/hooks/useReducedMotion";

export interface AtomVoter { id: string; leader: boolean; reachable: boolean | null }
export interface AtomLogoProps {
  voters?: AtomVoter[];
  quorum?: "ok" | "lost" | "unknown";
  size?: number;
  animated?: boolean;
  className?: string;
  forceReducedMotion?: boolean;
}
const BRAND_VOTERS: AtomVoter[] = Array.from({ length: 3 }, (_, i) => ({ id: `brand-${i}`, leader: i === 0, reachable: true }));

/** Three orbits, with real voters dealt round-robin. Missing evidence never looks healthy. */
export function AtomLogo({ size = 40, animated = true, className, forceReducedMotion = false, voters = BRAND_VOTERS, quorum = "ok" }: AtomLogoProps) {
  const reducedMotion = useReducedMotion();
  const motionOff = forceReducedMotion || reducedMotion || !animated;
  const descriptionId = useId();
  const drawn = voters.slice(0, 9);
  const orbitColor = quorum === "lost" ? "hsl(var(--danger))" : quorum === "unknown" ? "hsl(var(--text-4))" : "hsl(var(--cyan))";
  return <svg viewBox="0 0 512 512" width={size} height={size} className={cn("block cs-animated", className)}
    role="img" aria-label="Caesium" aria-describedby={descriptionId} data-quorum={quorum} data-reduced-motion={motionOff ? "true" : "false"}>
    <desc id={descriptionId}>{voters.length} voters. {quorum === "unknown" ? "Cluster health unknown." : quorum === "lost" ? "Quorum lost." : "Quorum available."}</desc>
    {[-60, 0, 60].map((angle, orbit) => {
      const period = [22, 30, 38][orbit];
      const members = drawn.filter((_, i) => i % 3 === orbit);
      return <g key={angle} transform={`rotate(${angle} 256 256)`}>
        <g className={motionOff ? undefined : "atom-orbit"} style={motionOff ? undefined : {
          transformOrigin: "256px 256px", animation: `cs-spin ${period}s linear infinite${orbit === 1 ? " reverse" : ""}`,
        }}>
          <ellipse cx="256" cy="256" rx="210" ry="70" fill="none" stroke={orbitColor} strokeWidth="3.5" strokeDasharray={quorum === "lost" ? "14 12" : undefined} />
          {members.map((voter, i) => {
            const theta = 2 * Math.PI * i / members.length;
            const x = 256 + 210 * Math.cos(theta), y = 256 + 70 * Math.sin(theta);
            const unknown = quorum === "unknown" || voter.reachable === null;
            const radius = size <= 22 ? 36 : 13;
            return <g key={voter.id} data-voter={voter.id} data-reachable={unknown ? "unknown" : String(voter.reachable)} data-leader={String(voter.leader)}>
              <circle cx={x} cy={y} r={radius} fill={!unknown && voter.reachable ? "hsl(var(--gold))" : "hsl(var(--void))"}
                stroke={unknown ? "hsl(var(--text-4))" : voter.reachable ? "hsl(var(--gold))" : "hsl(var(--danger))"} strokeWidth="4" />
              {voter.leader && !unknown && voter.reachable ? <circle cx={x} cy={y} r={radius + 7} fill="none" stroke="hsl(var(--cyan))" strokeWidth="4" /> : null}
            </g>;
          })}
        </g>
      </g>;
    })}
    <circle cx="256" cy="256" r="20" fill={quorum === "lost" ? "hsl(var(--text-4))" : "hsl(var(--cyan))"}
      className={!motionOff && quorum === "ok" ? "atom-nucleus" : undefined}
      style={!motionOff && quorum === "ok" ? { transformOrigin: "256px 256px", animation: "cs-nucleus 1s ease-out infinite" } : undefined} />
  </svg>;
}
